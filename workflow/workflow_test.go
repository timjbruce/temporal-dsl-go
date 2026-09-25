package workflow_test

import (
	"context"
	"testing"
	"time"

	"dsl/dsl"
	bank "dsl/workflow"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func newEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivity(fastActivities())
	return env
}

func rootData() map[string]string {
	return map[string]string{
		"account_number": "SV-4417-0092",
		"as_of_date":     "2026-09-30",
	}
}

func runResult(t *testing.T, env *testsuite.TestWorkflowEnvironment) bank.Result {
	t.Helper()
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result bank.Result
	require.NoError(t, env.GetWorkflowResult(&result))
	return result
}

func run(t *testing.T, steps []string, data map[string]string) bank.Result {
	t.Helper()
	env := newEnv(t)
	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{Steps: steps, Data: data})
	return runResult(t, env)
}

var discoveryLast = []string{
	"LookupAnchorAccount",
	"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition",
	"DiscoverAccounts",
}

// The headline: identical steps and identical data, reordered, produce a
// different answer.
func TestReorderingChangesTheAnswer(t *testing.T) {
	canonical := run(t, dsl.CanonicalOrder(), rootData())
	degraded := run(t, discoveryLast, rootData())

	require.Len(t, canonical.Trace, 6)
	require.Len(t, degraded.Trace, 6)

	assert.NotEqual(t, canonical.Data["deposit_total"], degraded.Data["deposit_total"],
		"discovery last must narrow the deposit summary")

	assert.Equal(t, bank.ScopeDiscovered, canonical.Data["deposit_scope"])
	assert.Equal(t, bank.ScopePrimary, degraded.Data["deposit_scope"])
	assert.Equal(t, bank.ScopeDiscovered, canonical.Data["credit_scope"])
	assert.Equal(t, bank.ScopePrimary, degraded.Data["credit_scope"])
	assert.Equal(t, bank.ScopeDiscovered, canonical.Data["lending_scope"])
	assert.Equal(t, bank.ScopePrimary, degraded.Data["lending_scope"])

	assert.Equal(t, "100", canonical.Data["position_confidence"])
	assert.Equal(t, "80", degraded.Data["position_confidence"],
		"account_count had not landed when the position was built")
}

// ...while the data the caller has to supply does not move at all.
func TestReorderingDoesNotChangeRequiredData(t *testing.T) {
	canonical := dsl.Resolve(dsl.CanonicalOrder())
	degraded := dsl.Resolve(discoveryLast)

	require.Empty(t, canonical.Errors)
	require.Empty(t, degraded.Errors)

	assert.Equal(t, canonical.Required, degraded.Required)

	// Both orderings run from the same two root values.
	for _, steps := range [][]string{dsl.CanonicalOrder(), discoveryLast} {
		result := run(t, steps, rootData())
		assert.Len(t, result.Trace, 6)
	}
}

func TestCanonicalOrderRunsNothingDegraded(t *testing.T) {
	result := run(t, dsl.CanonicalOrder(), rootData())

	assert.Empty(t, result.Progress.Advisories)
	for _, tr := range result.Trace {
		assert.Empty(t, tr.Degraded, "%s ran degraded in the canonical order", tr.Step)
	}
	assert.True(t, result.Progress.Done)
	assert.Equal(t, 6, result.Progress.Completed)
}

func TestRuntimeAdvisoriesNameWhatWasMissing(t *testing.T) {
	result := run(t, discoveryLast, rootData())

	require.Len(t, result.Progress.Advisories, 4)
	assert.Contains(t, result.Progress.Advisories[0], "SummarizeDeposits")
	assert.Contains(t, result.Progress.Advisories[0], "primary deposit account only")
	assert.Contains(t, result.Progress.Advisories[0], "account_list")
	assert.Contains(t, result.Progress.Advisories[3], "BuildPosition")
	assert.Contains(t, result.Progress.Advisories[3], "reduced confidence")
}

func TestTraceRecordsEnrichmentPerStep(t *testing.T) {
	canonical := run(t, dsl.CanonicalOrder(), rootData())

	deposits := canonical.Trace[2]
	require.Equal(t, "SummarizeDeposits", deposits.Step)
	assert.Equal(t, []string{"account_list"}, deposits.Enriched)
	assert.Empty(t, deposits.Degraded)
	assert.Contains(t, deposits.Inputs, "account_list", "the optional key must reach the activity")

	degraded := run(t, discoveryLast, rootData()).Trace[1]
	require.Equal(t, "SummarizeDeposits", degraded.Step)
	assert.Empty(t, degraded.Enriched)
	assert.Equal(t, []string{"account_list"}, degraded.Degraded)
	assert.NotContains(t, degraded.Inputs, "account_list")
}

func TestOptionalKeysAreNotRequired(t *testing.T) {
	// SummarizeDeposits alone has no account_list available and must still run.
	result := run(t, []string{"SummarizeDeposits"}, map[string]string{
		"customer_id": "CUST-88213", "as_of_date": "2026-09-30",
	})

	require.Len(t, result.Trace, 1)
	assert.Equal(t, bank.ScopePrimary, result.Data["deposit_scope"])
	assert.Equal(t, []string{"account_list"}, result.Trace[0].Degraded)
}

func TestExclusionDependsOnLookupRunningFirst(t *testing.T) {
	withLookup := run(t, []string{"LookupAnchorAccount", "DiscoverAccounts"}, rootData())
	without := run(t, []string{"DiscoverAccounts"}, map[string]string{"customer_id": "CUST-88213"})

	assert.Contains(t, withLookup.Data["discovery_scope"], "excluding")
	assert.Equal(t, "every account", without.Data["discovery_scope"])
	assert.Equal(t, "7", without.Data["account_count"])
}

func TestRunningPositionAccumulates(t *testing.T) {
	result := run(t, dsl.CanonicalOrder(), rootData())
	p := result.Progress

	require.Len(t, p.History, 6)
	assert.False(t, p.History[0].Contributed, "the lookup contributes nothing")
	assert.False(t, p.History[1].Contributed, "discovery contributes nothing")
	assert.True(t, p.History[2].Contributed)

	assert.Zero(t, p.History[1].NetPosition, "nothing has landed yet")
	assert.NotZero(t, p.History[5].NetPosition)

	assert.Positive(t, p.Position.DepositTotal)
	assert.Positive(t, p.Position.LoanBalance)
	assert.Equal(t, 100, p.Position.Confidence)
	assert.Equal(t, bank.ScopeDiscovered, p.Position.Scopes["SummarizeDeposits"])
}

func TestProgressIsQueryableWhileRunning(t *testing.T) {
	env := newEnv(t)

	env.RegisterDelayedCallback(func() {
		val, err := env.QueryWorkflow(bank.ProgressQuery)
		require.NoError(t, err)

		var p bank.Progress
		require.NoError(t, val.Get(&p))

		assert.False(t, p.Done)
		assert.Positive(t, p.Completed)
		assert.Less(t, p.Completed, p.Total)
	}, 25*time.Second)

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: dsl.CanonicalOrder(), Data: rootData(),
	})

	assert.True(t, runResult(t, env).Progress.Done)
}

// Activities are dispatched by string name, so they mock by string name too.
func TestWorkflowDispatchesActivitiesByName(t *testing.T) {
	env := newEnv(t)

	env.OnActivity("SummarizeDeposits", mock.Anything, map[string]string{
		"customer_id": "CUST-88213", "as_of_date": "2026-09-30",
	}).Return(map[string]string{
		"deposit_total": "1234.00", "deposit_count": "1", "deposit_scope": bank.ScopePrimary,
	}, nil).Once()

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: []string{"SummarizeDeposits"},
		Data:  map[string]string{"customer_id": "CUST-88213", "as_of_date": "2026-09-30"},
	})

	result := runResult(t, env)
	assert.Equal(t, "1234.00", result.Data["deposit_total"])
	assert.Equal(t, 1234.0, result.Progress.Position.DepositTotal)
	env.AssertExpectations(t)
}

func TestWorkflowPassesOnlyDeclaredKeys(t *testing.T) {
	env := newEnv(t)

	env.OnActivity("DiscoverAccounts", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			in := args.Get(1).(map[string]string)
			assert.Contains(t, in, "customer_id")
			assert.NotContains(t, in, "as_of_date", "DiscoverAccounts declares neither a need nor a use for the date")
		}).
		Return(map[string]string{
			"account_list": "CK-1", "account_count": "1", "discovery_scope": "every account",
		}, nil)

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: []string{"DiscoverAccounts"},
		Data:  map[string]string{"customer_id": "CUST-88213", "as_of_date": "2026-09-30"},
	})

	runResult(t, env)
}

func TestWorkflowFailsFastOnMissingRequiredInput(t *testing.T) {
	env := newEnv(t)

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: []string{"SummarizeDeposits"},
		Data:  map[string]string{"customer_id": "CUST-88213"},
	})

	require.Error(t, env.GetWorkflowError())
	assert.Contains(t, env.GetWorkflowError().Error(), `needs "as_of_date"`)
}

func TestWorkflowRejectsUnknownStep(t *testing.T) {
	env := newEnv(t)
	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{Steps: []string{"SummarizeCrypto"}})

	require.Error(t, env.GetWorkflowError())
	assert.Contains(t, env.GetWorkflowError().Error(), `unknown step "SummarizeCrypto"`)
}

// The worker registers the whole Activities struct, which registers every
// exported method. A helper that is not a valid activity signature breaks the
// worker at startup, so this asserts registration stays clean.
func TestActivitiesStructRegistersCleanly(t *testing.T) {
	assert.NotPanics(t, func() {
		(&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment().
			RegisterActivity(&bank.Activities{})
	})
}

func TestEveryCatalogStepHasARegisteredActivity(t *testing.T) {
	result := run(t, dsl.CanonicalOrder(), rootData())

	for i, step := range dsl.CanonicalOrder() {
		assert.Equal(t, step, result.Trace[i].Step)
		assert.NotEmpty(t, result.Trace[i].Outputs, "%s produced nothing", step)
	}
}

// The advisor path: skip the lookup and discovery entirely, hand over an account
// number, and the summaries still run.
func TestAdvisorPathRunsFromAnAccountNumber(t *testing.T) {
	steps := []string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition"}

	result := run(t, steps, map[string]string{
		"account_number": "SV-4417-0092", "as_of_date": "2026-09-30",
	})

	require.Len(t, result.Trace, 4)
	assert.NotEmpty(t, result.Data["deposit_total"])
	assert.NotEmpty(t, result.Data["net_position"])
	assert.Equal(t, bank.ScopePrimary, result.Data["deposit_scope"],
		"without discovery the summaries stay narrow")

	assert.Contains(t, result.Trace[0].Inputs, "account_number")
	assert.NotContains(t, result.Trace[0].Inputs, "customer_id")
}

// Whichever identifier the caller supplies, the figures agree, because both
// resolve to the same customer.
func TestEitherIdentifierGivesTheSamePosition(t *testing.T) {
	steps := []string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition"}

	anchor := run(t, []string{"LookupAnchorAccount"}, rootData())
	customer := anchor.Data["customer_id"]
	require.NotEmpty(t, customer)

	byAccount := run(t, steps, map[string]string{
		"account_number": "SV-4417-0092", "as_of_date": "2026-09-30",
	})
	byCustomer := run(t, steps, map[string]string{
		"customer_id": customer, "as_of_date": "2026-09-30",
	})

	for _, field := range []string{"deposit_total", "card_balance", "loan_balance", "net_position"} {
		assert.Equal(t, byAccount.Data[field], byCustomer.Data[field], "%s", field)
	}
}

func TestWorkflowPrefersAProducedIdentifier(t *testing.T) {
	env := newEnv(t)

	env.OnActivity("SummarizeDeposits", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			in := args.Get(1).(map[string]string)
			assert.Contains(t, in, "account_number",
				"the account number is offered first, so it is what the bag yields")
			assert.NotContains(t, in, "customer_id")
		}).
		Return(map[string]string{
			"deposit_total": "1.00", "deposit_count": "1", "deposit_scope": bank.ScopePrimary,
		}, nil)

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: []string{"SummarizeDeposits"},
		Data: map[string]string{
			"account_number": "SV-4417-0092", "customer_id": "CUST-1", "as_of_date": "2026-09-30",
		},
	})

	runResult(t, env)
}

func TestWorkflowFailsWhenNeitherIdentifierIsPresent(t *testing.T) {
	env := newEnv(t)

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: []string{"SummarizeDeposits"},
		Data:  map[string]string{"as_of_date": "2026-09-30"},
	})

	require.Error(t, env.GetWorkflowError())
	assert.Contains(t, env.GetWorkflowError().Error(), "needs one of")
	assert.Contains(t, env.GetWorkflowError().Error(), "account_number")
}

// The Workflow validates its own request. The web service checks all of this
// too, but a client calling the Workflow directly is not exempt.
func TestWorkflowValidatesItsOwnRequest(t *testing.T) {
	cases := []struct {
		name string
		req  bank.Request
		want string
	}{
		{
			name: "empty ordering",
			req:  bank.Request{Steps: nil, Data: rootData()},
			want: "no steps provided",
		},
		{
			name: "unknown step",
			req:  bank.Request{Steps: []string{"SummarizeCrypto"}, Data: rootData()},
			want: `unknown step "SummarizeCrypto" at position 1`,
		},
		{
			name: "duplicate step",
			req: bank.Request{
				Steps: []string{"LookupAnchorAccount", "DiscoverAccounts", "DiscoverAccounts"},
				Data:  rootData(),
			},
			want: `duplicate step "DiscoverAccounts" at position 3`,
		},
		{
			name: "malformed date",
			req: bank.Request{
				Steps: dsl.CanonicalOrder(),
				Data:  map[string]string{"account_number": "SV-4417-0092", "as_of_date": "Sept 30"},
			},
			want: `input "as_of_date" is unusable: expected a date as YYYY-MM-DD`,
		},
		{
			name: "malformed number supplied directly",
			req: bank.Request{
				Steps: []string{"BuildPosition"},
				Data:  map[string]string{"account_number": "SV-1", "deposit_total": "lots"},
			},
			want: `input "deposit_total" is unusable: expected a number`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			env.ExecuteWorkflow(bank.PositionWorkflow, tc.req)

			require.Error(t, env.GetWorkflowError())
			assert.Contains(t, env.GetWorkflowError().Error(), tc.want)
		})
	}
}

// Validation is itself an activity: its verdict is recorded in Event History, and
// the Workflow runs the step definitions it returned rather than re-reading the
// catalog. Mocking it proves the Workflow takes its answer from there.
func TestValidationRunsAsAnActivity(t *testing.T) {
	env := newEnv(t)

	def, ok := dsl.Step("SummarizeDeposits")
	require.True(t, ok)
	def.Optional = nil // a definition the live catalog does not hold

	env.OnActivity("ValidateRequest", mock.Anything, mock.Anything).
		Return(bank.ValidatedRequest{Steps: []dsl.StepDef{def}}, nil).Once()

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: []string{"SummarizeDeposits"},
		Data:  map[string]string{"customer_id": "CUST-88213", "as_of_date": "2026-09-30"},
	})

	result := runResult(t, env)
	require.Len(t, result.Trace, 1)
	assert.Empty(t, result.Trace[0].Degraded, "the recorded definition, not the catalog, decides the inputs")
	env.AssertExpectations(t)
}

// The running confidence is counted off the recorded inputs too, not whatever
// BuildPosition declares in the live catalog.
func TestRunningConfidenceUsesRecordedInputs(t *testing.T) {
	env := newEnv(t)

	def, ok := dsl.Step("SummarizeDeposits")
	require.True(t, ok)

	env.OnActivity("ValidateRequest", mock.Anything, mock.Anything).
		Return(bank.ValidatedRequest{
			Steps:            []dsl.StepDef{def},
			ConfidenceInputs: []string{"deposit_total", "never_supplied"},
		}, nil).Once()

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: []string{"SummarizeDeposits"},
		Data:  map[string]string{"customer_id": "CUST-88213", "as_of_date": "2026-09-30"},
	})

	result := runResult(t, env)
	assert.Equal(t, 50, result.Progress.Position.Confidence,
		"one of two recorded inputs present; the live catalog has five")
}

// A refused request fails the Workflow non-retryably, so a Workflow retry
// policy cannot re-run it.
func TestRejectionFailsTheWorkflowNonRetryably(t *testing.T) {
	env := newEnv(t)
	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{Steps: []string{"SummarizeCrypto"}})

	var appErr *temporal.ApplicationError
	require.ErrorAs(t, env.GetWorkflowError(), &appErr)
	assert.Equal(t, "InvalidRequest", appErr.Type())
	assert.True(t, appErr.NonRetryable())
}

func TestValidationRejectionIsNotRetried(t *testing.T) {
	env := newEnv(t)

	attempts := 0
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "ValidateRequest" {
			attempts++
		}
	})

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{Steps: []string{"SummarizeCrypto"}})

	require.Error(t, env.GetWorkflowError())
	assert.Contains(t, env.GetWorkflowError().Error(), "request rejected")
	assert.Equal(t, 1, attempts)
}

// Rejection happens before any step is scheduled, so a bad request costs only the
// validation activity.
func TestInvalidRequestSchedulesNoStep(t *testing.T) {
	env := newEnv(t)

	env.OnActivity("LookupAnchorAccount", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { t.Fatal("no activity should run for an invalid request") }).
		Return(map[string]string{}, nil).Maybe()

	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: []string{"LookupAnchorAccount", "LookupAnchorAccount"},
		Data:  rootData(),
	})

	require.Error(t, env.GetWorkflowError())
}

// Keys the ordering never consumes are a warning in the web layer, not a reason
// for the Workflow to refuse — but they are still type-checked.
func TestUnusedKeysAreToleratedButStillChecked(t *testing.T) {
	ok := run(t, []string{"DiscoverAccounts"}, map[string]string{
		"customer_id": "CUST-88213",
		"as_of_date":  "2026-09-30",
		"not_a_field": "anything at all",
	})
	assert.Len(t, ok.Trace, 1)

	env := newEnv(t)
	env.ExecuteWorkflow(bank.PositionWorkflow, bank.Request{
		Steps: []string{"DiscoverAccounts"},
		Data:  map[string]string{"customer_id": "CUST-88213", "as_of_date": "nope"},
	})
	require.Error(t, env.GetWorkflowError(), "an unused key is still a registered field")
}

// Go randomizes map iteration, so validating req.Data in map order would let two
// equally-bad inputs report different errors run to run — and differ between a
// run and its replay. Keys are visited sorted; this asserts the consequence.
func TestValidationErrorIsStableAcrossRuns(t *testing.T) {
	req := bank.Request{
		Steps: dsl.CanonicalOrder(),
		Data: map[string]string{
			"account_number": "SV-4417-0092",
			"as_of_date":     "Sept 30",
			"deposit_total":  "lots",
			"account_count":  "several",
			"anchor_balance": "a bit",
		},
	}

	seen := map[string]int{}
	for i := 0; i < 25; i++ {
		env := newEnv(t)
		env.ExecuteWorkflow(bank.PositionWorkflow, req)
		require.Error(t, env.GetWorkflowError())
		seen[env.GetWorkflowError().Error()]++
	}

	require.Len(t, seen, 1, "four bad inputs produced more than one error message: %v", seen)
	for msg := range seen {
		assert.Contains(t, msg, `input "account_count"`, "sorted order reports the first key")
	}
}
