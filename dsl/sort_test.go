package dsl_test

import (
	"sort"
	"testing"

	"dsl/dsl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prerequisites mirrors the rule the UI's constrained sort applies: a step waits
// on any selected step that produces something it requires, that could supply
// its identifier, or — when it needs at least one optional input to say anything
// — that feeds it at all.
//
// The UI implements this in JavaScript; this guards the catalog shape the rule
// depends on. It regressed once already, when BuildPosition's identifier moved
// into RequiresOneOf and left its Requires empty, so the sort saw no
// prerequisites and scheduled it first into a starved state.
func prerequisites(steps []string) map[string][]string {
	chosen := map[string]bool{}
	for _, n := range steps {
		chosen[n] = true
	}

	out := map[string][]string{}
	for _, n := range steps {
		def, _ := dsl.Step(n)

		edges := append(append([]string{}, def.Requires...), def.RequiresOneOf...)
		if def.MinOptional > 0 {
			edges = append(edges, def.Optional...)
		}

		seen := map[string]bool{}
		for _, field := range edges {
			p, ok := dsl.ProducerOf(field)
			if ok && p != n && chosen[p] && !seen[p] {
				out[n] = append(out[n], p)
				seen[p] = true
			}
		}
	}
	return out
}

func constrainedSort(steps []string, fastestFirst bool) []string {
	needs := prerequisites(steps)
	placed, out := map[string]bool{}, []string{}

	for len(out) < len(steps) {
		ready := []string{}
		for _, n := range steps {
			if placed[n] {
				continue
			}
			ok := true
			for _, p := range needs[n] {
				if !placed[p] {
					ok = false
				}
			}
			if ok {
				ready = append(ready, n)
			}
		}
		if len(ready) == 0 {
			for _, n := range steps {
				if !placed[n] {
					out, placed[n] = append(out, n), true
				}
			}
			break
		}

		sort.SliceStable(ready, func(i, j int) bool {
			a, _ := dsl.Step(ready[i])
			b, _ := dsl.Step(ready[j])
			if fastestFirst {
				return dsl.AvgSeconds(a) < dsl.AvgSeconds(b)
			}
			return dsl.AvgSeconds(a) > dsl.AvgSeconds(b)
		})
		out, placed[ready[0]] = append(out, ready[0]), true
	}
	return out
}

func TestSpeedSortNeverProducesABlockedOrdering(t *testing.T) {
	for _, fastest := range []bool{true, false} {
		name := map[bool]string{true: "fastest first", false: "slowest first"}[fastest]

		t.Run(name, func(t *testing.T) {
			ordered := constrainedSort(dsl.CanonicalOrder(), fastest)
			require.Len(t, ordered, len(dsl.Steps))

			p := dsl.Resolve(ordered)
			require.Empty(t, p.Errors)
			assert.Empty(t, p.Blocked(),
				"%s produced an ordering the planner rejects: %v", name, ordered)

			assert.Equal(t, "LookupAnchorAccount", ordered[0],
				"every other step needs an identifier only the lookup produces")
			assert.Equal(t, "BuildPosition", ordered[len(ordered)-1],
				"the synthesis step needs upstream figures, so it trails")
		})
	}
}

// Any step that can be starved must have something upstream that feeds it, or
// the sort has no edge to order it by.
func TestEveryStarvableStepHasAnUpstreamProducer(t *testing.T) {
	for _, s := range dsl.Steps {
		if s.MinOptional == 0 {
			continue
		}
		fed := 0
		for _, field := range s.Optional {
			if p, ok := dsl.ProducerOf(field); ok && p != s.Name {
				fed++
			}
		}
		assert.GreaterOrEqual(t, fed, s.MinOptional,
			"%s needs %d optional input(s) but only %d are produced by other steps",
			s.Name, s.MinOptional, fed)
	}
}

// The regression itself: a step whose only identifier lives in RequiresOneOf
// still has a prerequisite, so it cannot be scheduled first.
func TestIdentifierOnlyStepsStillHavePrerequisites(t *testing.T) {
	needs := prerequisites(dsl.CanonicalOrder())

	for _, s := range dsl.Steps {
		if s.Name == "LookupAnchorAccount" {
			assert.Empty(t, needs[s.Name], "the lookup runs from root values alone")
			continue
		}
		assert.NotEmpty(t, needs[s.Name],
			"%s would be schedulable first, ahead of anything that feeds it", s.Name)
	}
}

// Sorting by speed must still be able to narrow scope — that trade is the point.
func TestFastestFirstStillNarrowsScope(t *testing.T) {
	ordered := constrainedSort(dsl.CanonicalOrder(), true)

	pos := map[string]int{}
	for i, n := range ordered {
		pos[n] = i
	}
	assert.Less(t, pos["SummarizeDeposits"], pos["DiscoverAccounts"],
		"fastest first should still cost the deposit summary its scope: %v", ordered)

	p := dsl.Resolve(ordered)
	assert.NotEmpty(t, p.Advisories, "that narrowing should be reported as an advisory")
}
