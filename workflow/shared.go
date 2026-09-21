package workflow

const (
	TaskQueueName = "banking-position-dsl"
	ProgressQuery = "position_progress"
)

type Request struct {
	Steps []string          `json:"steps"`
	Data  map[string]string `json:"data"`
}

type StepTrace struct {
	Step       string            `json:"step"`
	Order      int               `json:"order"`
	Inputs     map[string]string `json:"inputs"`
	Enriched   []string          `json:"enriched"`
	Degraded   []string          `json:"degraded"`
	Outputs    map[string]string `json:"outputs"`
	ElapsedSec float64           `json:"elapsed_sec"`
}

// PositionSnapshot is the running answer, updated as each summary lands.
type PositionSnapshot struct {
	DepositTotal  float64           `json:"deposit_total"`
	CardBalance   float64           `json:"card_balance"`
	LoanBalance   float64           `json:"loan_balance"`
	NetPosition   float64           `json:"net_position"`
	CoverageRatio float64           `json:"coverage_ratio"`
	Confidence    int               `json:"confidence"`
	Scopes        map[string]string `json:"scopes"`
}

type PositionPoint struct {
	Order       int     `json:"order"`
	AfterStep   string  `json:"after_step"`
	NetPosition float64 `json:"net_position"`
	Confidence  int     `json:"confidence"`
	ElapsedSec  float64 `json:"elapsed_sec"`
	Contributed bool    `json:"contributed"`
}

// Progress is what the running Workflow reports through its query handler.
// Advisories record steps that ran degraded because of where they sat in the
// ordering.
type Progress struct {
	Done      bool   `json:"done"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
	Current   string `json:"current"`

	ElapsedSec     float64 `json:"elapsed_sec"`
	FirstResultSec float64 `json:"first_result_sec"`

	Position   PositionSnapshot `json:"position"`
	Advisories []string         `json:"advisories"`
	History    []PositionPoint  `json:"history"`
}

type Result struct {
	Data     map[string]string `json:"data"`
	Trace    []StepTrace       `json:"trace"`
	Progress Progress          `json:"progress"`
}
