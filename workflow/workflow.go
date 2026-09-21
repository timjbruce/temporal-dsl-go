package workflow

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"dsl/dsl"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// PositionWorkflow executes whatever ordering it is handed. Activities are
// dispatched by string name rather than by function reference, which is what
// makes the step list a DSL instead of a hardcoded pipeline.
//
// Each activity receives its required inputs plus whichever of its optional
// inputs happen to be in the bag. A missing optional is not an error: the step
// runs degraded, and the Workflow records that it did. That is why the same
// steps in a different order produce a different answer.
func PositionWorkflow(ctx workflow.Context, req Request) (Result, error) {
	log := workflow.GetLogger(ctx)

	if err := validateRequest(req); err != nil {
		return Result{Data: map[string]string{}, Trace: []StepTrace{}}, err
	}

	bag := map[string]string{}
	for k, v := range req.Data {
		bag[k] = v
	}

	progress := Progress{
		Total:      len(req.Steps),
		Advisories: []string{},
		History:    []PositionPoint{},
		Position:   PositionSnapshot{Scopes: map[string]string{}},
	}
	result := Result{Data: bag, Trace: []StepTrace{}}

	if err := workflow.SetQueryHandler(ctx, ProgressQuery, func() (Progress, error) {
		return progress, nil
	}); err != nil {
		return result, fmt.Errorf("could not register %s query: %w", ProgressQuery, err)
	}

	begun := workflow.Now(ctx)
	elapsed := func() float64 { return workflow.Now(ctx).Sub(begun).Seconds() }

	for i, name := range req.Steps {
		def, _ := dsl.Step(name) // validateRequest has already resolved every name

		inputs := make(map[string]string, len(def.Requires)+len(def.Optional)+1)

		// Any one of the interchangeable identifiers will do; pass whichever the
		// bag holds.
		if len(def.RequiresOneOf) > 0 {
			found := false
			for _, key := range def.RequiresOneOf {
				if v, ok := bag[key]; ok && v != "" {
					inputs[key] = v
					found = true
					break
				}
			}
			if !found {
				return result, fmt.Errorf("step %q needs one of %v, and neither a prior step nor the request supplied any", name, def.RequiresOneOf)
			}
		}

		for _, key := range def.Requires {
			v, ok := bag[key]
			if !ok || v == "" {
				return result, fmt.Errorf("step %q needs %q, which no prior step produced and the request did not supply", name, key)
			}
			inputs[key] = v
		}

		enriched, degraded := []string{}, []string{}
		for _, key := range def.Optional {
			if v, ok := bag[key]; ok && v != "" {
				inputs[key] = v
				enriched = append(enriched, key)
				continue
			}
			degraded = append(degraded, key)
		}

		if len(degraded) > 0 {
			progress.Advisories = append(progress.Advisories,
				fmt.Sprintf("%s %s (no prior step supplied %v)", name, def.Effect, degraded))
		}

		progress.Current = name
		log.Info("executing step", "step", name, "order", i+1, "degraded", degraded)

		started := workflow.Now(ctx)
		stepCtx := workflow.WithActivityOptions(ctx, activityOptions(def))

		var outputs map[string]string
		if err := workflow.ExecuteActivity(stepCtx, name, inputs).Get(stepCtx, &outputs); err != nil {
			progress.Current = ""
			return result, fmt.Errorf("step %q failed: %w", name, err)
		}

		for k, v := range outputs {
			bag[k] = v
		}

		result.Trace = append(result.Trace, StepTrace{
			Step:       name,
			Order:      i + 1,
			Inputs:     inputs,
			Enriched:   enriched,
			Degraded:   degraded,
			Outputs:    outputs,
			ElapsedSec: workflow.Now(ctx).Sub(started).Seconds(),
		})

		progress.Completed = i + 1
		progress.Current = ""
		progress.ElapsedSec = elapsed()

		if def.Contributes {
			applyToPosition(&progress.Position, name, outputs, bag)
			if progress.FirstResultSec == 0 {
				progress.FirstResultSec = progress.ElapsedSec
			}
		}

		progress.History = append(progress.History, PositionPoint{
			Order:       i + 1,
			AfterStep:   name,
			NetPosition: progress.Position.NetPosition,
			Confidence:  progress.Position.Confidence,
			ElapsedSec:  progress.ElapsedSec,
			Contributed: def.Contributes,
		})
	}

	progress.Done = true
	progress.ElapsedSec = elapsed()
	result.Progress = progress
	return result, nil
}

// applyToPosition folds a step's outputs into the running answer. Confidence is
// counted off the same inputs BuildPosition itself counts, so the figure served
// mid-run and the one it finally reports are on one scale.
func applyToPosition(p *PositionSnapshot, step string, out map[string]string, bag map[string]string) {
	assign := func(key string, target *float64) {
		if v, ok := parseMoney(out[key]); ok {
			*target = v
		}
	}

	assign("deposit_total", &p.DepositTotal)
	assign("card_balance", &p.CardBalance)
	assign("loan_balance", &p.LoanBalance)

	for _, key := range []string{"deposit_scope", "credit_scope", "lending_scope"} {
		if v, ok := out[key]; ok && v != "" {
			p.Scopes[step] = v
		}
	}

	if v, ok := parseMoney(out["position_confidence"]); ok {
		p.Confidence = int(v)
	} else {
		p.Confidence = ConfidenceFor(bag)
	}

	liabilities := p.CardBalance + p.LoanBalance
	p.NetPosition = math.Round((p.DepositTotal-liabilities)*100) / 100
	p.CoverageRatio = 0
	if liabilities > 0 {
		p.CoverageRatio = math.Round(p.DepositTotal/liabilities*1000) / 1000
	}
}

func parseMoney(raw string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// validateRequest rejects a request the Workflow cannot sensibly execute, before
// any activity is scheduled. The web service checks all of this too, but the
// Workflow does not depend on having been called through it.
//
// Data keys are visited in sorted order: iterating the map directly would let
// two equally-bad inputs produce different errors on different replays.
func validateRequest(req Request) error {
	if len(req.Steps) == 0 {
		return fmt.Errorf("no steps provided")
	}

	seen := make(map[string]bool, len(req.Steps))
	for i, name := range req.Steps {
		if _, ok := dsl.Step(name); !ok {
			return fmt.Errorf("unknown step %q at position %d", name, i+1)
		}
		if seen[name] {
			return fmt.Errorf("duplicate step %q at position %d", name, i+1)
		}
		seen[name] = true
	}

	for _, key := range slices.Sorted(maps.Keys(req.Data)) {
		if reason := dsl.CheckValue(key, req.Data[key]); reason != "" {
			return fmt.Errorf("input %q is unusable: %s", key, reason)
		}
	}

	return nil
}

// Each step declares how long its system takes, so the timeout tracks the
// system rather than one global guess.
func activityOptions(def dsl.StepDef) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: time.Duration(def.MaxSeconds)*time.Second + 30*time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			MaximumInterval:    time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    3,
		},
	}
}
