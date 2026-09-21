package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"dsl/dsl"
	"dsl/web"
	bank "dsl/workflow"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

type fakeRun struct {
	id, runID string
	result    bank.Result
	err       error
}

func (f *fakeRun) GetID() string                  { return f.id }
func (f *fakeRun) GetRunID() string               { return f.runID }
func (f *fakeRun) GetFirstExecutionRunID() string { return f.runID }
func (f *fakeRun) Get(ctx context.Context, valuePtr interface{}) error {
	if f.err != nil {
		return f.err
	}
	if valuePtr != nil {
		*(valuePtr.(*bank.Result)) = f.result
	}
	return nil
}
func (f *fakeRun) GetWithOptions(ctx context.Context, valuePtr interface{}, _ client.WorkflowRunGetOptions) error {
	return f.Get(ctx, valuePtr)
}

type fakeStarter struct {
	mu       sync.Mutex
	calls    int
	gotReqs  []bank.Request
	gotOpts  client.StartWorkflowOptions
	err      error
	results  map[string]bank.Result // keyed by first step, for compare
	run      *fakeRun
	queries  int
	queryID  string
	queryTyp string
	progress bank.Progress
	queryErr error
}

func (f *fakeStarter) ExecuteWorkflow(ctx context.Context, opts client.StartWorkflowOptions, wf interface{}, args ...interface{}) (client.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.gotOpts = opts
	if f.err != nil {
		return nil, f.err
	}

	req := args[0].(bank.Request)
	f.gotReqs = append(f.gotReqs, req)

	run := &fakeRun{id: opts.ID, runID: "run-" + opts.ID, result: f.run.result}
	if len(req.Steps) > 0 {
		if r, ok := f.results[req.Steps[0]]; ok {
			run.result = r
		}
	}
	return run, nil
}

func (f *fakeStarter) QueryWorkflow(ctx context.Context, workflowID, runID, queryType string, args ...interface{}) (converter.EncodedValue, error) {
	f.queries++
	f.queryID = workflowID
	f.queryTyp = queryType
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return encodedProgress{f.progress}, nil
}

type encodedProgress struct{ p bank.Progress }

func (e encodedProgress) HasValue() bool { return true }
func (e encodedProgress) Get(valuePtr interface{}) error {
	*(valuePtr.(*bank.Progress)) = e.p
	return nil
}

func newStarter() *fakeStarter {
	return &fakeStarter{
		results: map[string]bank.Result{},
		run: &fakeRun{
			result: bank.Result{
				Data: map[string]string{
					"deposit_total": "28940.12", "deposit_scope": bank.ScopeDiscovered,
					"position_confidence": "100",
				},
				Trace:    []bank.StepTrace{{Step: "SummarizeDeposits", Order: 1}},
				Progress: bank.Progress{Done: true, Completed: 1, Total: 1},
			},
		},
		progress: bank.Progress{
			Completed: 3, Total: 6, Current: "SummarizeLending",
			Position:   bank.PositionSnapshot{DepositTotal: 28940.12, Confidence: 25},
			Advisories: []string{"SummarizeDeposits will cover the primary deposit account only"},
		},
	}
}

func do(t *testing.T, s *web.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

func TestStepsEndpointReturnsCatalog(t *testing.T) {
	rec := do(t, web.NewServer(newStarter()), http.MethodGet, "/api/steps", "")
	require.Equal(t, http.StatusOK, rec.Code)

	body := decodeInto[struct {
		Steps  []dsl.StepDef        `json:"steps"`
		Fields map[string]dsl.Field `json:"fields"`
	}](t, rec)

	require.Len(t, body.Steps, 6)
	assert.Equal(t, "LookupAnchorAccount", body.Steps[0].Name)
	assert.Equal(t, "Core banking", body.Steps[0].System)

	deposits := body.Steps[2]
	assert.Equal(t, []string{"account_list"}, deposits.Optional)
	assert.Equal(t, dsl.ModeScope, deposits.OptionalMode)
	assert.NotEmpty(t, deposits.Effect)
	assert.Equal(t, 8, deposits.MinSeconds)
	assert.Equal(t, "Position as of date", body.Fields["as_of_date"].Label)
}

func TestPlanEndpointCanonicalOrder(t *testing.T) {
	steps, _ := json.Marshal(map[string]any{"steps": dsl.CanonicalOrder()})
	rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/plan", string(steps))
	require.Equal(t, http.StatusOK, rec.Code)

	plan := decodeInto[dsl.Plan](t, rec)
	require.Len(t, plan.Required, 2)
	assert.Equal(t, "account_number", plan.Required[0].Name)
	assert.Equal(t, "as_of_date", plan.Required[1].Name)
	assert.Empty(t, plan.Advisories, "nothing is degraded in the canonical order")
	assert.Equal(t, 60, plan.Total.MinSeconds)
}

// The endpoint must distinguish "you must supply this" from "this would be better".
func TestPlanEndpointSeparatesRequiredFromAdvisory(t *testing.T) {
	rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/plan",
		`{"steps":["SummarizeDeposits"]}`)
	require.Equal(t, http.StatusOK, rec.Code)

	plan := decodeInto[dsl.Plan](t, rec)

	var names []string
	for _, f := range plan.Required {
		names = append(names, f.Name)
	}
	assert.Equal(t, []string{"account_number", "as_of_date"}, names)
	assert.NotContains(t, names, "account_list")

	require.Len(t, plan.Advisories, 1)
	assert.Equal(t, dsl.ModeScope, plan.Advisories[0].Mode)
	assert.Equal(t, []string{"DiscoverAccounts"}, plan.Advisories[0].Producers)
}

func TestPlanEndpointReportsDegradedSteps(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"steps": []string{
		"LookupAnchorAccount", "SummarizeDeposits", "DiscoverAccounts",
	}})
	rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/plan", string(body))
	require.Equal(t, http.StatusOK, rec.Code)

	plan := decodeInto[dsl.Plan](t, rec)
	require.Len(t, plan.Steps, 3)
	assert.Equal(t, []string{"account_list"}, plan.Steps[1].Degraded)
	assert.Equal(t, map[string]string{"anchor_type": "LookupAnchorAccount"}, plan.Steps[2].EnrichedBy)
}

func TestPlanEndpointRejectsBadOrdering(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"unknown step", `{"steps":["SummarizeCrypto"]}`, `unknown step "SummarizeCrypto"`},
		{"duplicate step", `{"steps":["DiscoverAccounts","DiscoverAccounts"]}`, `duplicate step "DiscoverAccounts"`},
		{"empty", `{"steps":[]}`, "no steps provided"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/plan", tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, decodeInto[dsl.Plan](t, rec).Errors, tc.want)
		})
	}
}

func TestPlanEndpointRejectsMalformedJSON(t *testing.T) {
	rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/plan", `{"steps":`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRunEndpointReportsMissingDataWithoutStarting(t *testing.T) {
	starter := newStarter()
	rec := do(t, web.NewServer(starter), http.MethodPost, "/api/run",
		`{"steps":["LookupAnchorAccount"],"data":{"account_number":"SV-1"}}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Zero(t, starter.calls, "must not start a workflow with incomplete data")

	body := decodeInto[struct {
		Validation dsl.Validation `json:"validation"`
	}](t, rec)
	require.Len(t, body.Validation.Missing, 1)
	assert.Equal(t, "as_of_date", body.Validation.Missing[0].Name)
}

func TestRunEndpointReportsInvalidDate(t *testing.T) {
	starter := newStarter()
	rec := do(t, web.NewServer(starter), http.MethodPost, "/api/run",
		`{"steps":["LookupAnchorAccount"],"data":{"account_number":"SV-1","as_of_date":"Sept 30"}}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Zero(t, starter.calls)

	body := decodeInto[struct {
		Validation dsl.Validation `json:"validation"`
	}](t, rec)
	require.Len(t, body.Validation.Invalid, 1)
	assert.Contains(t, body.Validation.Invalid[0].Reason, "YYYY-MM-DD")
}

// A degraded step is runnable: an unmet optional must not block the run.
func TestRunEndpointStartsDespiteAdvisories(t *testing.T) {
	starter := newStarter()
	rec := do(t, web.NewServer(starter), http.MethodPost, "/api/run",
		`{"steps":["SummarizeDeposits"],"data":{"customer_id":"CUST-1","as_of_date":"2026-09-30"}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, starter.calls)
	assert.Equal(t, []string{"SummarizeDeposits"}, starter.gotReqs[0].Steps)
	assert.Equal(t, bank.TaskQueueName, starter.gotOpts.TaskQueue)

	body := decodeInto[struct {
		WorkflowID string       `json:"workflow_id"`
		Result     *bank.Result `json:"result"`
	}](t, rec)
	assert.NotEmpty(t, body.WorkflowID)
	assert.Nil(t, body.Result, "result is only returned when wait=true")
}

func TestRunEndpointWaitsForResult(t *testing.T) {
	rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/run?wait=true",
		`{"steps":["SummarizeDeposits"],"data":{"customer_id":"CUST-1","as_of_date":"2026-09-30"}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeInto[struct {
		Result *bank.Result `json:"result"`
	}](t, rec)
	require.NotNil(t, body.Result)
	assert.Equal(t, "28940.12", body.Result.Data["deposit_total"])
}

func TestRunEndpointSurfacesStartFailure(t *testing.T) {
	starter := newStarter()
	starter.err = assert.AnError

	rec := do(t, web.NewServer(starter), http.MethodPost, "/api/run",
		`{"steps":["DiscoverAccounts"],"data":{"customer_id":"CUST-1"}}`)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "could not start workflow")
}

func TestStatusEndpointReportsRunningPosition(t *testing.T) {
	starter := newStarter()
	rec := do(t, web.NewServer(starter), http.MethodGet,
		"/api/status?workflow_id=position-1&run_id=run-1", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, starter.queries)
	assert.Equal(t, "position-1", starter.queryID)
	assert.Equal(t, bank.ProgressQuery, starter.queryTyp)

	p := decodeInto[bank.Progress](t, rec)
	assert.Equal(t, 3, p.Completed)
	assert.Equal(t, "SummarizeLending", p.Current)
	assert.Equal(t, 28940.12, p.Position.DepositTotal)
	require.Len(t, p.Advisories, 1)
}

func TestStatusEndpointRequiresWorkflowID(t *testing.T) {
	rec := do(t, web.NewServer(newStarter()), http.MethodGet, "/api/status", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "workflow_id is required")
}

func TestStatusEndpointSurfacesQueryFailure(t *testing.T) {
	starter := newStarter()
	starter.queryErr = assert.AnError

	rec := do(t, web.NewServer(starter), http.MethodGet, "/api/status?workflow_id=nope", "")
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "could not query workflow")
}

func TestCompareEndpointDiffsTwoOrderings(t *testing.T) {
	starter := newStarter()
	starter.results["LookupAnchorAccount"] = bank.Result{
		Data: map[string]string{
			"deposit_total": "28940.12", "deposit_scope": bank.ScopeDiscovered,
			"account_count": "6", "position_confidence": "100",
		},
	}
	starter.results["SummarizeDeposits"] = bank.Result{
		Data: map[string]string{
			"deposit_total": "4210.88", "deposit_scope": bank.ScopePrimary,
			"account_count": "6", "position_confidence": "75",
		},
	}

	body, _ := json.Marshal(map[string]any{
		"steps_a": []string{"LookupAnchorAccount", "DiscoverAccounts", "SummarizeDeposits"},
		"steps_b": []string{"SummarizeDeposits", "LookupAnchorAccount", "DiscoverAccounts"},
		"data":    map[string]string{"account_number": "SV-1", "as_of_date": "2026-09-30", "customer_id": "CUST-1"},
	})
	rec := do(t, web.NewServer(starter), http.MethodPost, "/api/compare", string(body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 2, starter.calls, "both orderings must actually run")

	resp := decodeInto[struct {
		A *struct {
			WorkflowID string `json:"workflow_id"`
		} `json:"a"`
		B *struct {
			WorkflowID string `json:"workflow_id"`
		} `json:"b"`
		Diff []web.FieldDiff `json:"diff"`
	}](t, rec)

	require.NotNil(t, resp.A)
	require.NotNil(t, resp.B)
	assert.NotEqual(t, resp.A.WorkflowID, resp.B.WorkflowID)

	fields := map[string][2]string{}
	for _, d := range resp.Diff {
		fields[d.Field] = [2]string{d.A, d.B}
	}
	assert.Equal(t, [2]string{"28940.12", "4210.88"}, fields["deposit_total"])
	assert.Equal(t, [2]string{bank.ScopeDiscovered, bank.ScopePrimary}, fields["deposit_scope"])
	assert.Equal(t, [2]string{"100", "75"}, fields["position_confidence"])
	assert.NotContains(t, fields, "account_count", "identical fields stay out of the diff")

	for _, d := range resp.Diff {
		assert.NotEmpty(t, d.Label, "%s has no label", d.Field)
	}
}

func TestCompareEndpointOrdersDiffByCatalog(t *testing.T) {
	starter := newStarter()
	starter.results["LookupAnchorAccount"] = bank.Result{Data: map[string]string{
		"position_confidence": "100", "deposit_total": "1", "account_count": "6",
	}}
	starter.results["SummarizeDeposits"] = bank.Result{Data: map[string]string{
		"position_confidence": "75", "deposit_total": "2", "account_count": "5",
	}}

	body, _ := json.Marshal(map[string]any{
		"steps_a": []string{"LookupAnchorAccount", "DiscoverAccounts", "SummarizeDeposits"},
		"steps_b": []string{"SummarizeDeposits", "LookupAnchorAccount", "DiscoverAccounts"},
		"data":    map[string]string{"account_number": "SV-1", "as_of_date": "2026-09-30", "customer_id": "CUST-1"},
	})
	rec := do(t, web.NewServer(starter), http.MethodPost, "/api/compare", string(body))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeInto[struct {
		Diff []web.FieldDiff `json:"diff"`
	}](t, rec)

	var order []string
	for _, d := range resp.Diff {
		order = append(order, d.Field)
	}
	assert.Equal(t, []string{"account_count", "deposit_total", "position_confidence"}, order,
		"diff follows catalog declaration order, not map iteration order")
}

func TestCompareEndpointValidatesBothOrderings(t *testing.T) {
	starter := newStarter()
	// DiscoverAccounts takes only a customer id, so an account number leaves
	// ordering B unsatisfiable even though A is fine.
	body, _ := json.Marshal(map[string]any{
		"steps_a": []string{"LookupAnchorAccount"},
		"steps_b": []string{"DiscoverAccounts"},
		"data":    map[string]string{"account_number": "SV-1", "as_of_date": "2026-09-30"},
	})

	rec := do(t, web.NewServer(starter), http.MethodPost, "/api/compare", string(body))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Zero(t, starter.calls, "neither ordering may start when one is unsatisfiable")
	assert.Contains(t, rec.Body.String(), "customer_id")
}

func TestCompareEndpointRejectsBadOrdering(t *testing.T) {
	rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/compare",
		`{"steps_a":["SummarizeCrypto"],"steps_b":["DiscoverAccounts"],"data":{}}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unknown step")
}

func TestIndexIsServed(t *testing.T) {
	rec := do(t, web.NewServer(newStarter()), http.MethodGet, "/", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, rec.Body.String(), "Banking Position")
}

func TestUnknownPathIs404(t *testing.T) {
	rec := do(t, web.NewServer(newStarter()), http.MethodGet, "/nope", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// The advisor path over HTTP: no lookup, no discovery, just an account number.
func TestPlanEndpointOffersEitherIdentifier(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"steps": []string{
		"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition",
	}})
	rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/plan", string(body))
	require.Equal(t, http.StatusOK, rec.Code)

	plan := decodeInto[dsl.Plan](t, rec)
	require.Len(t, plan.Required, 2)

	id := plan.Required[0]
	assert.Equal(t, "account_number", id.Name)
	require.Len(t, id.AnyOf, 2)
	assert.Equal(t, "account_number", id.AnyOf[0].Name)
	assert.Equal(t, "customer_id", id.AnyOf[1].Name)
	assert.Len(t, id.NeededBy, 4)

	assert.Equal(t, "as_of_date", plan.Required[1].Name)
	assert.Empty(t, plan.Required[1].AnyOf)
}

func TestRunEndpointAcceptsEitherIdentifier(t *testing.T) {
	steps := []string{"SummarizeDeposits", "BuildPosition"}

	for _, tc := range []struct{ name, id, value string }{
		{"account number", "account_number", "SV-4417-0092"},
		{"customer id", "customer_id", "CUST-88213"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starter := newStarter()
			body, _ := json.Marshal(map[string]any{
				"steps": steps,
				"data":  map[string]string{tc.id: tc.value, "as_of_date": "2026-09-30"},
			})

			rec := do(t, web.NewServer(starter), http.MethodPost, "/api/run", string(body))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, 1, starter.calls)
			assert.Equal(t, tc.value, starter.gotReqs[0].Data[tc.id])
		})
	}
}

func TestRunEndpointRejectsWhenNeitherIdentifierIsGiven(t *testing.T) {
	starter := newStarter()
	rec := do(t, web.NewServer(starter), http.MethodPost, "/api/run",
		`{"steps":["SummarizeDeposits"],"data":{"as_of_date":"2026-09-30"}}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Zero(t, starter.calls)

	body := decodeInto[struct {
		Validation dsl.Validation `json:"validation"`
	}](t, rec)
	require.Len(t, body.Validation.Missing, 1)
	assert.Equal(t, "account_number", body.Validation.Missing[0].Name)
	assert.Len(t, body.Validation.Missing[0].AnyOf, 2)
}

// An ordering that starves a step is flagged before it runs, distinctly from one
// that merely narrows it.
func TestPlanEndpointFlagsABlockedStep(t *testing.T) {
	rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/plan", `{"steps":["BuildPosition"]}`)
	require.Equal(t, http.StatusOK, rec.Code)

	plan := decodeInto[dsl.Plan](t, rec)
	require.Len(t, plan.Advisories, 1)
	assert.Equal(t, dsl.SeverityEmpty, plan.Advisories[0].Severity)
	assert.Contains(t, plan.Advisories[0].Effect, "cannot report a position at all")
	assert.Len(t, plan.Blocked(), 1)
}

func TestPlanEndpointMarksNarrowedStepsAsDegradedNotBlocked(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"steps": []string{
		"SummarizeDeposits", "SummarizeCredit", "SummarizeLending", "BuildPosition",
	}})
	rec := do(t, web.NewServer(newStarter()), http.MethodPost, "/api/plan", string(body))
	require.Equal(t, http.StatusOK, rec.Code)

	plan := decodeInto[dsl.Plan](t, rec)
	require.NotEmpty(t, plan.Advisories)
	for _, a := range plan.Advisories {
		assert.Equal(t, dsl.SeverityDegraded, a.Severity, "%s", a.Step)
	}
	assert.Empty(t, plan.Blocked(), "the advisor path runs fine, just narrowly")
}
