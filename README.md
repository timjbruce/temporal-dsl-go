# Temporal DSL Workflow — Retail Banking Position

A Temporal Workflow that executes a caller-supplied *ordering* of activities against a
caller-supplied *data bag*, driven by JSON:

```json
{ "steps": ["LookupAnchorAccount", "DiscoverAccounts"], "data": { "account_number": "SV-4417-0092" } }
```

Steps declare what they **require** (you must supply it) and what they can optionally **use**. An
unmet optional never blocks a run — the step just does less. That is why the same steps with the same
data produce a **different answer** depending on the order you put them in.

> All data is **synthetic**. The activities are simulators — no network calls, no real accounts.
> Values are seeded from the identifier, so a given account yields the same numbers on every run and
> on every retry. (That is about repeat runs, not replay: replay reads recorded activity results from
> Event History and does not re-execute activities at all.)

## The six steps

`!` required, `|` either one will do, `?` optional.

```
1 LookupAnchorAccount  ! account_number, as_of_date            -> customer_id, anchor_type, anchor_balance
2 DiscoverAccounts     ! customer_id       ? anchor_type        -> account_list, account_count, discovery_scope
3 SummarizeDeposits    ! as_of_date  !(account_number|customer_id)  ? account_list
                                                                -> deposit_total, deposit_count, deposit_scope
4 SummarizeCredit      ! as_of_date  !(account_number|customer_id)  ? account_list
                                                                -> card_balance, credit_utilization, credit_scope
5 SummarizeLending     ! as_of_date  !(account_number|customer_id)  ? account_list
                                                                -> loan_balance, loan_count, lending_scope
6 BuildPosition        !(account_number|customer_id)
                       ? deposit_total, card_balance, loan_balance, anchor_balance, account_count
                                                                -> net_position, coverage_ratio, position_confidence
```

The required set **overlaps** rather than being one key per step: `as_of_date` is required by four of
the six, and a single identifier covers the rest. In the canonical order the caller supplies just
`account_number` and `as_of_date`; everything else is produced upstream.

## Interchangeable identifiers

The product systems hold the accounts, so they can find an owner from an account number. The customer
hub is keyed by customer and cannot. A step declares that with `RequiresOneOf`, and **any one member
satisfies it** — the planner offers the alternatives rather than demanding a particular one.

That gives the advisor path: skip the lookup and discovery entirely, hand over the account number a
customer just read out over the phone, and the summaries still run.

| Ordering | Required |
|---|---|
| canonical (all six) | `account_number` + `as_of_date` |
| **advisor: summaries + position** | **(`account_number` or `customer_id`) + `as_of_date`** |
| `SummarizeDeposits` alone | (`account_number` or `customer_id`) + `as_of_date` |
| `BuildPosition` alone | (`account_number` or `customer_id`) |
| discovery first, then summaries | `customer_id` + `as_of_date` |
| `DiscoverAccounts` alone | `customer_id` |
| lookup first, then summaries | `account_number` + `as_of_date` |

The group is satisfied without a second ask when an identifier is already in play: produced by a prior
step (the lookup's `customer_id`), or already requested by one (discovery pins it to `customer_id`,
and the summaries reuse that). The form offers a toggle between the alternatives, defaulting to the
account number.

Both identifiers resolve to the same customer through one shared derivation, so **which one you use
does not change the figures** — only whether you had to run the lookup first.

| Step | System | Duration | Tier |
|---|---|---|---|
| `BuildPosition` | Reporting | 2–4s | fast |
| `LookupAnchorAccount` | Core banking | 3–6s | fast |
| `SummarizeDeposits` | Deposits | 8–14s | fast |
| `DiscoverAccounts` | Customer hub | 12–20s | moderate |
| `SummarizeCredit` | Card processor | 15–25s | moderate |
| `SummarizeLending` | Loan servicing | 20–35s | long |

## Three optional modes

| Mode | Step | Without the optional input | With it |
|---|---|---|---|
| `exclusion` | `DiscoverAccounts` | lists every account it knows of | drops one account of the anchor's type, on the grounds that you already hold it |
| `scope` | the three summaries | cover the **primary account only**, and say so via `*_scope` | cover every discovered account *of the kinds that summary handles* — `SV`/`CK` for deposits, `CC` for credit, `ML`/`AL` for lending |
| `confidence` | `BuildPosition` | folds in whatever landed, reports reduced confidence | one fifth of the score per input, so a full position needs all five |

The exclusion is approximate by construction: discovery generates a synthetic account list, so the
anchor account number the caller supplied never literally appears in it. Dropping one account of the
anchor's *type* stands in for "you already have this one".

### When a step has nothing to work with

`BuildPosition` takes all of its substance from optional inputs. Run it alone and, without a guard, it
would fold nothing and report a net position of **$0.00** — a fabricated statement about a real
customer, dressed as a successful result.

A step declares `MinOptional`: how many optional inputs it needs before it can say anything at all.
`BuildPosition` declares 1. Below that:

- the planner raises the advisory from `degraded` to **`empty`** severity and the form shows a red
  banner and a ✕ against the step — *before* you run anything. The API carries this as the advisory's
  `severity`; Go callers can use the `Plan.Blocked()` helper.
- the activity refuses rather than inventing a figure, returning a non-retryable `ApplicationError` of
  type `InvalidInput`. The server records it as `RETRY_STATE_NON_RETRYABLE_FAILURE` and stops after a
  single attempt, since no number of retries will conjure a value the caller never sent.

One figure anywhere upstream clears it. The three summaries leave `MinOptional` at 0, because
primary-only really is a valid answer — narrow, and labelled as such.

## Why the ordering matters

Same six steps, same two root values, discovery moved from second to last:

```
A  LookupAnchorAccount → DiscoverAccounts → SummarizeDeposits → SummarizeCredit → SummarizeLending → BuildPosition
B  LookupAnchorAccount → SummarizeDeposits → SummarizeCredit → SummarizeLending → BuildPosition → DiscoverAccounts

data: {"account_number": "SV-4417-0092", "as_of_date": "2026-09-30"}
```

| Field | A canonical | B discovery last |
|---|---|---|
| `deposit_total` | $16,089.64 | $2,098.53 |
| `deposit_count` | 2 | 1 |
| `deposit_scope` | all-discovered | primary-only |
| `card_balance` | $4,778.29 | $4,415.87 |
| `loan_balance` | $727,815.68 | $164,885.33 |
| `net_position` | −$676,069.70 | −$126,768.04 |
| `position_confidence` | 100 | 80 |

**The required data set is identical for both** — `account_number` and `as_of_date`. Only the answer
moves. `POST /api/compare` runs both orderings against the same data and returns these rows plus five
more that also differ: `credit_utilization`, `credit_scope`, `loan_count`, `lending_scope`, and
`coverage_ratio` — twelve in all.

The money figures come from a seeded generator keyed on the identifier, so they are stable for this
account but will change if the seeding does.

## Running it

```bash
temporal server start-dev     # terminal 1
make worker                   # terminal 2
make server                   # terminal 3  ->  http://localhost:8080
```

Open <http://localhost:8080>: tick steps, reorder with the arrows, press **Get required fields**. Steps
that will run degraded at their current position get a ▲ marker and an advisory explaining what they
will miss. Press **Set as A**, reorder, then **Compare with A** to see both results side by side.

A full run is a minute or more of real waiting. To watch it move faster:

```bash
SIM_SPEED=0.05 make worker    # systems respond at 5% of their declared latency
```

`SIM_SPEED` is read by the worker and scales only the real waiting. The plan's time estimates come
from the catalog and stay at their declared, real-time values. Values above 1 stretch the latencies
rather than shrink them, and past about 1.8 a step outlasts its own timeout and fails.

**Fastest first** / **Slowest first** sort subject to prerequisites, so they never hand back an
ordering that is known to fail. A step waits on any selected step that produces something it requires,
that could supply its identifier, or — when it needs at least one optional input to say anything —
that feeds it at all. So `LookupAnchorAccount` leads and `BuildPosition` trails:

```
Fastest first  LookupAnchorAccount → SummarizeDeposits → DiscoverAccounts → SummarizeCredit → SummarizeLending → BuildPosition
Slowest first  LookupAnchorAccount → SummarizeLending → SummarizeCredit → DiscoverAccounts → SummarizeDeposits → BuildPosition
```

`DiscoverAccounts` stays free to move, because nothing requires its output and no step depends on it
for a minimum. Note where fastest-first puts it — *after* `SummarizeDeposits`, which narrows that
summary to primary-only. Sorting by speed costs you scope, visibly.

## API

| Endpoint | Behavior |
|---|---|
| `GET /api/steps` | the catalog: requires, optional, mode, durations, field metadata |
| `POST /api/plan` | `{"steps":[…]}` → required fields, **advisories**, per-step provenance, time estimates |
| `POST /api/run` | `{"steps":[…],"data":{…}}` → `422` with `missing`/`invalid`, else starts the Workflow. `?wait=true` blocks for the result |
| `GET /api/status` | `?workflow_id=…` → a Temporal Query for the running position, completed count, in-flight step, runtime advisories |
| `POST /api/compare` | `{"steps_a":[…],"steps_b":[…],"data":{…}}` → both results plus a field-level diff |

```bash
curl -s localhost:8080/api/plan -d '{"steps":["SummarizeDeposits"]}'
# required : account_number, as_of_date     <- with any_of: account_number | customer_id
# advisory : [scope] SummarizeDeposits will cover the primary deposit account only
#            — run DiscoverAccounts first to change that
```

Note what is **not** in `required`: `account_list`. An unmet optional is advice, never a demand.

`/api/run` does **not** refuse an ordering the planner marked `empty`. Advisories are advice, so the
run starts, and a starved step then fails on its own terms — `502`, with the activity's error. The
`empty` severity is there so you find out before spending the time.

`/api/status` is a Query, which needs a worker online and polling the task queue; without one it
returns `502`. Queries work against completed runs too, for as long as the namespace retains them.

Validation separates `missing` (you left it out) from `invalid` (it does not parse as its declared
type — `as_of_date` wants `YYYY-MM-DD`). Keys the ordering does not consume come back as `unused`, a
warning rather than an error.

## Layout

```
dsl/        the step catalog, the resolver, advisories, and validation — no Temporal dependency
workflow/   the six step activities, request validation, the DSL workflow, and the running position
web/        JSON API and the embedded single-page UI
cmd/        worker and server binaries
test/       end-to-end, behind the `integration` build tag
```

`PositionWorkflow` dispatches activities by **string name**, not by function reference — that is what
makes the step list a DSL rather than a hardcoded pipeline. Each activity gets its required inputs
plus whichever optional keys are in the bag; a missing optional is recorded as a runtime advisory
rather than an error.

### What is validated, and where

The web service and the Workflow validate independently, so a client calling the Workflow directly is
not exempt from the checks the HTTP layer would have made.

The Workflow's checks run in the `ValidateRequest` activity before any step is scheduled. The activity
returns the catalog entry for each step plus the inputs the running confidence is counted from, and
the Workflow works only from that recorded snapshot, never the catalog itself. A replay after the
catalog changes therefore runs against the catalog the request was validated with. A rejection fails
the Workflow with a non-retryable `InvalidRequest` error (`request rejected: ...`), so a Workflow retry
policy will not re-run it. Unlike the steps, `ValidateRequest` is dispatched by function reference,
not by name.

The Workflow records a `validate-request-activity` version marker (`workflow.GetVersion`, minimum and
current version both `1`) before it schedules validation. No execution predates the activity, so there
is no older code path; a history without the marker fails fast. A later change to the command
sequence raises the maximum and branches on it.

| Check | Web service | Workflow |
|---|---|---|
| Step name exists in the catalog | ✓ | ✓ |
| No duplicate steps | ✓ | ✓ |
| Ordering is non-empty | ✓ | ✓ |
| Required inputs are present | ✓ `422` | ✓ fails before scheduling |
| Interchangeable identifier present | ✓ `422` | ✓ fails before scheduling |
| Supplied values parse as their declared type | ✓ `422` | ✓ fails before scheduling |
| Unmet *optional* inputs | advisory, never a refusal | runtime advisory, never a refusal |

`ValidateRequest` visits data keys in sorted order, so two equally-malformed inputs always report the
same error.

**Two gaps neither layer closes.** Both are worth knowing before you extend the catalog:

- **Activity outputs are not checked against `Produces`.** Whatever an activity returns is merged into
  the bag as-is. Rename a field in an activity but not in the catalog and nothing errors — the declared
  field is silently absent, downstream steps see an unmet optional and quietly degrade, and the first
  visible symptom is a wrong number rather than a failure. Comparing each activity's outputs against
  its declared `Produces` would turn that into a named error at the step that caused it.
- **A catalog step with no registered activity is only discovered at run time.** Because dispatch is by
  string, adding a step to `dsl/catalog.go` and forgetting its method in `workflow/activities.go` gets
  past every check here; the failure arrives when that step is scheduled, after its retries are spent.
  `workflow/workflow_test.go` covers the current six, so the gap only bites a step added without a
  test. A startup check comparing the catalog against the worker's registry would close it properly.

Two consequences worth knowing:

**String dispatch gives up compile-time checking.** Passing a function reference lets the SDK validate
the name and argument types when the activity is scheduled. The string form skips that — the cause of
the second gap above.

**The activities are slow and do not heartbeat.** `StartToCloseTimeout` is set per step from its
declared maximum plus 30s of headroom, so loan servicing is not held to the reporting system's budget,
and the retry policy is 3 attempts with a 1s initial interval and 2× backoff — except for input
errors, which are non-retryable. But no `HeartbeatTimeout` is set and nothing calls
`activity.RecordHeartbeat`. For activities that sleep 20–45 seconds that has two costs: a worker that
dies mid-activity is not detected until the whole start-to-close window elapses, and a workflow
cancellation never reaches the running activity. For simulators on a demo that is tolerable; for real
work of this length you would heartbeat and set a `HeartbeatTimeout` well under the start-to-close.

## Testing

```bash
make check             # gofmt, go vet, and the hermetic suite
make test              # hermetic: no Temporal server needed
make test-integration  # boots a real dev server via testsuite.StartDevServer
```

`make test` needs nothing running and takes seconds: the resolver is pure, activities run in
`TestActivityEnvironment` at a tiny speed factor, and the API is exercised through `httptest` against
a fake client. The Workflow runs in `WorkflowTestSuite` against the **real** activities at that same
speed factor; a few tests substitute `env.OnActivity("<StepName>", …)` by string name where they need
to pin an exact figure or assert what a step received.

The headline claim is asserted at two levels: `workflow/workflow_test.go` runs both orderings in the
test environment and asserts the totals, scope labels, and confidence differ while the required set
does not; `test/e2e_test.go` proves the same against a real dev server.

## Changing the graph

Edit the step table in `dsl/catalog.go` — requires, optional, mode, advisory effect, duration — then
add the matching method in `workflow/activities.go`. The method name must match the step name, since
dispatch is by name.

`dsl/catalog_test.go` will tell you whether the edit holds together: it checks that every step takes
1–3 required inputs (a `RequiresOneOf` group counting as one), that a group has at least two distinct
registered members and never overlaps `Requires`, that any step which can be starved declares both a
`MinOptional` it can actually reach and the advisory for it, that every optional field is produced by some other step, that any step with
optionals declares a valid mode and an advisory effect, that all three modes stay exercised, that no
field has two producers, that durations and tiers agree, and that the canonical order still needs
nothing beyond the two roots and raises no advisories. `workflow/workflow_test.go` additionally
asserts the `Activities` struct still registers cleanly on a worker — an exported method that is not a
valid activity signature would otherwise break the worker at startup.

## License

[MIT](LICENSE). The software is provided "as is", without warranty of any kind — you use this code
at your own risk, and the author is not liable for any claim, damages, or other liability arising
from it. It is a demonstration built on synthetic data, not a production banking system.
