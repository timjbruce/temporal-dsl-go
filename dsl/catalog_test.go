package dsl_test

import (
	"testing"

	"dsl/dsl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogHasSixSteps(t *testing.T) {
	require.Len(t, dsl.Steps, 6)
	assert.Equal(t, "LookupAnchorAccount", dsl.Steps[0].Name)
	assert.Equal(t, "BuildPosition", dsl.Steps[5].Name)
}

// A RequiresOneOf group is one input to the caller, whichever member they pick.
func effectiveInputs(s dsl.StepDef) int {
	n := len(s.Requires)
	if len(s.RequiresOneOf) > 0 {
		n++
	}
	return n
}

func TestEveryStepDeclaresOneToThreeRequiredInputs(t *testing.T) {
	for _, s := range dsl.Steps {
		assert.GreaterOrEqual(t, effectiveInputs(s), 1, "%s requires nothing", s.Name)
		assert.LessOrEqual(t, effectiveInputs(s), 3, "%s requires more than 3 inputs", s.Name)
		assert.NotEmpty(t, s.Produces, "%s produces nothing", s.Name)
	}
}

func TestInterchangeableGroupsAreRegisteredAndDistinct(t *testing.T) {
	for _, s := range dsl.Steps {
		if len(s.RequiresOneOf) == 0 {
			continue
		}
		assert.GreaterOrEqual(t, len(s.RequiresOneOf), 2,
			"%s declares a one-of group with nothing to choose between", s.Name)

		seen := map[string]bool{}
		for _, key := range s.RequiresOneOf {
			_, ok := dsl.Fields[key]
			assert.True(t, ok, "%s offers unregistered field %q", s.Name, key)
			assert.False(t, seen[key], "%s lists %q twice", s.Name, key)
			assert.NotContains(t, s.Requires, key,
				"%s both demands and offers %q", s.Name, key)
			seen[key] = true
		}
	}
}

// The advisor path: the summaries and the position each take either identifier,
// while the customer hub takes only the customer id.
func TestWhichStepsAcceptEitherIdentifier(t *testing.T) {
	want := map[string]bool{
		"LookupAnchorAccount": false, "DiscoverAccounts": false,
		"SummarizeDeposits": true, "SummarizeCredit": true,
		"SummarizeLending": true, "BuildPosition": true,
	}

	for _, s := range dsl.Steps {
		accepts := len(s.RequiresOneOf) > 0
		assert.Equal(t, want[s.Name], accepts, "%s", s.Name)
		if accepts {
			assert.Equal(t, []string{"account_number", "customer_id"}, s.RequiresOneOf,
				"%s should offer the account number first", s.Name)
		}
	}
}

// The required set is meant to overlap rather than be one key per step.
func TestRequiredFieldsOverlapAcrossSteps(t *testing.T) {
	count := map[string]int{}
	for _, s := range dsl.Steps {
		for _, key := range append(append([]string{}, s.Requires...), s.RequiresOneOf...) {
			count[key]++
		}
	}

	assert.Equal(t, 4, count["as_of_date"], "as_of_date should be required by four steps")
	assert.Equal(t, 5, count["customer_id"],
		"the customer id satisfies discovery plus the four steps that accept either identifier")
	assert.Equal(t, 5, count["account_number"],
		"the account number satisfies the lookup plus those same four")
}

func TestOptionalInputsAreProducedBySomeStep(t *testing.T) {
	for _, s := range dsl.Steps {
		for _, key := range s.Optional {
			producer, ok := dsl.ProducerOf(key)
			assert.True(t, ok, "%s declares optional %q that nothing produces", s.Name, key)
			assert.NotEqual(t, s.Name, producer, "%s cannot enrich itself", s.Name)
		}
	}
}

func TestEveryStepWithOptionalsDeclaresAModeAndEffect(t *testing.T) {
	valid := map[string]bool{dsl.ModeScope: true, dsl.ModeConfidence: true, dsl.ModeExclusion: true}

	for _, s := range dsl.Steps {
		if len(s.Optional) == 0 {
			assert.Empty(t, s.OptionalMode, "%s has no optionals but declares a mode", s.Name)
			assert.Empty(t, s.Effect)
			continue
		}
		assert.True(t, valid[s.OptionalMode], "%s has unknown mode %q", s.Name, s.OptionalMode)
		assert.NotEmpty(t, s.Effect, "%s declares no advisory effect", s.Name)
	}
}

func TestAllThreeModesAreExercised(t *testing.T) {
	seen := map[string][]string{}
	for _, s := range dsl.Steps {
		if s.OptionalMode != "" {
			seen[s.OptionalMode] = append(seen[s.OptionalMode], s.Name)
		}
	}

	assert.Equal(t, []string{"DiscoverAccounts"}, seen[dsl.ModeExclusion])
	assert.Equal(t, []string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending"}, seen[dsl.ModeScope])
	assert.Equal(t, []string{"BuildPosition"}, seen[dsl.ModeConfidence])
}

func TestEveryReferencedFieldIsRegistered(t *testing.T) {
	for _, s := range dsl.Steps {
		keys := append(append(append([]string{}, s.Requires...), s.Optional...), s.Produces...)
		for _, key := range keys {
			_, ok := dsl.Fields[key]
			assert.True(t, ok, "step %s references unregistered field %q", s.Name, key)
		}
	}
}

func TestNoFieldIsProducedTwice(t *testing.T) {
	producer := map[string]string{}
	for _, s := range dsl.Steps {
		for _, key := range s.Produces {
			prior, dup := producer[key]
			assert.False(t, dup, "field %q produced by both %s and %s", key, prior, s.Name)
			producer[key] = s.Name
		}
	}
}

func TestEveryRegisteredFieldIsUsed(t *testing.T) {
	used := map[string]bool{}
	for _, s := range dsl.Steps {
		keys := append(append(append([]string{}, s.Requires...), s.Optional...), s.Produces...)
		for _, key := range keys {
			used[key] = true
		}
	}
	for name := range dsl.Fields {
		assert.True(t, used[name], "field %q is registered but no step uses it", name)
	}
}

func TestCanonicalOrderNeedsOnlyRoots(t *testing.T) {
	p := dsl.Resolve(dsl.CanonicalOrder())
	require.Empty(t, p.Errors)

	var got []string
	for _, rf := range p.Required {
		got = append(got, rf.Name)
	}
	assert.Equal(t, []string{"account_number", "as_of_date"}, got)
}

// Every optional input is satisfied in the canonical order, so it raises no advisories.
func TestCanonicalOrderRaisesNoAdvisories(t *testing.T) {
	p := dsl.Resolve(dsl.CanonicalOrder())
	assert.Empty(t, p.Advisories)
}

func TestTimingsAreDeclaredAndTiersMatch(t *testing.T) {
	for _, s := range dsl.Steps {
		assert.Positive(t, s.MinSeconds, "%s declares no minimum duration", s.Name)
		assert.GreaterOrEqual(t, s.MaxSeconds, s.MinSeconds, "%s has an inverted range", s.Name)
		assert.Equal(t, dsl.SpeedTier(s.MinSeconds, s.MaxSeconds), s.SpeedTier, "%s", s.Name)
	}

	lending, _ := dsl.Step("SummarizeLending")
	position, _ := dsl.Step("BuildPosition")
	assert.Equal(t, dsl.TierLong, lending.SpeedTier, "loan servicing is the slowest system")
	assert.Equal(t, dsl.TierFast, position.SpeedTier)
}

func TestOnlyResultBearingStepsContribute(t *testing.T) {
	want := map[string]bool{
		"LookupAnchorAccount": false, "DiscoverAccounts": false,
		"SummarizeDeposits": true, "SummarizeCredit": true,
		"SummarizeLending": true, "BuildPosition": true,
	}
	for _, s := range dsl.Steps {
		assert.Equal(t, want[s.Name], s.Contributes, "%s", s.Name)
	}
}

func TestFieldMetadataIsComplete(t *testing.T) {
	valid := map[string]bool{
		dsl.TypeString: true, dsl.TypeInteger: true, dsl.TypeNumber: true,
		dsl.TypeEmail: true, dsl.TypeDate: true,
	}
	for name, f := range dsl.Fields {
		assert.Equal(t, name, f.Name, "field key %q disagrees with its Name", name)
		assert.NotEmpty(t, f.Label, "field %q has no label", name)
		assert.NotEmpty(t, f.Example, "field %q has no example", name)
		assert.True(t, valid[f.Type], "field %q has unknown type %q", name, f.Type)
	}
}

func TestStepsNeedingInputDeclareAnEmptyEffect(t *testing.T) {
	for _, s := range dsl.Steps {
		if s.MinOptional == 0 {
			assert.Empty(t, s.EmptyEffect, "%s declares an empty effect it can never use", s.Name)
			continue
		}
		assert.LessOrEqual(t, s.MinOptional, len(s.Optional),
			"%s needs more optional inputs than it declares", s.Name)
		assert.NotEmpty(t, s.EmptyEffect, "%s can be starved but says nothing about it", s.Name)
	}
}

// Only the synthesis step can be starved: every other step gives a real answer
// from its required inputs alone.
func TestOnlyTheSynthesisStepCanBeStarved(t *testing.T) {
	for _, s := range dsl.Steps {
		want := 0
		if s.Name == "BuildPosition" {
			want = 1
		}
		assert.Equal(t, want, s.MinOptional, "%s", s.Name)
	}
}
