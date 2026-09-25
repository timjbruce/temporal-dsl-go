package workflow

import (
	"context"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"dsl/dsl"

	"go.temporal.io/sdk/temporal"
)

const (
	ScopePrimary    = "primary-only"
	ScopeDiscovered = "all-discovered"
)

// Activities simulate core banking lookups. Nothing here touches the network:
// values are generated from a seed derived from the identifier, so a given
// customer always yields the same numbers across separate runs and across retry
// attempts. That says nothing about replay — replay reads recorded activity
// results from Event History and does not re-execute activities at all.
//
// SpeedFactor scales the simulated durations. 1.0 is real time (loan servicing
// really does take 20-35s); tests and demos dial it down.
type Activities struct {
	SpeedFactor float64
}

// ValidateRequest rejects a request the Workflow cannot sensibly execute, before
// any step is scheduled, and returns everything the Workflow needs from the
// catalog. Running it as an activity puts the verdict and that catalog snapshot
// into Event History, so a replay after the catalog changes still sees the
// catalog the run was validated against.
//
// Data keys are visited in sorted order, so two equally-bad inputs always report
// the same error.
func (a *Activities) ValidateRequest(ctx context.Context, req Request) (ValidatedRequest, error) {
	if len(req.Steps) == 0 {
		return ValidatedRequest{}, requestError("no steps provided")
	}

	defs := make([]dsl.StepDef, 0, len(req.Steps))
	seen := make(map[string]bool, len(req.Steps))
	for i, name := range req.Steps {
		def, ok := dsl.Step(name)
		if !ok {
			return ValidatedRequest{}, requestError("unknown step %q at position %d", name, i+1)
		}
		if seen[name] {
			return ValidatedRequest{}, requestError("duplicate step %q at position %d", name, i+1)
		}
		seen[name] = true
		defs = append(defs, def)
	}

	for _, key := range slices.Sorted(maps.Keys(req.Data)) {
		if reason := dsl.CheckValue(key, req.Data[key]); reason != "" {
			return ValidatedRequest{}, requestError("input %q is unusable: %s", key, reason)
		}
	}

	// The running confidence is counted off BuildPosition's inputs whether or
	// not BuildPosition is in the ordering, so they travel separately.
	build, _ := dsl.Step("BuildPosition")
	return ValidatedRequest{Steps: defs, ConfidenceInputs: build.Optional}, nil
}

func (a *Activities) LookupAnchorAccount(ctx context.Context, in map[string]string) (map[string]string, error) {
	number, err := str(in, "account_number")
	if err != nil {
		return nil, err
	}
	if _, err := str(in, "as_of_date"); err != nil {
		return nil, err
	}
	if err := a.simulate(ctx, "LookupAnchorAccount", number); err != nil {
		return nil, err
	}

	r := seeded("LookupAnchorAccount", number)
	types := []string{"savings", "checking"}

	return map[string]string{
		"customer_id":    customerFromAccount(number),
		"anchor_type":    types[r.IntN(len(types))],
		"anchor_balance": money(500 + r.Float64()*48000),
	}, nil
}

// DiscoverAccounts is the exclusion mode: knowing the anchor's type lets it drop
// the account the caller already retrieved.
func (a *Activities) DiscoverAccounts(ctx context.Context, in map[string]string) (map[string]string, error) {
	customer, err := str(in, "customer_id")
	if err != nil {
		return nil, err
	}
	if err := a.simulate(ctx, "DiscoverAccounts", customer); err != nil {
		return nil, err
	}

	r := seeded("DiscoverAccounts", customer)
	all := []struct{ kind, ref string }{
		{"savings", fmt.Sprintf("SV-%04d", r.IntN(9999))},
		{"checking", fmt.Sprintf("CK-%04d", r.IntN(9999))},
		{"checking", fmt.Sprintf("CK-%04d", r.IntN(9999))},
		{"credit_card", fmt.Sprintf("CC-%04d", r.IntN(9999))},
		{"credit_card", fmt.Sprintf("CC-%04d", r.IntN(9999))},
		{"mortgage", fmt.Sprintf("ML-%04d", r.IntN(9999))},
		{"auto_loan", fmt.Sprintf("AL-%04d", r.IntN(9999))},
	}

	anchor := strings.TrimSpace(in["anchor_type"])
	scope := "every account"
	if anchor != "" {
		scope = "excluding the anchor " + anchor
	}

	refs := []string{}
	dropped := false
	for _, acct := range all {
		if !dropped && anchor != "" && acct.kind == anchor {
			dropped = true
			continue
		}
		refs = append(refs, acct.ref)
	}

	return map[string]string{
		"account_list":    strings.Join(refs, ","),
		"account_count":   strconv.Itoa(len(refs)),
		"discovery_scope": scope,
	}, nil
}

func (a *Activities) SummarizeDeposits(ctx context.Context, in map[string]string) (map[string]string, error) {
	sum, err := a.summarize(ctx, "SummarizeDeposits", in, []string{"SV", "CK"}, 400, 24000)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"deposit_total": money(sum.total),
		"deposit_count": strconv.Itoa(sum.count),
		"deposit_scope": sum.scope,
	}, nil
}

func (a *Activities) SummarizeCredit(ctx context.Context, in map[string]string) (map[string]string, error) {
	sum, err := a.summarize(ctx, "SummarizeCredit", in, []string{"CC"}, 200, 9000)
	if err != nil {
		return nil, err
	}

	limit := sum.total*1.6 + 2500
	utilization := 0.0
	if limit > 0 {
		utilization = sum.total / limit * 100
	}

	return map[string]string{
		"card_balance":       money(sum.total),
		"credit_utilization": strconv.FormatFloat(math.Round(utilization*10)/10, 'f', 1, 64),
		"credit_scope":       sum.scope,
	}, nil
}

func (a *Activities) SummarizeLending(ctx context.Context, in map[string]string) (map[string]string, error) {
	sum, err := a.summarize(ctx, "SummarizeLending", in, []string{"ML", "AL"}, 8000, 420000)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"loan_balance":  money(sum.total),
		"loan_count":    strconv.Itoa(sum.count),
		"lending_scope": sum.scope,
	}, nil
}

// BuildPosition is the confidence mode: it folds in whatever landed and reports
// how much of the picture it actually had.
func (a *Activities) BuildPosition(ctx context.Context, in map[string]string) (map[string]string, error) {
	customer, err := customerFor(in)
	if err != nil {
		return nil, err
	}
	if err := a.simulate(ctx, "BuildPosition", customer); err != nil {
		return nil, err
	}

	present := 0
	value := func(key string) (float64, error) {
		raw := strings.TrimSpace(in[key])
		if raw == "" {
			return 0, nil
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, inputError("input %q is not a number: %q", key, raw)
		}
		present++
		return v, nil
	}

	deposits, err := value("deposit_total")
	if err != nil {
		return nil, err
	}
	anchor, err := value("anchor_balance")
	if err != nil {
		return nil, err
	}
	deposits += anchor
	cards, err := value("card_balance")
	if err != nil {
		return nil, err
	}
	loans, err := value("loan_balance")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in["account_count"]) != "" {
		present++
	}

	// With nothing to fold, a "position" of zero would be a fabricated statement
	// about a real customer. Fail instead.
	if def, ok := dsl.Step("BuildPosition"); ok && present < def.MinOptional {
		return nil, inputError(
			"no position to build for %s: none of deposit_total, card_balance, loan_balance, anchor_balance or account_count were supplied",
			customer)
	}

	liabilities := cards + loans
	coverage := 0.0
	if liabilities > 0 {
		coverage = deposits / liabilities
	}

	return map[string]string{
		"net_position":        money(deposits - liabilities),
		"coverage_ratio":      strconv.FormatFloat(math.Round(coverage*1000)/1000, 'f', 3, 64),
		"position_confidence": strconv.Itoa(present * confidenceStep(confidenceInputs())),
	}, nil
}

type summary struct {
	total float64
	count int
	scope string
}

// summarize is the scope mode every product summary shares. Without account_list
// it can only see the customer's primary account; with it, it covers every
// discovered account of the kinds it handles.
func (a *Activities) summarize(ctx context.Context, step string, in map[string]string, prefixes []string, low, high float64) (summary, error) {
	customer, err := customerFor(in)
	if err != nil {
		return summary{}, err
	}
	if _, err := str(in, "as_of_date"); err != nil {
		return summary{}, err
	}
	if err := a.simulate(ctx, step, customer); err != nil {
		return summary{}, err
	}

	balance := func(key string) float64 {
		return low + seeded(step, key).Float64()*(high-low)
	}

	if strings.TrimSpace(in["account_list"]) == "" {
		return summary{total: balance("primary:" + customer), count: 1, scope: ScopePrimary}, nil
	}

	refs := matching(in["account_list"], prefixes)
	total := 0.0
	for _, ref := range refs {
		total += balance(ref)
	}
	return summary{total: total, count: len(refs), scope: ScopeDiscovered}, nil
}

func matching(list string, prefixes []string) []string {
	out := []string{}
	for _, ref := range strings.Split(list, ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(ref, prefix+"-") {
				out = append(out, ref)
				break
			}
		}
	}
	return out
}

func (a *Activities) simulate(ctx context.Context, step, seed string) error {
	def, ok := dsl.Step(step)
	if !ok {
		return fmt.Errorf("no catalog entry for step %q", step)
	}

	d := a.duration(def, seed)
	if d <= 0 {
		return nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Activities) duration(def dsl.StepDef, seed string) time.Duration {
	factor := a.SpeedFactor
	if factor <= 0 {
		factor = 1
	}

	spread := def.MaxSeconds - def.MinSeconds
	secs := float64(def.MinSeconds)
	if spread > 0 {
		secs += seeded("duration:"+def.Name, seed).Float64() * float64(spread)
	}
	return time.Duration(secs * factor * float64(time.Second))
}

func seeded(step, key string) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(step + "|" + key))
	sum := h.Sum64()
	return rand.New(rand.NewPCG(sum, sum>>32))
}

// confidenceInputs are the inputs BuildPosition counts. Deriving everything
// from them keeps the running figure and the final one on one scale by
// construction rather than by agreement.
func confidenceInputs() []string {
	def, _ := dsl.Step("BuildPosition")
	return def.Optional
}

// confidenceStep is what one confidence input is worth.
func confidenceStep(inputs []string) int {
	if len(inputs) == 0 {
		return 0
	}
	return 100 / len(inputs)
}

// ConfidenceFor counts the inputs BuildPosition would have, given a data bag.
func ConfidenceFor(bag map[string]string) int {
	return confidenceOf(confidenceInputs(), bag)
}

// confidenceOf is ConfidenceFor against a given set of inputs. The Workflow
// passes the set ValidateRequest recorded rather than reading the catalog.
func confidenceOf(inputs []string, bag map[string]string) int {
	n := 0
	for _, key := range inputs {
		if strings.TrimSpace(bag[key]) != "" {
			n++
		}
	}
	return n * confidenceStep(inputs)
}

// customerFromAccount is the one derivation of a customer from an account
// number, shared by the lookup and by every product system that resolves an
// owner itself. Because they agree, supplying an account number instead of a
// customer id yields exactly the same figures.
func customerFromAccount(number string) string {
	return fmt.Sprintf("CUST-%05d", 10000+seeded("customer", number).IntN(89999))
}

// customerFor accepts either interchangeable identifier.
func customerFor(in map[string]string) (string, error) {
	if v := strings.TrimSpace(in["customer_id"]); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(in["account_number"]); v != "" {
		return customerFromAccount(v), nil
	}
	return "", inputError(`missing input: one of "customer_id" or "account_number"`)
}

// inputError marks a bad or absent input non-retryable: no number of attempts
// will conjure a value the caller never sent.
func inputError(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(
		fmt.Sprintf(format, args...), "InvalidInput", nil)
}

const requestErrorType = "InvalidRequest"

// requestError marks a malformed request non-retryable: the same request will
// fail the same way on every attempt.
func requestError(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(
		fmt.Sprintf(format, args...), requestErrorType, nil)
}

func str(in map[string]string, key string) (string, error) {
	v := strings.TrimSpace(in[key])
	if v == "" {
		return "", inputError("missing input %q", key)
	}
	return v, nil
}

func money(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', 2, 64)
}
