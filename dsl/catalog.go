package dsl

type Field struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	Example string `json:"example"`
}

const (
	TypeString  = "string"
	TypeInteger = "integer"
	TypeNumber  = "number"
	TypeEmail   = "email"
	TypeDate    = "date"
)

// An optional input changes what a step does rather than whether it can run.
// The mode says how.
const (
	ModeScope      = "scope"      // widens what the step covers
	ModeConfidence = "confidence" // raises how much the step trusts its own answer
	ModeExclusion  = "exclusion"  // narrows the result, dropping what is already known
)

const (
	TierLong     = "long"
	TierModerate = "moderate"
	TierFast     = "fast"
)

type StepDef struct {
	Name        string   `json:"name"`
	System      string   `json:"system"`
	Description string   `json:"description"`
	Requires    []string `json:"requires"`

	// RequiresOneOf is a group of interchangeable identifiers: any one of them
	// satisfies the step. Product systems hold the accounts themselves, so they
	// can find an owner from an account number; the customer hub cannot.
	RequiresOneOf []string `json:"requires_one_of"`

	Optional []string `json:"optional"`

	// OptionalMode governs what Optional buys. Effect renders the advisory shown
	// when those inputs are not available at a step's position in the ordering.
	OptionalMode string `json:"optional_mode"`
	Effect       string `json:"effect"`

	// MinOptional is how many optional inputs the step needs before it can say
	// anything at all. Below it the step has nothing to report and fails, rather
	// than passing off an empty answer as a real one. EmptyEffect is the
	// advisory for that case.
	MinOptional int    `json:"min_optional"`
	EmptyEffect string `json:"empty_effect"`

	Produces    []string `json:"produces"`
	Contributes bool     `json:"contributes"`
	MinSeconds  int      `json:"min_seconds"`
	MaxSeconds  int      `json:"max_seconds"`
	SpeedTier   string   `json:"speed_tier"`
}

// SpeedTier is derived from the declared range rather than stored alongside it,
// so a retimed step cannot end up wearing the wrong label.
func SpeedTier(minSeconds, maxSeconds int) string {
	switch mid := float64(minSeconds+maxSeconds) / 2; {
	case mid >= 25:
		return TierLong
	case mid >= 12:
		return TierModerate
	}
	return TierFast
}

func AvgSeconds(def StepDef) float64 {
	return float64(def.MinSeconds+def.MaxSeconds) / 2
}

var Fields = map[string]Field{
	"account_number": {"account_number", "Anchor account number", TypeString, "SV-4417-0092"},
	"as_of_date":     {"as_of_date", "Position as of date", TypeDate, "2026-09-30"},

	"customer_id":    {"customer_id", "Customer identifier", TypeString, "CUST-88213"},
	"anchor_type":    {"anchor_type", "Anchor account type", TypeString, "savings"},
	"anchor_balance": {"anchor_balance", "Anchor account balance", TypeNumber, "18420.55"},

	"account_list":    {"account_list", "Discovered account references", TypeString, "CK-1120,CC-8841,ML-2207"},
	"account_count":   {"account_count", "Discovered account count", TypeInteger, "3"},
	"discovery_scope": {"discovery_scope", "What discovery covered", TypeString, "excluding savings"},

	"deposit_total": {"deposit_total", "Deposit balance total", TypeNumber, "28940.12"},
	"deposit_count": {"deposit_count", "Deposit accounts counted", TypeInteger, "2"},
	"deposit_scope": {"deposit_scope", "What the deposit summary covered", TypeString, "all-discovered"},

	"card_balance":       {"card_balance", "Credit card balance", TypeNumber, "3180.44"},
	"credit_utilization": {"credit_utilization", "Credit utilization (%)", TypeNumber, "21.2"},
	"credit_scope":       {"credit_scope", "What the credit summary covered", TypeString, "primary-only"},

	"loan_balance":  {"loan_balance", "Lending balance total", TypeNumber, "244900.00"},
	"loan_count":    {"loan_count", "Loans counted", TypeInteger, "1"},
	"lending_scope": {"lending_scope", "What the lending summary covered", TypeString, "all-discovered"},

	"net_position":        {"net_position", "Net position", TypeNumber, "-199139.32"},
	"coverage_ratio":      {"coverage_ratio", "Assets over liabilities", TypeNumber, "0.12"},
	"position_confidence": {"position_confidence", "Position confidence (0-100)", TypeInteger, "75"},
}

var Steps = []StepDef{
	{
		Name:        "LookupAnchorAccount",
		System:      "Core banking",
		Description: "Pull the account you already know about and identify who owns it.",
		Requires:    []string{"account_number", "as_of_date"},
		Produces:    []string{"customer_id", "anchor_type", "anchor_balance"},
		MinSeconds:  3, MaxSeconds: 6,
	},
	{
		Name:         "DiscoverAccounts",
		System:       "Customer hub",
		Description:  "Enumerate the customer's other accounts. The customer hub is keyed by customer, so it needs a customer id.",
		Requires:     []string{"customer_id"},
		Optional:     []string{"anchor_type"},
		OptionalMode: ModeExclusion,
		Effect:       "will list every account including the one already retrieved",
		Produces:     []string{"account_list", "account_count", "discovery_scope"},
		MinSeconds:   12, MaxSeconds: 20,
	},
	{
		Name:          "SummarizeDeposits",
		System:        "Deposits",
		Description:   "Total checking and savings balances. Finds the owner from either identifier.",
		Requires:      []string{"as_of_date"},
		RequiresOneOf: []string{"account_number", "customer_id"},
		Optional:      []string{"account_list"},
		OptionalMode:  ModeScope,
		Effect:        "will cover the primary deposit account only",
		Produces:      []string{"deposit_total", "deposit_count", "deposit_scope"},
		Contributes:   true,
		MinSeconds:    8, MaxSeconds: 14,
	},
	{
		Name:          "SummarizeCredit",
		System:        "Card processor",
		Description:   "Total revolving card balances and utilization. Finds the owner from either identifier.",
		Requires:      []string{"as_of_date"},
		RequiresOneOf: []string{"account_number", "customer_id"},
		Optional:      []string{"account_list"},
		OptionalMode:  ModeScope,
		Effect:        "will cover the primary card only",
		Produces:      []string{"card_balance", "credit_utilization", "credit_scope"},
		Contributes:   true,
		MinSeconds:    15, MaxSeconds: 25,
	},
	{
		Name:          "SummarizeLending",
		System:        "Loan servicing",
		Description:   "Total mortgage and installment loan balances. Finds the owner from either identifier.",
		Requires:      []string{"as_of_date"},
		RequiresOneOf: []string{"account_number", "customer_id"},
		Optional:      []string{"account_list"},
		OptionalMode:  ModeScope,
		Effect:        "will cover the primary loan only",
		Produces:      []string{"loan_balance", "loan_count", "lending_scope"},
		Contributes:   true,
		MinSeconds:    20, MaxSeconds: 35,
	},
	{
		Name:          "BuildPosition",
		System:        "Reporting",
		Description:   "Fold whatever has landed into a net position and say how complete it is.",
		RequiresOneOf: []string{"account_number", "customer_id"},
		Optional:      []string{"deposit_total", "card_balance", "loan_balance", "anchor_balance", "account_count"},
		OptionalMode:  ModeConfidence,
		Effect:        "will report reduced confidence",
		MinOptional:   1,
		EmptyEffect:   "has no figures to fold and cannot report a position at all",
		Produces:      []string{"net_position", "coverage_ratio", "position_confidence"},
		Contributes:   true,
		MinSeconds:    2, MaxSeconds: 4,
	},
}

var stepIndex = map[string]StepDef{}

func init() {
	for i := range Steps {
		Steps[i].SpeedTier = SpeedTier(Steps[i].MinSeconds, Steps[i].MaxSeconds)
		stepIndex[Steps[i].Name] = Steps[i]
	}
}

func Step(name string) (StepDef, bool) {
	s, ok := stepIndex[name]
	return s, ok
}

func CanonicalOrder() []string {
	names := make([]string, len(Steps))
	for i, s := range Steps {
		names[i] = s.Name
	}
	return names
}

// ProducerOf names the step that produces a field, if any does.
func ProducerOf(field string) (string, bool) {
	for _, s := range Steps {
		for _, key := range s.Produces {
			if key == field {
				return s.Name, true
			}
		}
	}
	return "", false
}
