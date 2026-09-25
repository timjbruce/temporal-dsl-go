//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dsl/dsl"
	"dsl/web"
	bank "dsl/workflow"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
)

// newStack boots a real Temporal dev server, a real worker, and the HTTP API in
// front of them, so a request exercises the whole path.
func newStack(t *testing.T) string {
	t.Helper()

	dev, err := testsuite.StartDevServer(context.Background(), testsuite.DevServerOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = dev.Stop() })

	c := dev.Client()
	w := worker.New(c, bank.TaskQueueName, worker.Options{})
	w.RegisterWorkflow(bank.PositionWorkflow)
	// Real durations total minutes; run the systems at 2% of that.
	w.RegisterActivity(&bank.Activities{SpeedFactor: 0.02})
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	api := httptest.NewServer(web.NewServer(c).Routes())
	t.Cleanup(api.Close)
	return api.URL
}

func post(t *testing.T, base, path, body string) (int, []byte) {
	t.Helper()

	res, err := http.Post(base+path, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, raw
}

var discoveryLast = []string{
	"LookupAnchorAccount",
	"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition",
	"DiscoverAccounts",
}

func roots() map[string]string {
	return map[string]string{"account_number": "SV-4417-0092", "as_of_date": "2026-09-30"}
}

func runOrdering(t *testing.T, base string, steps []string) *bank.Result {
	t.Helper()

	body, err := json.Marshal(map[string]any{"steps": steps, "data": roots()})
	require.NoError(t, err)

	code, raw := post(t, base, "/api/run?wait=true", string(body))
	require.Equal(t, http.StatusOK, code, string(raw))

	var resp struct {
		Result *bank.Result `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.NotNil(t, resp.Result)
	return resp.Result
}

// The headline claim, end to end: identical steps and identical data, reordered,
// produce a different answer.
func TestEndToEndReorderingChangesTheAnswer(t *testing.T) {
	base := newStack(t)

	canonical := runOrdering(t, base, dsl.CanonicalOrder())
	degraded := runOrdering(t, base, discoveryLast)

	require.Len(t, canonical.Trace, 6)
	require.Len(t, degraded.Trace, 6)

	assert.Equal(t, bank.ScopeDiscovered, canonical.Data["deposit_scope"])
	assert.Equal(t, bank.ScopePrimary, degraded.Data["deposit_scope"])
	assert.NotEqual(t, canonical.Data["deposit_total"], degraded.Data["deposit_total"])

	assert.Equal(t, "100", canonical.Data["position_confidence"])
	assert.Equal(t, "80", degraded.Data["position_confidence"])

	assert.Empty(t, canonical.Progress.Advisories)
	assert.Len(t, degraded.Progress.Advisories, 4)

	t.Logf("deposits: canonical %s (%s) vs discovery-last %s (%s)",
		canonical.Data["deposit_total"], canonical.Data["deposit_scope"],
		degraded.Data["deposit_total"], degraded.Data["deposit_scope"])
}

// ...while the data the caller must supply is identical for both.
func TestEndToEndRequiredDataIsUnchangedByOrdering(t *testing.T) {
	base := newStack(t)

	for _, steps := range [][]string{dsl.CanonicalOrder(), discoveryLast} {
		body, err := json.Marshal(map[string]any{"steps": steps})
		require.NoError(t, err)

		code, raw := post(t, base, "/api/plan", string(body))
		require.Equal(t, http.StatusOK, code, string(raw))

		var plan dsl.Plan
		require.NoError(t, json.Unmarshal(raw, &plan))

		var names []string
		for _, f := range plan.Required {
			names = append(names, f.Name)
		}
		assert.Equal(t, []string{"account_number", "as_of_date"}, names)
	}
}

func TestEndToEndCompareDiffsTheTwoOrderings(t *testing.T) {
	base := newStack(t)

	body, err := json.Marshal(map[string]any{
		"steps_a": dsl.CanonicalOrder(),
		"steps_b": discoveryLast,
		"data":    roots(),
	})
	require.NoError(t, err)

	code, raw := post(t, base, "/api/compare", string(body))
	require.Equal(t, http.StatusOK, code, string(raw))

	var resp struct {
		Diff []web.FieldDiff `json:"diff"`
	}
	require.NoError(t, json.Unmarshal(raw, &resp))

	changed := map[string]bool{}
	for _, d := range resp.Diff {
		changed[d.Field] = true
		assert.NotEqual(t, d.A, d.B)
	}

	for _, field := range []string{
		"deposit_total", "deposit_scope", "credit_scope", "lending_scope", "position_confidence",
	} {
		assert.True(t, changed[field], "%s should differ between the orderings", field)
	}
}

// An unmet optional must not be demanded as input.
func TestEndToEndDegradedStepStillRuns(t *testing.T) {
	base := newStack(t)

	code, raw := post(t, base, "/api/plan", `{"steps":["SummarizeDeposits"]}`)
	require.Equal(t, http.StatusOK, code, string(raw))

	var plan dsl.Plan
	require.NoError(t, json.Unmarshal(raw, &plan))
	require.Len(t, plan.Required, 2)
	require.Len(t, plan.Advisories, 1)
	assert.Equal(t, dsl.ModeScope, plan.Advisories[0].Mode)

	code, raw = post(t, base, "/api/run?wait=true",
		`{"steps":["SummarizeDeposits"],"data":{"customer_id":"CUST-88213","as_of_date":"2026-09-30"}}`)
	require.Equal(t, http.StatusOK, code, string(raw))

	var resp struct {
		Result *bank.Result `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.NotNil(t, resp.Result)
	assert.Equal(t, bank.ScopePrimary, resp.Result.Data["deposit_scope"])
}

func TestEndToEndProgressIsQueryableWhileRunning(t *testing.T) {
	base := newStack(t)

	body, err := json.Marshal(map[string]any{"steps": dsl.CanonicalOrder(), "data": roots()})
	require.NoError(t, err)

	code, raw := post(t, base, "/api/run", string(body))
	require.Equal(t, http.StatusOK, code, string(raw))

	var started struct {
		WorkflowID string `json:"workflow_id"`
		RunID      string `json:"run_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &started))

	seen := []int{}
	var final bank.Progress
	for i := 0; i < 150; i++ {
		res, err := http.Get(base + "/api/status?workflow_id=" + started.WorkflowID + "&run_id=" + started.RunID)
		require.NoError(t, err)
		payload, err := io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode, string(payload))

		var p bank.Progress
		require.NoError(t, json.Unmarshal(payload, &p))
		seen = append(seen, p.Completed)
		if p.Done {
			final = p
			break
		}
		time.Sleep(120 * time.Millisecond)
	}

	require.True(t, final.Done, "workflow never reported done; completions seen: %v", seen)
	assert.Equal(t, 6, final.Completed)
	assert.Equal(t, 100, final.Position.Confidence)
	assert.Less(t, seen[0], seen[len(seen)-1], "the query should show progress advancing")
}

func TestEndToEndMissingDataNeverStartsWorkflow(t *testing.T) {
	base := newStack(t)

	code, raw := post(t, base, "/api/run?wait=true",
		`{"steps":["LookupAnchorAccount"],"data":{"account_number":"SV-1"}}`)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	assert.Contains(t, string(raw), "as_of_date")
}

// The advisor path end to end: no lookup, no discovery, just an account number.
func TestEndToEndAdvisorPathFromAnAccountNumber(t *testing.T) {
	base := newStack(t)
	steps := []string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition"}

	body, err := json.Marshal(map[string]any{"steps": steps})
	require.NoError(t, err)
	code, raw := post(t, base, "/api/plan", string(body))
	require.Equal(t, http.StatusOK, code, string(raw))

	var plan dsl.Plan
	require.NoError(t, json.Unmarshal(raw, &plan))
	require.Len(t, plan.Required, 2)
	assert.Equal(t, "account_number", plan.Required[0].Name)
	require.Len(t, plan.Required[0].AnyOf, 2)

	body, err = json.Marshal(map[string]any{
		"steps": steps,
		"data":  map[string]string{"account_number": "SV-4417-0092", "as_of_date": "2026-09-30"},
	})
	require.NoError(t, err)

	code, raw = post(t, base, "/api/run?wait=true", string(body))
	require.Equal(t, http.StatusOK, code, string(raw))

	var resp struct {
		Result *bank.Result `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.NotNil(t, resp.Result)

	assert.Len(t, resp.Result.Trace, 4)
	assert.NotEmpty(t, resp.Result.Data["net_position"])
	assert.Equal(t, bank.ScopePrimary, resp.Result.Data["deposit_scope"])
}

// Whichever identifier the advisor uses, the figures agree.
func TestEndToEndEitherIdentifierAgrees(t *testing.T) {
	base := newStack(t)
	steps := []string{"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition"}

	anchor := runOrdering(t, base, []string{"LookupAnchorAccount"})
	customer := anchor.Data["customer_id"]
	require.NotEmpty(t, customer)

	runWith := func(data map[string]string) *bank.Result {
		body, err := json.Marshal(map[string]any{"steps": steps, "data": data})
		require.NoError(t, err)
		code, raw := post(t, base, "/api/run?wait=true", string(body))
		require.Equal(t, http.StatusOK, code, string(raw))

		var resp struct {
			Result *bank.Result `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &resp))
		require.NotNil(t, resp.Result)
		return resp.Result
	}

	byAccount := runWith(map[string]string{"account_number": "SV-4417-0092", "as_of_date": "2026-09-30"})
	byCustomer := runWith(map[string]string{"customer_id": customer, "as_of_date": "2026-09-30"})

	for _, field := range []string{"deposit_total", "card_balance", "loan_balance", "net_position"} {
		assert.Equal(t, byAccount.Data[field], byCustomer.Data[field], "%s", field)
	}
	t.Logf("net position from either identifier: %s", byAccount.Data["net_position"])
}

// A starved step is flagged at plan time and fails honestly at run time, rather
// than reporting a position of zero for a real customer.
func TestEndToEndStarvedStepIsFlaggedAndFails(t *testing.T) {
	base := newStack(t)

	code, raw := post(t, base, "/api/plan", `{"steps":["BuildPosition"]}`)
	require.Equal(t, http.StatusOK, code, string(raw))

	var plan dsl.Plan
	require.NoError(t, json.Unmarshal(raw, &plan))
	require.Len(t, plan.Blocked(), 1)
	assert.Equal(t, "BuildPosition", plan.Blocked()[0].Step)

	code, raw = post(t, base, "/api/run?wait=true",
		`{"steps":["BuildPosition"],"data":{"account_number":"SV-4417-0092"}}`)
	require.Equal(t, http.StatusBadGateway, code, string(raw))
	assert.Contains(t, string(raw), "no position to build")
}

func TestEndToEndOneFigureIsEnoughToReport(t *testing.T) {
	base := newStack(t)

	code, raw := post(t, base, "/api/plan", `{"steps":["SummarizeDeposits","BuildPosition"]}`)
	require.Equal(t, http.StatusOK, code, string(raw))

	var plan dsl.Plan
	require.NoError(t, json.Unmarshal(raw, &plan))
	assert.Empty(t, plan.Blocked())

	code, raw = post(t, base, "/api/run?wait=true",
		`{"steps":["SummarizeDeposits","BuildPosition"],"data":{"account_number":"SV-4417-0092","as_of_date":"2026-09-30"}}`)
	require.Equal(t, http.StatusOK, code, string(raw))

	var resp struct {
		Result *bank.Result `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.NotNil(t, resp.Result)
	assert.Equal(t, "20", resp.Result.Data["position_confidence"])
}

// The Workflow rejects a bad request on its own, without the web service having
// screened it first.
func TestEndToEndWorkflowRejectsBadRequestsDirectly(t *testing.T) {
	dev, err := testsuite.StartDevServer(context.Background(), testsuite.DevServerOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = dev.Stop() })

	c := dev.Client()
	w := worker.New(c, bank.TaskQueueName, worker.Options{})
	w.RegisterWorkflow(bank.PositionWorkflow)
	w.RegisterActivity(&bank.Activities{SpeedFactor: 0.02})
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	cases := []struct {
		name string
		req  bank.Request
		want string
	}{
		{"empty", bank.Request{Data: roots()}, "no steps provided"},
		{"duplicate", bank.Request{
			Steps: []string{"DiscoverAccounts", "DiscoverAccounts"},
			Data:  map[string]string{"customer_id": "CUST-1"},
		}, "duplicate step"},
		{"malformed date", bank.Request{
			Steps: dsl.CanonicalOrder(),
			Data:  map[string]string{"account_number": "SV-1", "as_of_date": "Sept 30"},
		}, "is unusable"},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
				ID:        fmt.Sprintf("direct-%d-%d", i, time.Now().UnixNano()),
				TaskQueue: bank.TaskQueueName,
			}, bank.PositionWorkflow, tc.req)
			require.NoError(t, err, "the request should be accepted, then rejected by the Workflow")

			var result bank.Result
			err = run.Get(context.Background(), &result)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)

			assertVersionMarkedBeforeValidation(t, c, run)
		})
	}
}

// Every execution records the validate-request-activity version marker before
// it schedules ValidateRequest, so a later change to the command sequence can
// branch on it.
func assertVersionMarkedBeforeValidation(t *testing.T, c client.Client, run client.WorkflowRun) {
	t.Helper()

	marked := false
	iter := c.GetWorkflowHistory(context.Background(), run.GetID(), run.GetRunID(), false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		require.NoError(t, err)

		if m := event.GetMarkerRecordedEventAttributes(); m != nil {
			for _, p := range m.GetDetails()["change-id"].GetPayloads() {
				if strings.Contains(string(p.GetData()), "validate-request-activity") {
					marked = true
				}
			}
		}
		if a := event.GetActivityTaskScheduledEventAttributes(); a != nil {
			require.Equal(t, "ValidateRequest", a.GetActivityType().GetName())
			require.True(t, marked, "ValidateRequest was scheduled before the version marker was recorded")
			return
		}
	}
	t.Fatal("ValidateRequest was never scheduled")
}
