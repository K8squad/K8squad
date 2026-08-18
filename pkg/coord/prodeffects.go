// prodeffects.go — the crash-safe Run reconcile machine's side-effect seam
// (reconcile.Effects) BOUND TO THE PRODUCTION SCHEMA (Story 3.1 / ISI-2655, child
// ISI-2802). Where pkg/coord/prodreconcile.go binds reconcile.Store (the durable
// step/fence the machine CAS-advances) to Postgres, this file binds reconcile.Effects
// — the side-effecting outside world the machine drives at each step: warm-pool bind
// (claiming_sandbox), A2A task dispatch (dispatching), artifact upsert (collecting),
// and the terminal transition record. reconcile/memstore.go's World is the in-memory
// model of exactly these effects the falsification asserts against; ProdEffects is
// the physical §6.4 binding of that model.
//
// The load-bearing property is AT-MOST-ONCE under re-entry (§6.4). The machine fires
// each effect BEFORE it commits the step-advance, so a crash in that window re-drives
// the effect on failover (reconcile.Options.CrashAfterEffect is exactly this window).
// Each effect therefore keys its durability on the deterministic run identifier the
// re-drive lands on, so the second invocation REATTACHES instead of re-applying:
//
//	BindSandbox → coord.sandbox_bind  (run_id PK; 0007)      — reattach, never re-provision
//	Dispatch    → coord.a2a_dispatch  (a2a_task_id PK; 0005) — reattach, never re-execute
//	Collect     → coord.artifact      (UNIQUE work_item,run,kind; 0001) — republish, never dupe
//	Terminal    → coord.audit_log     (§6.5; 0001)           — one row per committed terminal advance
//
// Physical seam (Story 3.4/3.5). The DURABLE marker is the at-most-once GUARD; the
// physical mechanism (warm-pool provision, A2A shim submit) is invoked through the
// SandboxBinder / TaskDispatcher ports. Both ports are optional: a nil binder/
// dispatcher is the LEDGER-ONLY mode — the marker (and its §6.5 audit row) is written
// but no physical call is made — so the reconcile machine's idempotency contract is
// provable and shippable now, ahead of the physical warm-pool/A2A adapters. Wiring
// the real adapters is a drop-in: the marker keeps the physical call at-most-once.
//
// Run-identity note. reconcile.RunID ("run-1") is the machine's FIXTURE run key: the
// happy-path effect calls pass it (and its per-lap suffix) verbatim (runPhase). It is
// NOT a production run id — ProdEffects is bound per Run to the real work_item_id /
// run_id uuids and keys every durable row on THOSE, rewriting the fixture task id to
// the bound run so a2a_task_id / sandbox_bind.run_id / artifact.run_id are all the
// Run's real uuid (or run_uuid#lapN across §8 retry laps). The string params carry
// only the logical shape (which lap, which artifact kind), never the physical key.
//
// Error seam. reconcile.Effects methods return nothing (the machine is level-
// triggered; a failed effect must not be read as "applied"). ProdEffects mirrors
// ProdReconcileStore: it CAPTURES the first infrastructure error in a sticky field
// and the reconcile-loop caller checks Err() after a drive, requeueing on non-nil
// rather than letting the machine advance past an effect that never committed.
package coord

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/K8squad/K8squad/pkg/reconcile"
)

// SandboxBinder is the physical warm-pool bind mechanism (Story 3.4/3.5). ProdEffects
// invokes it at most once per Run — gated by the coord.sandbox_bind marker — to
// provision/attach the run's sandbox, and stamps the returned opaque handle on the
// marker. It is a CUSTODY/execution port, not an agent-to-agent channel: it carries
// only the run identifier and returns an opaque physical handle. A nil binder selects
// ledger-only mode (marker recorded, no physical provision).
type SandboxBinder interface {
	// Bind provisions or reattaches the warm-pool sandbox for runID and returns its
	// opaque physical handle. It MUST be idempotent on runID (a re-drive reattaches).
	Bind(ctx context.Context, runID string) (sandboxRef string, err error)
}

// TaskDispatcher is the physical A2A shim submit mechanism (§10.1, Story 3.5).
// ProdEffects invokes it at most once per a2a_task_id — gated by the
// coord.a2a_dispatch marker — to start the agent execution for a Run. It is a
// CUSTODY/execution port (the sanctioned §10.1 run-execution dispatch), not an
// agent-to-agent chat channel: it carries only the deterministic task/run identifiers
// and no worker-authored content. A nil dispatcher selects ledger-only mode.
type TaskDispatcher interface {
	// Submit hands the A2A task (a2aTaskID, deterministic per §6.4) for runID to the
	// shim. It MUST be idempotent on a2aTaskID (a re-drive reattaches to the in-flight
	// task, never a second execution).
	Submit(ctx context.Context, a2aTaskID, runID string) error
}

// ProdEffects implements reconcile.Effects against the production coord schema for
// ONE Run. Like ProdReconcileStore it is constructed per Run and bound to that Run's
// work_item_id / run_id / principal; those (not the machine's fixture strings) key
// every durable marker and stamp every §6.5 audit row. It holds no mutable state
// beyond the *sql.DB, its bound identifiers, the optional physical ports, and the
// sticky error — a fresh ProdEffects reconstructs nothing, so it is safe to discard
// and rebuild on every reconcile pass (the durable markers carry the idempotency).
type ProdEffects struct {
	db          *sql.DB
	ctx         context.Context
	workItemID  string // uuid — the item every effect row belongs to
	runID       string // uuid — the deterministic run key (sandbox_bind / a2a_dispatch / artifact / audit)
	principal   string // who is driving (§6.5 provenance on every marker/audit row)
	initiatedBy string // §12.4 control-plane stamp (may be empty → NULL)

	binder     SandboxBinder  // physical warm-pool bind; nil = ledger-only
	dispatcher TaskDispatcher // physical A2A shim submit; nil = ledger-only

	err error // first infrastructure error; sticky (see the error-seam note above)
}

// NewProdEffects binds the reconcile.Effects seam to one Run's coord rows. workItemID,
// runID and principal are required (every effect is audited as some principal driving
// some Run over some item); initiatedByUserID may be empty (recorded NULL). binder and
// dispatcher are the physical mechanisms and may be nil (ledger-only mode until the
// Story 3.4/3.5 adapters land). ctx bounds every statement and every physical call.
func NewProdEffects(ctx context.Context, db *sql.DB, workItemID, runID, principal, initiatedByUserID string, binder SandboxBinder, dispatcher TaskDispatcher) (*ProdEffects, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdEffects: nil db")
	}
	if workItemID == "" || runID == "" || principal == "" {
		return nil, fmt.Errorf("coord.NewProdEffects: workItemID, runID and principal are required "+
			"(got workItemID=%q runID=%q principal=%q)", workItemID, runID, principal)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &ProdEffects{
		db:          db,
		ctx:         ctx,
		workItemID:  workItemID,
		runID:       runID,
		principal:   principal,
		initiatedBy: initiatedByUserID,
		binder:      binder,
		dispatcher:  dispatcher,
	}, nil
}

// Err returns the first infrastructure error captured by any effect, or nil. The
// reconcile-loop caller checks it after a drive and requeues on non-nil rather than
// treating a failed effect as applied (mirrors ProdReconcileStore.Err).
func (e *ProdEffects) Err() error { return e.err }

// fail records the first infrastructure error (sticky).
func (e *ProdEffects) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}

// initiator maps the optional §12.4 user id to a NULL-able driver value.
func (e *ProdEffects) initiator() any {
	if e.initiatedBy == "" {
		return nil
	}
	return e.initiatedBy
}

// BindSandbox provisions/attaches the warm-pool sandbox for the bound Run
// (claiming_sandbox, Story 3.2). keyed=true is the DURABLE path: the coord.sandbox_bind
// run_id PK makes a re-drive a reattach — the physical binder (itself run_id-keyed) is
// called at most once and its handle is stamped on first provision. keyed=false is the
// NAIVE model (no durable marker: every pass re-provisions), retained only so the
// falsification can flip it; production always passes keyed=true. The runID param is
// the machine's fixture run key and is IGNORED — the bound e.runID is authoritative.
func (e *ProdEffects) BindSandbox(runID string, keyed bool) {
	if e.err != nil {
		return
	}
	if !keyed {
		// NAIVE: no durable dedup marker — re-provision every pass (double-provision).
		if e.binder != nil {
			if _, err := e.binder.Bind(e.ctx, e.runID); err != nil {
				e.fail(fmt.Errorf("coord.ProdEffects.BindSandbox: naive bind: %w", err))
			}
		}
		return
	}

	// DURABLE: is this Run already bound? A committed marker means a prior pass
	// provisioned the sandbox — reattach with NO physical call and NO second audit row.
	var existing string
	switch err := e.db.QueryRowContext(e.ctx,
		`SELECT sandbox_ref FROM coord.sandbox_bind WHERE run_id = $1::uuid`,
		e.runID).Scan(&existing); {
	case err == nil:
		return // reattach: the Run's sandbox is already bound
	case !errors.Is(err, sql.ErrNoRows):
		e.fail(fmt.Errorf("coord.ProdEffects.BindSandbox: lookup: %w", err))
		return
	}

	// First provision: call the physical binder (if wired), then record the marker.
	// The ON CONFLICT DO NOTHING closes the race with a concurrent same-fence leader —
	// the loser's marker no-ops and, because the physical bind is itself run_id-keyed,
	// its bind reattaches rather than provisioning a second sandbox.
	sandboxRef := ""
	if e.binder != nil {
		ref, err := e.binder.Bind(e.ctx, e.runID)
		if err != nil {
			e.fail(fmt.Errorf("coord.ProdEffects.BindSandbox: bind: %w", err))
			return
		}
		sandboxRef = ref
	}
	if _, err := e.db.ExecContext(e.ctx, `
		INSERT INTO coord.sandbox_bind (run_id, work_item_id, sandbox_ref, bound_by)
		     VALUES ($1::uuid, $2::uuid, $3, $4)
		ON CONFLICT (run_id) DO NOTHING`,
		e.runID, e.workItemID, sandboxRef, e.principal); err != nil {
		e.fail(fmt.Errorf("coord.ProdEffects.BindSandbox: marker: %w", err))
		return
	}
	e.audit("sandbox_bound", reconcile.StepClaimingSandbox, nil)
}

// Dispatch submits the A2A task to the shim (dispatching, §6.4/§10.1). dedup=true is
// the DURABLE path: the machine's fixture taskID is rewritten to the bound Run
// (e.runID, plus any #lapN retry suffix), and the coord.a2a_dispatch a2a_task_id PK
// makes a re-drive a reattach — the physical dispatcher is called at most once.
// dedup=false is the NAIVE model (submit afresh every pass); production always passes
// dedup=true.
func (e *ProdEffects) Dispatch(taskID string, dedup bool) {
	if e.err != nil {
		return
	}
	a2aTaskID := e.boundTaskID(taskID)
	if !dedup {
		// NAIVE: no durable marker — a fresh submit each pass (double-dispatch).
		if e.dispatcher != nil {
			if err := e.dispatcher.Submit(e.ctx, a2aTaskID, e.runID); err != nil {
				e.fail(fmt.Errorf("coord.ProdEffects.Dispatch: naive submit: %w", err))
			}
		}
		return
	}

	// DURABLE get-or-create: the a2a_task_id PK is the §6.4 dedup guard. RowsAffected==1
	// means we won the insert (first dispatch); 0 means the task is already in flight —
	// reattach with no second submit and no second audit row.
	res, err := e.db.ExecContext(e.ctx, `
		INSERT INTO coord.a2a_dispatch (a2a_task_id, work_item_id, run_id)
		     VALUES ($1, $2::uuid, $3::uuid)
		ON CONFLICT (a2a_task_id) DO NOTHING`,
		a2aTaskID, e.workItemID, e.runID)
	if err != nil {
		e.fail(fmt.Errorf("coord.ProdEffects.Dispatch: marker: %w", err))
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		e.fail(fmt.Errorf("coord.ProdEffects.Dispatch: rows: %w", err))
		return
	}
	if n == 0 {
		return // reattach: the task is already in flight for this run/lap
	}
	if e.dispatcher != nil {
		if err := e.dispatcher.Submit(e.ctx, a2aTaskID, e.runID); err != nil {
			e.fail(fmt.Errorf("coord.ProdEffects.Dispatch: submit: %w", err))
			return
		}
	}
	e.audit("a2a_dispatched", reconcile.StepDispatching, nil)
}

// Collect registers an artifact (collecting, §6.1). upsert=true is the DURABLE path:
// a content-addressed row keyed by UNIQUE(work_item_id, run_id, kind) — a re-entry
// republishes the same key idempotently (ON CONFLICT DO NOTHING), never a duplicate.
// The kind is the trailing path segment of the machine's key ("run-1/patch" → "patch");
// the content is content-addressed (uri = "sha256:<hex>", sha256 = hash(content)).
// upsert=false is the NAIVE model (append a fresh dupe under a uniquified kind);
// production always passes upsert=true.
func (e *ProdEffects) Collect(key, content string, upsert bool) {
	if e.err != nil {
		return
	}
	kind := artifactKind(key)
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	uri := "sha256:" + sha

	if !upsert {
		// NAIVE: append a distinct row every pass (models World's key#N dupes). A
		// uniquified kind sidesteps the UNIQUE guard so the naive double-collect is
		// observable rather than an error.
		var n int
		if err := e.db.QueryRowContext(e.ctx,
			`SELECT count(*) FROM coord.artifact WHERE work_item_id=$1::uuid AND run_id=$2::uuid`,
			e.workItemID, e.runID).Scan(&n); err != nil {
			e.fail(fmt.Errorf("coord.ProdEffects.Collect: naive count: %w", err))
			return
		}
		if _, err := e.db.ExecContext(e.ctx, `
			INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256)
			     VALUES ($1::uuid, $2::uuid, $3, $4, $5)`,
			e.workItemID, e.runID, fmt.Sprintf("%s#%d", kind, n), uri, sha); err != nil {
			e.fail(fmt.Errorf("coord.ProdEffects.Collect: naive insert: %w", err))
		}
		return
	}

	// DURABLE: content-addressed upsert. DO NOTHING is pure idempotency — the key is
	// content-addressed, so a re-entry carries identical (uri, sha256) and nothing
	// needs updating. RowsAffected==1 → first publish (audit it); 0 → re-publish no-op.
	res, err := e.db.ExecContext(e.ctx, `
		INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256)
		     VALUES ($1::uuid, $2::uuid, $3, $4, $5)
		ON CONFLICT (work_item_id, run_id, kind) DO NOTHING`,
		e.workItemID, e.runID, kind, uri, sha)
	if err != nil {
		e.fail(fmt.Errorf("coord.ProdEffects.Collect: upsert: %w", err))
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		e.fail(fmt.Errorf("coord.ProdEffects.Collect: rows: %w", err))
		return
	}
	if n == 1 {
		e.audit("artifact_registered", reconcile.StepCollecting, nil)
	}
}

// Terminal records a terminal transition (succeeded/failed/cancelled) as a §6.5 audit
// row. The machine calls Terminal only when the step-advance INTO the terminal step
// committed (runPhase: `if s.Advance(...) && IsTerminal(next)`), and Advance is the
// exactly-once serialization point — so a plain append is at-most-once per committed
// terminal advance (no separate dedup marker needed).
func (e *ProdEffects) Terminal(s reconcile.Step) {
	if e.err != nil {
		return
	}
	e.audit("run_terminal", s, nil)
}

// audit appends one §6.5 provenance row for an effect. to carries the effect's target
// step (the sandbox/dispatch/collect step, or the terminal step); from is optional.
func (e *ProdEffects) audit(eventType string, to reconcile.Step, from *reconcile.Step) {
	var fromArg any
	if from != nil {
		fromArg = string(*from)
	}
	if _, err := e.db.ExecContext(e.ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal,
		        initiated_by_user_id, from_state, to_state)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6, $7)`,
		e.workItemID, e.runID, eventType, e.principal, e.initiator(),
		fromArg, string(to)); err != nil {
		e.fail(fmt.Errorf("coord.ProdEffects.audit(%s): %w", eventType, err))
	}
}

// boundTaskID rewrites the machine's fixture task id (reconcile.RunID, optionally with
// a #lapN retry suffix) onto the bound Run: the run's real uuid carries the same lap
// shape, so a2a_task_id is unique per Run/lap in production instead of colliding on
// the shared fixture constant. A task id that does not carry the fixture prefix is
// namespaced under the run to stay collision-free.
func (e *ProdEffects) boundTaskID(taskID string) string {
	if taskID == reconcile.RunID {
		return e.runID
	}
	if suffix := strings.TrimPrefix(taskID, reconcile.RunID); suffix != taskID {
		return e.runID + suffix // e.g. run_uuid#lap2
	}
	return e.runID + "/" + taskID
}

// artifactKind derives the artifact kind from the machine's content-addressed key:
// the trailing path segment ("run-1/patch" → "patch"), or the whole key if unslashed.
func artifactKind(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 && i+1 < len(key) {
		return key[i+1:]
	}
	return key
}

// Compile-time proof that ProdEffects satisfies the reconcile.Effects seam the machine
// drives — if the interface grows a method, this fails to build here, at the binding.
var _ reconcile.Effects = (*ProdEffects)(nil)
