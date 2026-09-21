package workflow_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"dsl/dsl"
	bank "dsl/workflow"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

// Activities really do sleep, so tests run them at a tiny fraction of real time.
const testSpeed = 0.0005

func fastActivities() *bank.Activities {
	return &bank.Activities{SpeedFactor: testSpeed}
}

func runActivity(t *testing.T, fn any, in map[string]string) map[string]string {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(fn)

	val, err := env.ExecuteActivity(fn, in)
	require.NoError(t, err)

	var out map[string]string
	require.NoError(t, val.Get(&out))
	return out
}

func activityError(t *testing.T, fn any, in map[string]string) error {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(fn)

	_, err := env.ExecuteActivity(fn, in)
	require.Error(t, err)
	return err
}

func number(t *testing.T, raw string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(raw, 64)
	require.NoError(t, err, "value %q is not numeric", raw)
	return v
}

func TestLookupAnchorAccountIsDeterministic(t *testing.T) {
	a := fastActivities()
	in := map[string]string{"account_number": "SV-4417-0092", "as_of_date": "2026-09-30"}

	first := runActivity(t, a.LookupAnchorAccount, in)
	assert.Equal(t, first, runActivity(t, a.LookupAnchorAccount, in))

	assert.Regexp(t, `^CUST-\d{5}$`, first["customer_id"])
	assert.Contains(t, []string{"savings", "checking"}, first["anchor_type"],
		"the anchor is a deposit account, so its balance is unambiguously an asset")
	assert.Positive(t, number(t, first["anchor_balance"]))
}

// Exclusion mode: knowing the anchor type drops those accounts from the list.
func TestDiscoverAccountsExclusionMode(t *testing.T) {
	a := fastActivities()
	customer := map[string]string{"customer_id": "CUST-88213"}

	all := runActivity(t, a.DiscoverAccounts, customer)
	assert.Equal(t, "7", all["account_count"])
	assert.Equal(t, "every account", all["discovery_scope"])

	excluded := runActivity(t, a.DiscoverAccounts,
		map[string]string{"customer_id": "CUST-88213", "anchor_type": "checking"})

	assert.Equal(t, "6", excluded["account_count"], "exactly one checking account is dropped")
	assert.Equal(t, "excluding the anchor checking", excluded["discovery_scope"])
	assert.Equal(t, 1, strings.Count(excluded["account_list"], "CK-"),
		"the other checking account must survive")
	assert.Contains(t, excluded["account_list"], "SV-")
}

func TestDiscoverAccountsExcludesEachType(t *testing.T) {
	a := fastActivities()

	for kind, wantCount := range map[string]string{
		"savings": "6", "checking": "6",
	} {
		t.Run(kind, func(t *testing.T) {
			out := runActivity(t, a.DiscoverAccounts,
				map[string]string{"customer_id": "CUST-88213", "anchor_type": kind})
			assert.Equal(t, wantCount, out["account_count"], "one account dropped, not the whole kind")
			assert.Equal(t, "excluding the anchor "+kind, out["discovery_scope"])
		})
	}
}

// Scope mode: without account_list a summary sees one account; with it, all of them.
func TestSummariesWidenWithDiscovery(t *testing.T) {
	a := fastActivities()
	list := "SV-1001,CK-1120,CK-2240,CC-8841,CC-9001,ML-2207,AL-3310"

	cases := []struct {
		name       string
		fn         any
		totalField string
		scopeField string
		countField string
		wantCount  string
	}{
		{"deposits", a.SummarizeDeposits, "deposit_total", "deposit_scope", "deposit_count", "3"},
		{"lending", a.SummarizeLending, "loan_balance", "lending_scope", "loan_count", "2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			narrow := runActivity(t, tc.fn, map[string]string{
				"customer_id": "CUST-88213", "as_of_date": "2026-09-30",
			})
			wide := runActivity(t, tc.fn, map[string]string{
				"customer_id": "CUST-88213", "as_of_date": "2026-09-30", "account_list": list,
			})

			assert.Equal(t, bank.ScopePrimary, narrow[tc.scopeField])
			assert.Equal(t, "1", narrow[tc.countField])

			assert.Equal(t, bank.ScopeDiscovered, wide[tc.scopeField])
			assert.Equal(t, tc.wantCount, wide[tc.countField])
			assert.NotEqual(t, narrow[tc.totalField], wide[tc.totalField],
				"widening the scope must change the total")
		})
	}
}

func TestDepositSummaryCountsOnlyDepositAccounts(t *testing.T) {
	a := fastActivities()
	out := runActivity(t, a.SummarizeDeposits, map[string]string{
		"customer_id": "CUST-88213", "as_of_date": "2026-09-30",
		"account_list": "SV-1001,CK-1120,CC-8841,ML-2207",
	})

	assert.Equal(t, "2", out["deposit_count"], "cards and loans are not deposits")
}

func TestCreditSummaryReportsUtilization(t *testing.T) {
	a := fastActivities()

	narrow := runActivity(t, a.SummarizeCredit, map[string]string{
		"customer_id": "CUST-88213", "as_of_date": "2026-09-30",
	})
	wide := runActivity(t, a.SummarizeCredit, map[string]string{
		"customer_id": "CUST-88213", "as_of_date": "2026-09-30",
		"account_list": "CC-8841,CC-9001,SV-1001",
	})

	assert.Equal(t, bank.ScopePrimary, narrow["credit_scope"])
	assert.Equal(t, bank.ScopeDiscovered, wide["credit_scope"])
	assert.Greater(t, number(t, wide["card_balance"]), number(t, narrow["card_balance"]))

	for _, out := range []map[string]string{narrow, wide} {
		u := number(t, out["credit_utilization"])
		assert.Greater(t, u, 0.0)
		assert.Less(t, u, 100.0)
	}
}

func TestSummaryIgnoresEmptyAccountList(t *testing.T) {
	a := fastActivities()

	blank := runActivity(t, a.SummarizeDeposits, map[string]string{
		"customer_id": "CUST-88213", "as_of_date": "2026-09-30", "account_list": "",
	})
	assert.Equal(t, bank.ScopePrimary, blank["deposit_scope"])
}

// Confidence mode: 25 points per input that landed.
func TestBuildPositionConfidenceScalesWithInputs(t *testing.T) {
	a := fastActivities()
	base := map[string]string{"customer_id": "CUST-88213"}

	with := func(extra map[string]string) map[string]string {
		in := map[string]string{}
		for k, v := range base {
			in[k] = v
		}
		for k, v := range extra {
			in[k] = v
		}
		return runActivity(t, a.BuildPosition, in)
	}

	assert.Equal(t, "20", with(map[string]string{"deposit_total": "1000"})["position_confidence"])
	assert.Equal(t, "40", with(map[string]string{
		"deposit_total": "1000", "card_balance": "200",
	})["position_confidence"])
	assert.Equal(t, "60", with(map[string]string{
		"deposit_total": "1000", "card_balance": "200", "loan_balance": "5000",
	})["position_confidence"])
	assert.Equal(t, "80", with(map[string]string{
		"deposit_total": "1000", "card_balance": "200", "loan_balance": "5000",
		"anchor_balance": "900",
	})["position_confidence"])
	assert.Equal(t, "100", with(map[string]string{
		"deposit_total": "1000", "card_balance": "200", "loan_balance": "5000",
		"anchor_balance": "900", "account_count": "4",
	})["position_confidence"])
}

func TestBuildPositionMaths(t *testing.T) {
	a := fastActivities()
	out := runActivity(t, a.BuildPosition, map[string]string{
		"customer_id": "CUST-88213", "deposit_total": "30000",
		"card_balance": "5000", "loan_balance": "245000",
	})

	assert.Equal(t, "-220000.00", out["net_position"])
	assert.Equal(t, "0.120", out["coverage_ratio"])

	// The anchor account was already retrieved, so its balance must not go
	// missing just because discovery excluded it.
	withAnchor := runActivity(t, a.BuildPosition, map[string]string{
		"customer_id": "CUST-88213", "deposit_total": "30000", "anchor_balance": "18000",
		"card_balance": "5000", "loan_balance": "245000",
	})
	assert.Equal(t, "-202000.00", withAnchor["net_position"])
}

func TestBuildPositionHandlesNoLiabilities(t *testing.T) {
	a := fastActivities()
	out := runActivity(t, a.BuildPosition, map[string]string{
		"customer_id": "CUST-88213", "deposit_total": "30000",
	})

	assert.Equal(t, "30000.00", out["net_position"])
	assert.Equal(t, "0.000", out["coverage_ratio"], "no liabilities means no ratio, not a divide by zero")
}

func TestDifferentCustomersDifferentNumbers(t *testing.T) {
	a := fastActivities()

	one := runActivity(t, a.SummarizeLending, map[string]string{
		"customer_id": "CUST-11111", "as_of_date": "2026-09-30",
	})
	two := runActivity(t, a.SummarizeLending, map[string]string{
		"customer_id": "CUST-22222", "as_of_date": "2026-09-30",
	})

	assert.NotEqual(t, one["loan_balance"], two["loan_balance"])
}

func TestDurationsStayInsideTheDeclaredRange(t *testing.T) {
	a := &bank.Activities{}

	for _, s := range dsl.Steps {
		t.Run(s.Name, func(t *testing.T) {
			for _, key := range []string{"CUST-1", "CUST-2", "CUST-3", "SV-9", "CK-4"} {
				d := bank.ActivityDuration(a, s, key)
				assert.GreaterOrEqual(t, d, time.Duration(s.MinSeconds)*time.Second)
				assert.LessOrEqual(t, d, time.Duration(s.MaxSeconds)*time.Second)
			}
		})
	}
}

func TestLoanServicingIsTheSlowestSystem(t *testing.T) {
	a := &bank.Activities{}
	lending, _ := dsl.Step("SummarizeLending")
	deposits, _ := dsl.Step("SummarizeDeposits")
	position, _ := dsl.Step("BuildPosition")

	assert.Greater(t, bank.ActivityDuration(a, lending, "CUST-1"), bank.ActivityDuration(a, deposits, "CUST-1"))
	assert.Greater(t, bank.ActivityDuration(a, deposits, "CUST-1"), bank.ActivityDuration(a, position, "CUST-1"))
}

func TestSpeedFactorScalesDuration(t *testing.T) {
	lending, _ := dsl.Step("SummarizeLending")

	full := bank.ActivityDuration(&bank.Activities{SpeedFactor: 1}, lending, "CUST-1")
	tenth := bank.ActivityDuration(&bank.Activities{SpeedFactor: 0.1}, lending, "CUST-1")

	assert.InDelta(t, float64(full)/10, float64(tenth), float64(time.Millisecond))
	assert.Equal(t, full, bank.ActivityDuration(&bank.Activities{}, lending, "CUST-1"), "zero means real time")
}

func TestActivityActuallyWaits(t *testing.T) {
	a := &bank.Activities{SpeedFactor: 0.02} // deposits are 8-14s, so 160-280ms here

	started := time.Now()
	runActivity(t, a.SummarizeDeposits, map[string]string{
		"customer_id": "CUST-88213", "as_of_date": "2026-09-30",
	})

	assert.GreaterOrEqual(t, time.Since(started), 150*time.Millisecond)
	assert.Less(t, time.Since(started), 3*time.Second)
}

func TestActivitiesRejectBadInput(t *testing.T) {
	a := fastActivities()

	cases := []struct {
		name string
		fn   any
		in   map[string]string
		want string
	}{
		{"missing account number", a.LookupAnchorAccount, map[string]string{"as_of_date": "2026-09-30"}, `missing input "account_number"`},
		{"missing date", a.LookupAnchorAccount, map[string]string{"account_number": "SV-1"}, `missing input "as_of_date"`},
		{"missing customer", a.DiscoverAccounts, map[string]string{}, `missing input "customer_id"`},
		{"summary missing date", a.SummarizeCredit, map[string]string{"customer_id": "CUST-1"}, `missing input "as_of_date"`},
		{"position missing identifier", a.BuildPosition, map[string]string{}, `one of "customer_id" or "account_number"`},
		{"summary missing identifier", a.SummarizeDeposits, map[string]string{"as_of_date": "2026-09-30"}, `one of "customer_id" or "account_number"`},
		{"position given junk", a.BuildPosition, map[string]string{"customer_id": "CUST-1", "deposit_total": "lots"}, `is not a number`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, activityError(t, tc.fn, tc.in).Error(), tc.want)
		})
	}
}

func TestScopeLabelsAreTheDeclaredConstants(t *testing.T) {
	assert.Equal(t, "primary-only", bank.ScopePrimary)
	assert.Equal(t, "all-discovered", bank.ScopeDiscovered)
	assert.True(t, strings.Contains(bank.ScopeDiscovered, "discovered"))
}

// Supplying an account number instead of a customer id must give the same
// figures, because both resolve to the same customer.
func TestEitherIdentifierYieldsTheSameFigures(t *testing.T) {
	a := fastActivities()
	const account = "SV-4417-0092"

	anchor := runActivity(t, a.LookupAnchorAccount, map[string]string{
		"account_number": account, "as_of_date": "2026-09-30",
	})
	customer := anchor["customer_id"]
	require.NotEmpty(t, customer)

	for _, tc := range []struct {
		name string
		fn   any
	}{
		{"deposits", a.SummarizeDeposits},
		{"credit", a.SummarizeCredit},
		{"lending", a.SummarizeLending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			byCustomer := runActivity(t, tc.fn, map[string]string{
				"customer_id": customer, "as_of_date": "2026-09-30",
			})
			byAccount := runActivity(t, tc.fn, map[string]string{
				"account_number": account, "as_of_date": "2026-09-30",
			})
			assert.Equal(t, byCustomer, byAccount,
				"the account number resolves to the same customer")
		})
	}
}

func TestCustomerIDWinsWhenBothSupplied(t *testing.T) {
	a := fastActivities()

	both := runActivity(t, a.SummarizeDeposits, map[string]string{
		"customer_id": "CUST-11111", "account_number": "SV-4417-0092", "as_of_date": "2026-09-30",
	})
	just := runActivity(t, a.SummarizeDeposits, map[string]string{
		"customer_id": "CUST-11111", "as_of_date": "2026-09-30",
	})

	assert.Equal(t, just, both, "an explicit customer id takes precedence")
}

// A position folded from nothing would be a fabricated statement about a real
// customer, so the step refuses rather than reporting zero.
func TestBuildPositionRefusesWithNothingToFold(t *testing.T) {
	a := fastActivities()

	err := activityError(t, a.BuildPosition, map[string]string{"account_number": "SV-4417-0092"})
	assert.Contains(t, err.Error(), "no position to build")
	assert.Contains(t, err.Error(), "deposit_total")

	err = activityError(t, a.BuildPosition, map[string]string{"customer_id": "CUST-88213"})
	assert.Contains(t, err.Error(), "no position to build")
}

func TestBuildPositionAcceptsASingleFigure(t *testing.T) {
	a := fastActivities()

	for _, key := range []string{"deposit_total", "card_balance", "loan_balance", "anchor_balance"} {
		t.Run(key, func(t *testing.T) {
			out := runActivity(t, a.BuildPosition, map[string]string{
				"customer_id": "CUST-88213", key: "1000",
			})
			assert.Equal(t, "20", out["position_confidence"])
			assert.NotEmpty(t, out["net_position"])
		})
	}
}

// account_count alone carries no money, but it is still a real signal that
// discovery ran, so it clears the threshold.
func TestBuildPositionAcceptsAccountCountAlone(t *testing.T) {
	a := fastActivities()

	out := runActivity(t, a.BuildPosition, map[string]string{
		"customer_id": "CUST-88213", "account_count": "6",
	})
	assert.Equal(t, "20", out["position_confidence"])
	assert.Equal(t, "0.00", out["net_position"])
}

// The running figure and the final one must be on one scale, or the number
// visibly jumps when the position lands.
func TestRunningAndFinalConfidenceAgree(t *testing.T) {
	a := fastActivities()

	bag := map[string]string{
		"customer_id":   "CUST-88213",
		"deposit_total": "1000", "card_balance": "200", "loan_balance": "5000",
	}

	out := runActivity(t, a.BuildPosition, bag)
	assert.Equal(t, strconv.Itoa(bank.ConfidenceFor(bag)), out["position_confidence"])
	assert.Equal(t, "60", out["position_confidence"])
}

func TestConfidenceScaleIsDerivedFromTheCatalog(t *testing.T) {
	def, ok := dsl.Step("BuildPosition")
	require.True(t, ok)

	full := map[string]string{}
	for _, key := range def.Optional {
		full[key] = "1"
	}
	assert.Equal(t, 100, bank.ConfidenceFor(full), "every input present is full confidence")
	assert.Zero(t, bank.ConfidenceFor(map[string]string{}))

	one := map[string]string{def.Optional[0]: "1"}
	assert.Equal(t, 100/len(def.Optional), bank.ConfidenceFor(one))
}
