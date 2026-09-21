package dsl_test

import (
	"testing"

	"dsl/dsl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAcceptsCompleteData(t *testing.T) {
	p := dsl.Resolve(dsl.CanonicalOrder())
	v := dsl.Validate(p, map[string]string{
		"account_number": "SV-4417-0092",
		"as_of_date":     "2026-09-30",
	})

	assert.True(t, v.OK())
	assert.Empty(t, v.Missing)
	assert.Empty(t, v.Invalid)
	assert.Empty(t, v.Unused)
}

func TestValidateReportsMissingFields(t *testing.T) {
	p := dsl.Resolve(dsl.CanonicalOrder())
	v := dsl.Validate(p, map[string]string{"account_number": "SV-4417-0092"})

	require.Len(t, v.Missing, 1)
	assert.Equal(t, "as_of_date", v.Missing[0].Name)
	assert.Equal(t, []string{
		"LookupAnchorAccount", "SummarizeDeposits", "SummarizeCredit", "SummarizeLending",
	}, v.Missing[0].NeededBy, "one missing field blocks every step that shares it")
	assert.False(t, v.OK())
}

func TestValidateNamesEveryStepSharingAMissingField(t *testing.T) {
	p := dsl.Resolve([]string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending"})
	v := dsl.Validate(p, map[string]string{})

	require.Len(t, v.Missing, 2)
	assert.Equal(t, "account_number", v.Missing[0].Name,
		"the summaries offer either identifier, and default to the account number")
	assert.Equal(t,
		[]string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending"},
		v.Missing[0].NeededBy)
}

// An unmet optional is an advisory, never a validation failure.
func TestValidateIgnoresUnmetOptionals(t *testing.T) {
	p := dsl.Resolve([]string{"SummarizeDeposits"})
	require.NotEmpty(t, p.Advisories)

	v := dsl.Validate(p, map[string]string{
		"customer_id": "CUST-88213",
		"as_of_date":  "2026-09-30",
	})

	assert.True(t, v.OK(), "a degraded step is still runnable")
	assert.Empty(t, v.Missing)
}

func TestValidateTreatsBlankAsMissing(t *testing.T) {
	p := dsl.Resolve([]string{"DiscoverAccounts"})
	v := dsl.Validate(p, map[string]string{"customer_id": "   "})

	require.Len(t, v.Missing, 1)
	assert.Equal(t, "customer_id", v.Missing[0].Name)
}

func TestValidateReportsBadTypes(t *testing.T) {
	cases := []struct {
		name  string
		data  map[string]string
		field string
		want  string
	}{
		{
			name:  "date",
			data:  map[string]string{"account_number": "SV-1", "as_of_date": "Sept 30"},
			field: "as_of_date",
			want:  "YYYY-MM-DD",
		},
		{
			name:  "date with wrong separators",
			data:  map[string]string{"account_number": "SV-1", "as_of_date": "09/30/2026"},
			field: "as_of_date",
			want:  "YYYY-MM-DD",
		},
	}

	p := dsl.Resolve(dsl.CanonicalOrder())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := dsl.Validate(p, tc.data)
			assert.Empty(t, v.Missing)
			require.Len(t, v.Invalid, 1)
			assert.Equal(t, tc.field, v.Invalid[0].Name)
			assert.Contains(t, v.Invalid[0].Reason, tc.want)
			assert.False(t, v.OK())
		})
	}
}

func TestValidateAcceptsNumericFields(t *testing.T) {
	p := dsl.Resolve([]string{"BuildPosition"})
	v := dsl.Validate(p, map[string]string{"customer_id": "CUST-88213"})
	assert.True(t, v.OK())
}

func TestValidateFlagsUnusedKeysWithoutFailing(t *testing.T) {
	p := dsl.Resolve([]string{"DiscoverAccounts"})
	v := dsl.Validate(p, map[string]string{
		"customer_id":    "CUST-88213",
		"as_of_date":     "2026-09-30",
		"account_number": "SV-4417-0092",
	})

	assert.True(t, v.OK(), "unused keys must not fail validation")
	assert.Equal(t, []string{"account_number", "as_of_date"}, v.Unused)
}

// The advisor path: an account number satisfies the summaries in place of a
// customer id.
func TestValidateAcceptsEitherIdentifier(t *testing.T) {
	p := dsl.Resolve([]string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition"})
	require.Len(t, p.Required, 2)
	require.Len(t, p.Required[0].AnyOf, 2)

	byAccount := dsl.Validate(p, map[string]string{
		"account_number": "SV-4417-0092", "as_of_date": "2026-09-30",
	})
	assert.True(t, byAccount.OK(), "an account number should satisfy the summaries")
	assert.Empty(t, byAccount.Unused, "the identifier actually supplied is not unused")

	byCustomer := dsl.Validate(p, map[string]string{
		"customer_id": "CUST-88213", "as_of_date": "2026-09-30",
	})
	assert.True(t, byCustomer.OK(), "a customer id should satisfy them too")
	assert.Empty(t, byCustomer.Unused)

	neither := dsl.Validate(p, map[string]string{"as_of_date": "2026-09-30"})
	require.Len(t, neither.Missing, 1)
	assert.Equal(t, "account_number", neither.Missing[0].Name)
	assert.Equal(t, "Either Anchor account number and Customer identifier",
		neither.Missing[0].Describe())
}

func TestValidateChecksTheAlternativeThatWasSupplied(t *testing.T) {
	p := dsl.Resolve([]string{"SummarizeDeposits"})

	v := dsl.Validate(p, map[string]string{
		"customer_id": "CUST-88213", "as_of_date": "not-a-date",
	})
	require.Len(t, v.Invalid, 1)
	assert.Equal(t, "as_of_date", v.Invalid[0].Name)
	assert.Empty(t, v.Missing, "the customer id satisfied the identifier requirement")
}
