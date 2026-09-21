package dsl

import (
	"fmt"
	"strings"
)

type PlannedStep struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	System      string            `json:"system"`
	Order       int               `json:"order"`
	SatisfiedBy map[string]string `json:"satisfied_by"`
	NeedsInput  []string          `json:"needs_input"`
	EnrichedBy  map[string]string `json:"enriched_by"`
	Degraded    []string          `json:"degraded"`
	Produces    []string          `json:"produces"`
}

type RequiredField struct {
	Field
	NeededBy []string `json:"needed_by"`

	// AnyOf, when set, means the caller supplies one of these interchangeable
	// identifiers. The embedded Field is the default offered.
	AnyOf []Field `json:"any_of,omitempty"`
}

// Advisory reports an optional input a step will not have at its current
// position. It never becomes a required field: the step still runs, it just
// does less.
const (
	SeverityDegraded = "degraded" // the step runs, with a narrower or less certain answer
	SeverityEmpty    = "empty"    // the step has nothing to work with and will fail
)

type Advisory struct {
	Step      string   `json:"step"`
	Mode      string   `json:"mode"`
	Severity  string   `json:"severity"`
	Missing   []string `json:"missing"`
	Producers []string `json:"producers"`
	Effect    string   `json:"effect"`
}

type Estimate struct {
	MinSeconds int `json:"min_seconds"`
	MaxSeconds int `json:"max_seconds"`
}

func (e Estimate) Known() bool { return e.MaxSeconds > 0 }

type Plan struct {
	Steps      []PlannedStep   `json:"steps"`
	Required   []RequiredField `json:"required"`
	Advisories []Advisory      `json:"advisories"`
	Errors     []string        `json:"errors"`

	Total       Estimate `json:"total"`
	FirstResult Estimate `json:"first_result"`
}

func (p Plan) OK() bool { return len(p.Errors) == 0 }

// Resolve walks the ordering once, tracking which fields a prior step has
// produced. A required field that is not yet available becomes a required input.
// An optional field that is not yet available becomes an advisory instead: the
// step runs either way, which is what makes reordering change the answer rather
// than the paperwork.
func Resolve(steps []string) Plan {
	p := Plan{
		Steps:      []PlannedStep{},
		Required:   []RequiredField{},
		Advisories: []Advisory{},
		Errors:     []string{},
	}

	if len(steps) == 0 {
		p.Errors = append(p.Errors, "no steps provided")
		return p
	}

	seen := make(map[string]bool, len(steps))
	available := map[string]string{}
	requiredIdx := map[string]int{}

	for i, name := range steps {
		def, ok := Step(name)
		if !ok {
			p.Errors = append(p.Errors, fmt.Sprintf("unknown step %q", name))
			continue
		}
		if seen[name] {
			p.Errors = append(p.Errors, fmt.Sprintf("duplicate step %q", name))
			continue
		}
		seen[name] = true

		ps := PlannedStep{
			Name:        def.Name,
			Description: def.Description,
			System:      def.System,
			Order:       i + 1,
			SatisfiedBy: map[string]string{},
			NeedsInput:  []string{},
			EnrichedBy:  map[string]string{},
			Degraded:    []string{},
			Produces:    def.Produces,
		}

		// The interchangeable identifier is resolved first so it leads the
		// required list, ahead of the step's plain requirements.
		if len(def.RequiresOneOf) > 0 {
			resolveOneOf(&p, &ps, def, available, requiredIdx)
		}

		for _, key := range def.Requires {
			if producer, ok := available[key]; ok {
				ps.SatisfiedBy[key] = producer
				continue
			}
			ps.NeedsInput = append(ps.NeedsInput, key)
			if idx, ok := requiredIdx[key]; ok {
				p.Required[idx].NeededBy = append(p.Required[idx].NeededBy, def.Name)
				continue
			}
			requiredIdx[key] = len(p.Required)
			p.Required = append(p.Required, RequiredField{
				Field:    Fields[key],
				NeededBy: []string{def.Name},
			})
		}

		var producers []string
		for _, key := range def.Optional {
			if producer, ok := available[key]; ok {
				ps.EnrichedBy[key] = producer
				continue
			}
			ps.Degraded = append(ps.Degraded, key)
			if producer, ok := ProducerOf(key); ok && !contains(producers, producer) {
				producers = append(producers, producer)
			}
		}

		if len(ps.Degraded) > 0 {
			severity := SeverityDegraded
			if len(ps.EnrichedBy) < def.MinOptional {
				severity = SeverityEmpty
			}
			p.Advisories = append(p.Advisories, Advisory{
				Step:      def.Name,
				Mode:      def.OptionalMode,
				Severity:  severity,
				Missing:   ps.Degraded,
				Producers: producers,
				Effect:    renderEffect(def, producers, severity),
			})
		}

		for _, key := range def.Produces {
			available[key] = def.Name
		}
		p.Steps = append(p.Steps, ps)

		p.Total.MinSeconds += def.MinSeconds
		p.Total.MaxSeconds += def.MaxSeconds
		if !p.FirstResult.Known() && def.Contributes {
			p.FirstResult = p.Total
		}
	}

	return p
}

// resolveOneOf satisfies a group of interchangeable identifiers: from a prior
// step's output if one is available, else from an identifier the caller has
// already been asked for, else by asking for one and offering the alternatives.
func resolveOneOf(p *Plan, ps *PlannedStep, def StepDef, available map[string]string, requiredIdx map[string]int) {
	for _, key := range def.RequiresOneOf {
		if producer, ok := available[key]; ok {
			ps.SatisfiedBy[key] = producer
			return
		}
	}

	for _, key := range def.RequiresOneOf {
		if idx, ok := requiredIdx[key]; ok {
			p.Required[idx].NeededBy = append(p.Required[idx].NeededBy, def.Name)
			ps.NeedsInput = append(ps.NeedsInput, key)
			return
		}
	}

	choice := def.RequiresOneOf[0]
	alternatives := make([]Field, 0, len(def.RequiresOneOf))
	for _, key := range def.RequiresOneOf {
		alternatives = append(alternatives, Fields[key])
	}

	ps.NeedsInput = append(ps.NeedsInput, choice)
	requiredIdx[choice] = len(p.Required)
	p.Required = append(p.Required, RequiredField{
		Field:    Fields[choice],
		NeededBy: []string{def.Name},
		AnyOf:    alternatives,
	})
}

func renderEffect(def StepDef, producers []string, severity string) string {
	effect := def.Effect
	if severity == SeverityEmpty && def.EmptyEffect != "" {
		effect = def.EmptyEffect
	}
	if len(producers) == 0 {
		return def.Name + " " + effect
	}
	return fmt.Sprintf("%s %s — run %s first to change that",
		def.Name, effect, joinList(producers))
}

// Blocked reports whether any step in this ordering will fail outright for want
// of inputs nothing upstream supplies.
func (p Plan) Blocked() []Advisory {
	out := []Advisory{}
	for _, a := range p.Advisories {
		if a.Severity == SeverityEmpty {
			out = append(out, a)
		}
	}
	return out
}

// joinList renders "A", "A and B", or "A, B and C" rather than "A and B and C".
func joinList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Choices lists the identifiers that would satisfy this requirement: the
// alternatives when it offers any, otherwise the field itself.
func (r RequiredField) Choices() []Field {
	if len(r.AnyOf) > 0 {
		return r.AnyOf
	}
	return []Field{r.Field}
}

// Label describes the requirement, naming the alternatives when there are any.
func (r RequiredField) Describe() string {
	if len(r.AnyOf) < 2 {
		return r.Label
	}
	names := make([]string, 0, len(r.AnyOf))
	for _, f := range r.AnyOf {
		names = append(names, f.Label)
	}
	return "Either " + joinList(names)
}
