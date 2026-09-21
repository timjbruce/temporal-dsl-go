package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"dsl/dsl"
	bank "dsl/workflow"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

//go:embed index.html
var assets embed.FS

// Starter is the slice of client.Client the server needs, so handlers can be
// tested without a Temporal connection.
type Starter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
	QueryWorkflow(ctx context.Context, workflowID string, runID string, queryType string, args ...interface{}) (converter.EncodedValue, error)
}

type Server struct {
	client Starter
	queue  string
}

func NewServer(c Starter) *Server {
	return &Server{client: c, queue: bank.TaskQueueName}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/steps", s.handleSteps)
	mux.HandleFunc("POST /api/plan", s.handlePlan)
	mux.HandleFunc("POST /api/run", s.handleRun)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/compare", s.handleCompare)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

type catalogResponse struct {
	Steps  []dsl.StepDef        `json:"steps"`
	Fields map[string]dsl.Field `json:"fields"`
}

func (s *Server) handleSteps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, catalogResponse{Steps: dsl.Steps, Fields: dsl.Fields})
}

type planRequest struct {
	Steps []string          `json:"steps"`
	Data  map[string]string `json:"data"`
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req planRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	plan := dsl.Resolve(req.Steps)
	if !plan.OK() {
		writeJSON(w, http.StatusBadRequest, plan)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type runResponse struct {
	WorkflowID string          `json:"workflow_id"`
	RunID      string          `json:"run_id"`
	Steps      []string        `json:"steps,omitempty"`
	Result     *bank.Result    `json:"result,omitempty"`
	Validation *dsl.Validation `json:"validation,omitempty"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req planRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	plan := dsl.Resolve(req.Steps)
	if !plan.OK() {
		writeJSON(w, http.StatusBadRequest, plan)
		return
	}

	v := dsl.Validate(plan, req.Data)
	if !v.OK() {
		writeJSON(w, http.StatusUnprocessableEntity, runResponse{Validation: &v})
		return
	}

	resp, err := s.start(r.Context(), req.Steps, req.Data, r.URL.Query().Get("wait") == "true")
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	resp.Validation = &v
	writeJSON(w, http.StatusOK, resp)
}

// start launches one ordering. Callers validate first; compare reuses this to
// run two orderings against the same data.
func (s *Server) start(ctx context.Context, steps []string, data map[string]string, wait bool) (*runResponse, error) {
	opts := client.StartWorkflowOptions{
		ID:        fmt.Sprintf("position-%d", time.Now().UnixNano()),
		TaskQueue: s.queue,
	}

	run, err := s.client.ExecuteWorkflow(ctx, opts, bank.PositionWorkflow, bank.Request{
		Steps: steps,
		Data:  data,
	})
	if err != nil {
		return nil, fmt.Errorf("could not start workflow: %w", err)
	}

	resp := &runResponse{WorkflowID: run.GetID(), RunID: run.GetRunID(), Steps: steps}
	if !wait {
		return resp, nil
	}

	var result bank.Result
	if err := run.Get(ctx, &result); err != nil {
		return nil, fmt.Errorf("workflow failed: %w", err)
	}
	resp.Result = &result
	return resp, nil
}

// handleStatus queries the running Workflow, so a caller can watch the position
// build while the slower systems are still responding.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("workflow_id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "workflow_id is required")
		return
	}

	val, err := s.client.QueryWorkflow(r.Context(), id, r.URL.Query().Get("run_id"), bank.ProgressQuery)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("could not query workflow: %v", err))
		return
	}

	var progress bank.Progress
	if err := val.Get(&progress); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("could not read progress: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, progress)
}

type compareRequest struct {
	StepsA []string          `json:"steps_a"`
	StepsB []string          `json:"steps_b"`
	Data   map[string]string `json:"data"`
}

type FieldDiff struct {
	Field string `json:"field"`
	Label string `json:"label"`
	A     string `json:"a"`
	B     string `json:"b"`
}

type compareResponse struct {
	A    *runResponse `json:"a"`
	B    *runResponse `json:"b"`
	Diff []FieldDiff  `json:"diff"`
}

// handleCompare runs the same data under two orderings so the caller can see
// that ordering changes the answer, not just the required fields.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	var req compareRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	planA, planB := dsl.Resolve(req.StepsA), dsl.Resolve(req.StepsB)
	if !planA.OK() || !planB.OK() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"a": planA, "b": planB})
		return
	}

	vA, vB := dsl.Validate(planA, req.Data), dsl.Validate(planB, req.Data)
	if !vA.OK() || !vB.OK() {
		writeJSON(w, http.StatusUnprocessableEntity, compareResponse{
			A: &runResponse{Validation: &vA},
			B: &runResponse{Validation: &vB},
		})
		return
	}

	type outcome struct {
		resp *runResponse
		err  error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup

	for i, steps := range [][]string{req.StepsA, req.StepsB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := s.start(r.Context(), steps, req.Data, true)
			results[i] = outcome{resp, err}
		}()
	}
	wg.Wait()

	for _, o := range results {
		if o.err != nil {
			writeError(w, http.StatusBadGateway, o.err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, compareResponse{
		A:    results[0].resp,
		B:    results[1].resp,
		Diff: diffResults(results[0].resp.Result, results[1].resp.Result),
	})
}

func diffResults(a, b *bank.Result) []FieldDiff {
	out := []FieldDiff{}
	if a == nil || b == nil {
		return out
	}

	keys := map[string]bool{}
	for k := range a.Data {
		keys[k] = true
	}
	for k := range b.Data {
		keys[k] = true
	}

	for _, field := range orderedFields(keys) {
		if a.Data[field] == b.Data[field] {
			continue
		}
		out = append(out, FieldDiff{
			Field: field,
			Label: dsl.Fields[field].Label,
			A:     a.Data[field],
			B:     b.Data[field],
		})
	}
	return out
}

// Order the diff the way the catalog declares its fields, so it reads in a
// stable, meaningful sequence rather than by map iteration.
func orderedFields(keys map[string]bool) []string {
	out := []string{}
	seen := map[string]bool{}

	for _, step := range dsl.Steps {
		for _, field := range step.Produces {
			if keys[field] && !seen[field] {
				out = append(out, field)
				seen[field] = true
			}
		}
	}
	for field := range keys {
		if !seen[field] {
			out = append(out, field)
			seen[field] = true
		}
	}
	return out
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, err := assets.ReadFile("index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(body)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
