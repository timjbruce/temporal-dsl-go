package dsl_test

import (
	"testing"

	"dsl/dsl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func required(p dsl.Plan) []string {
	out := []string{}
	for _, rf := range p.Required {
		out = append(out, rf.Name)
	}
	return out
}

func advisorySteps(p dsl.Plan) []string {
	out := []string{}
	for _, a := range p.Advisories {
		out = append(out, a.Step)
	}
	return out
}

func TestResolveOrderings(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
		want  []string
	}{
		{
			name:  "canonical order needs only the two roots",
			steps: dsl.CanonicalOrder(),
			want:  []string{"account_number", "as_of_date"},
		},
		{
			name:  "summaries alone default to an account number",
			steps: []string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending"},
			want:  []string{"account_number", "as_of_date"},
		},
		{
			name:  "the advisor path: summaries and position, no lookup or discovery",
			steps: []string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition"},
			want:  []string{"account_number", "as_of_date"},
		},
		{
			name:  "discovery first pins the identifier to the customer id it needs",
			steps: []string{"DiscoverAccounts", "SummarizeDeposits", "BuildPosition"},
			want:  []string{"customer_id", "as_of_date"},
		},
		{
			name:  "discovery alone needs only the customer",
			steps: []string{"DiscoverAccounts"},
			want:  []string{"customer_id"},
		},
		{
			name:  "position alone needs only an identifier",
			steps: []string{"BuildPosition"},
			want:  []string{"account_number"},
		},
		{
			name:  "lookup first covers everything downstream",
			steps: []string{"LookupAnchorAccount", "SummarizeCredit", "BuildPosition"},
			want:  []string{"account_number", "as_of_date"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := dsl.Resolve(tc.steps)
			require.Empty(t, p.Errors)
			assert.Equal(t, tc.want, required(p))
		})
	}
}

// The central distinction: an unmet optional never becomes a required field.
func TestUnmetOptionalsNeverBecomeRequired(t *testing.T) {
	p := dsl.Resolve([]string{"SummarizeDeposits"})
	require.Empty(t, p.Errors)

	assert.Equal(t, []string{"account_number", "as_of_date"}, required(p))
	assert.NotContains(t, required(p), "account_list", "an optional input must not be demanded")

	require.Len(t, p.Advisories, 1)
	a := p.Advisories[0]
	assert.Equal(t, "SummarizeDeposits", a.Step)
	assert.Equal(t, dsl.ModeScope, a.Mode)
	assert.Equal(t, []string{"account_list"}, a.Missing)
	assert.Equal(t, []string{"DiscoverAccounts"}, a.Producers)
	assert.Contains(t, a.Effect, "primary deposit account only")
	assert.Contains(t, a.Effect, "run DiscoverAccounts first")
}

// Moving discovery changes nothing about the required data and everything about
// what the summaries will cover.
func TestMovingDiscoveryChangesAdvisoriesNotRequirements(t *testing.T) {
	first := dsl.Resolve([]string{
		"LookupAnchorAccount", "DiscoverAccounts",
		"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition",
	})
	last := dsl.Resolve([]string{
		"LookupAnchorAccount",
		"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition",
		"DiscoverAccounts",
	})

	require.Empty(t, first.Errors)
	require.Empty(t, last.Errors)

	assert.Equal(t, required(first), required(last), "the required data set must not move")
	assert.Equal(t, []string{"account_number", "as_of_date"}, required(first))

	assert.Empty(t, first.Advisories)
	assert.Equal(t,
		[]string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition"},
		advisorySteps(last))
}

func TestEnrichedByAndDegradedFlipWithPosition(t *testing.T) {
	enriched := dsl.Resolve([]string{"DiscoverAccounts", "SummarizeDeposits"})
	require.Len(t, enriched.Steps, 2)
	assert.Equal(t, map[string]string{"account_list": "DiscoverAccounts"}, enriched.Steps[1].EnrichedBy)
	assert.Empty(t, enriched.Steps[1].Degraded)

	degraded := dsl.Resolve([]string{"SummarizeDeposits", "DiscoverAccounts"})
	require.Len(t, degraded.Steps, 2)
	assert.Empty(t, degraded.Steps[0].EnrichedBy)
	assert.Equal(t, []string{"account_list"}, degraded.Steps[0].Degraded)
}

func TestExclusionAdvisoryOnDiscovery(t *testing.T) {
	p := dsl.Resolve([]string{"DiscoverAccounts"})
	require.Len(t, p.Advisories, 1)

	a := p.Advisories[0]
	assert.Equal(t, dsl.ModeExclusion, a.Mode)
	assert.Equal(t, []string{"anchor_type"}, a.Missing)
	assert.Equal(t, []string{"LookupAnchorAccount"}, a.Producers)
	assert.Contains(t, a.Effect, "including the one already retrieved")
}

func TestConfidenceAdvisoryNamesEveryProducer(t *testing.T) {
	p := dsl.Resolve([]string{"BuildPosition"})
	require.Len(t, p.Advisories, 1)

	a := p.Advisories[0]
	assert.Equal(t, dsl.ModeConfidence, a.Mode)
	assert.Equal(t,
		[]string{"deposit_total", "card_balance", "loan_balance", "anchor_balance", "account_count"},
		a.Missing)
	assert.Equal(t, []string{
		"SummarizeDeposits", "SummarizeCredit", "SummarizeLending",
		"LookupAnchorAccount", "DiscoverAccounts",
	}, a.Producers)
	assert.Contains(t, a.Effect, "cannot report a position at all",
		"starved of every input, the advisory says so rather than promising a weaker answer")
}

func TestPartiallyEnrichedStepReportsOnlyWhatIsMissing(t *testing.T) {
	p := dsl.Resolve([]string{"SummarizeDeposits", "BuildPosition"})

	require.Len(t, p.Advisories, 2)
	position := p.Advisories[1]
	assert.Equal(t, "BuildPosition", position.Step)
	assert.Equal(t, []string{"card_balance", "loan_balance", "anchor_balance", "account_count"},
		position.Missing)

	require.Len(t, p.Steps, 2)
	assert.Equal(t,
		map[string]string{"deposit_total": "SummarizeDeposits"}, p.Steps[1].EnrichedBy)
}

func TestResolveRecordsProvenance(t *testing.T) {
	p := dsl.Resolve([]string{"LookupAnchorAccount", "DiscoverAccounts"})
	require.Empty(t, p.Errors)
	require.Len(t, p.Steps, 2)

	assert.Equal(t, []string{"account_number", "as_of_date"}, p.Steps[0].NeedsInput)
	assert.Empty(t, p.Steps[0].SatisfiedBy)

	assert.Empty(t, p.Steps[1].NeedsInput)
	assert.Equal(t, map[string]string{"customer_id": "LookupAnchorAccount"}, p.Steps[1].SatisfiedBy)
	assert.Equal(t, map[string]string{"anchor_type": "LookupAnchorAccount"}, p.Steps[1].EnrichedBy)
}

func TestResolveDedupesSharedRequiredFields(t *testing.T) {
	p := dsl.Resolve([]string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending"})
	require.Len(t, p.Required, 2)

	assert.Equal(t, "account_number", p.Required[0].Name)
	assert.Equal(t,
		[]string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending"},
		p.Required[0].NeededBy)
	assert.Equal(t, "as_of_date", p.Required[1].Name)
}

func TestResolveErrors(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
		want  string
	}{
		{"empty ordering", nil, "no steps provided"},
		{"unknown step", []string{"SummarizeCrypto"}, `unknown step "SummarizeCrypto"`},
		{"duplicate step", []string{"DiscoverAccounts", "DiscoverAccounts"}, `duplicate step "DiscoverAccounts"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := dsl.Resolve(tc.steps)
			assert.False(t, p.OK())
			assert.Contains(t, p.Errors, tc.want)
		})
	}
}

func TestEstimates(t *testing.T) {
	p := dsl.Resolve(dsl.CanonicalOrder())
	assert.Equal(t, dsl.Estimate{MinSeconds: 60, MaxSeconds: 104}, p.Total)
	assert.Equal(t, dsl.Estimate{MinSeconds: 23, MaxSeconds: 40}, p.FirstResult,
		"lookup 3-6 plus discovery 12-20 plus deposits 8-14")

	lean := dsl.Resolve([]string{"BuildPosition"})
	assert.Equal(t, dsl.Estimate{MinSeconds: 2, MaxSeconds: 4}, lean.FirstResult)

	noResult := dsl.Resolve([]string{"LookupAnchorAccount", "DiscoverAccounts"})
	assert.False(t, noResult.FirstResult.Known(), "neither step contributes a result")
}

func TestResolveIncludesFieldMetadata(t *testing.T) {
	p := dsl.Resolve([]string{"LookupAnchorAccount"})
	require.Len(t, p.Required, 2)
	assert.Equal(t, "Anchor account number", p.Required[0].Label)
	assert.Equal(t, dsl.TypeDate, p.Required[1].Type)
	assert.Equal(t, "2026-09-30", p.Required[1].Example)
}

func TestAdvisoryNamesProducersReadably(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
		want  string
	}{
		{"one producer", []string{"SummarizeDeposits"}, "run DiscoverAccounts first"},
		{"two producers", []string{"SummarizeDeposits", "SummarizeCredit", "BuildPosition"},
			"run SummarizeLending, LookupAnchorAccount and DiscoverAccounts first"},
		{"five producers", []string{"BuildPosition"},
			"run SummarizeDeposits, SummarizeCredit, SummarizeLending, LookupAnchorAccount and DiscoverAccounts first"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := dsl.Resolve(tc.steps)
			require.NotEmpty(t, p.Advisories)

			last := p.Advisories[len(p.Advisories)-1]
			assert.Contains(t, last.Effect, tc.want)
			assert.NotContains(t, last.Effect, "and and")
		})
	}
}

// A step that accepts either identifier offers the alternatives rather than
// demanding one of them.
func TestInterchangeableIdentifiersAreOffered(t *testing.T) {
	p := dsl.Resolve([]string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition"})
	require.Empty(t, p.Errors)
	require.Len(t, p.Required, 2)

	id := p.Required[0]
	assert.Equal(t, "account_number", id.Name, "the account number is the default offered")
	require.Len(t, id.AnyOf, 2)
	assert.Equal(t, "account_number", id.AnyOf[0].Name)
	assert.Equal(t, "customer_id", id.AnyOf[1].Name)
	assert.Equal(t,
		[]string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition"},
		id.NeededBy, "one identifier covers all four steps")

	assert.Equal(t, "as_of_date", p.Required[1].Name)
	assert.Empty(t, p.Required[1].AnyOf, "the date has no alternative")
}

// An identifier a prior step produces, or one already asked for, satisfies the
// group without a second ask.
func TestInterchangeableGroupReusesWhatIsAlreadyThere(t *testing.T) {
	produced := dsl.Resolve([]string{"LookupAnchorAccount", "SummarizeDeposits"})
	assert.Equal(t, []string{"account_number", "as_of_date"}, required(produced))
	assert.Equal(t,
		map[string]string{"customer_id": "LookupAnchorAccount", "as_of_date": ""},
		trimmed(produced.Steps[1].SatisfiedBy),
		"the lookup's customer id satisfies the summary")

	asked := dsl.Resolve([]string{"DiscoverAccounts", "SummarizeDeposits"})
	assert.Equal(t, []string{"customer_id", "as_of_date"}, required(asked),
		"discovery pins the identifier, and the summary reuses it rather than asking again")
	require.Len(t, asked.Required, 2)
	assert.Equal(t, []string{"DiscoverAccounts", "SummarizeDeposits"}, asked.Required[0].NeededBy)
	assert.Empty(t, asked.Required[0].AnyOf, "a plain requirement offers no alternatives")
}

func TestDiscoveryStillDemandsACustomerID(t *testing.T) {
	p := dsl.Resolve([]string{"DiscoverAccounts"})
	require.Len(t, p.Required, 1)
	assert.Equal(t, "customer_id", p.Required[0].Name)
	assert.Empty(t, p.Required[0].AnyOf,
		"the customer hub is keyed by customer, so it takes no account number")
}

func trimmed(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	if _, ok := out["as_of_date"]; !ok {
		out["as_of_date"] = ""
	}
	return out
}

// An ordering that leaves a step with nothing to work with is flagged before it
// runs, and more starkly than a merely narrower one.
func TestEmptySeverityWhenAStepHasNothingToWorkWith(t *testing.T) {
	alone := dsl.Resolve([]string{"BuildPosition"})
	require.Len(t, alone.Advisories, 1)

	a := alone.Advisories[0]
	assert.Equal(t, dsl.SeverityEmpty, a.Severity)
	assert.Contains(t, a.Effect, "cannot report a position at all")
	assert.NotContains(t, a.Effect, "reduced confidence")
	assert.Len(t, alone.Blocked(), 1)
}

func TestDegradedSeverityWhenSomethingLands(t *testing.T) {
	fed := dsl.Resolve([]string{"SummarizeDeposits", "BuildPosition"})
	require.Len(t, fed.Advisories, 2)

	position := fed.Advisories[1]
	assert.Equal(t, "BuildPosition", position.Step)
	assert.Equal(t, dsl.SeverityDegraded, position.Severity)
	assert.Contains(t, position.Effect, "reduced confidence")
	assert.Empty(t, fed.Blocked(), "one figure is enough to report something")
}

func TestScopeAdvisoriesAreNeverBlocking(t *testing.T) {
	// A summary with no account list still gives a real primary-only answer.
	p := dsl.Resolve([]string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending"})
	require.Len(t, p.Advisories, 3)

	for _, a := range p.Advisories {
		assert.Equal(t, dsl.SeverityDegraded, a.Severity, "%s", a.Step)
	}
	assert.Empty(t, p.Blocked())
}

func TestCanonicalOrderIsNeverBlocked(t *testing.T) {
	assert.Empty(t, dsl.Resolve(dsl.CanonicalOrder()).Blocked())
}
