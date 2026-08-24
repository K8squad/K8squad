// sweeper.go — the §6.2 background crash-safe reclaim sweeper (Story 2.4 /
// ISI-3104). Where §6.2 reclaim is otherwise OPPORTUNISTIC — an expired lease is
// only returned to the pool on the NEXT Acquire/ClaimNext that happens to guard on
// `lease_expires_at < clock_timestamp()` (coord.go acquireTx) — this file adds the
// PERIODIC scan that reclaims a dead holder's item even when no claimant comes
// along: the §5.3 crash-recovery path a dead Run depends on when nothing else is
// contending for its work.
//
// The three properties this file is load-bearing for:
//
//   - FENCE-BEFORE-RELEASE, IN ONE STATEMENT. ReclaimExpired reclaims a whole
//     batch of expired claims in a SINGLE data-modifying CTE that co-commits three
//     facts that must never diverge (the coord.go / prodreconcile.go discipline):
//     the fence bump (fence_token+1, fencing the dead holder's stale-fence writes),
//     the reclaim_fenced_at stamp (§6.3 marker) + holder/lease release, and the
//     work_item → open transition that returns the item to the pool. Either all
//     three commit for an item or none do — there is no observable moment where the
//     item is open but its stale holder's fence still validates (AC: "a resurrected
//     stale holder CANNOT complete or clobber the reclaimed item").
//
//   - CRASH-SAFE, STATELESS RE-DERIVATION. The sweeper holds NO in-memory schedule
//     or cursor: every cycle re-derives the entire due set from the durable claim
//     table alone (`holder IS NOT NULL AND lease_expires_at < clock_timestamp()`).
//     A process that dies mid-sweep and restarts loses nothing — the next cycle (in
//     the same or a replacement process) simply re-scans and reclaims whatever is
//     still expired. Lease boundaries are compared against clock_timestamp() (the
//     live wall clock, never now() frozen at statement start), so a scan that blocks
//     on a row lock past a lease still reclaims correctly.
//
//   - EXACTLY-ONCE ACROSS REPLICAS. The due rows are selected FOR UPDATE OF claim
//     SKIP LOCKED and released in the same statement, so two concurrent sweepers
//     (or two controller replicas) PARTITION the due set — the ClaimNext fan-out
//     property applied to reclaim — and no expired claim is reclaimed (fence-bumped)
//     twice. After release holder is NULL, so a subsequent cycle cannot re-reclaim
//     the same item: the reclaim is idempotent.
//
// # Resource-layer fencing
//
// Like ReclaimFenced's default (no-fencer) path, the sweeper's reclaim fences a
// surviving zombie against DB writes (its fence is now stale) but not against
// out-of-band resource-layer writes. The operator wiring that owns the live
// resource layer performs the pod-kill/cordon; the sweeper returns the batch it
// reclaimed via OnReclaim so that wiring can fence each surviving holder. The
// durable DB reclaim is complete and committed before OnReclaim fires.
//
// # Schema binding
//
// Same discipline as coord.go / resume.go: the statement is parameterised only by
// a SweepConfig binding (table names, lifecycle values, batch/interval policy) and
// is chaos-proven against the self-contained int-keyed harness schema
// NewSweepForTest / freshSchema provision (the same work_item/claim tables the
// spine chaos gate uses). Production binding to the uuid-keyed coord schema lands
// with the reclaim/dispatch production-wiring follow-up (see coord.go's binding
// note); until then this is the chaos-gate-proven reclaim scanner, wired over the
// same guards ReclaimFenced proves.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// SweepConfig binds the sweeper statement to a concrete schema and pins the scan
// policy. Table/column and lifecycle names are trusted, code-supplied identifiers
// — interpolated into statement text; every VALUE travels as a bound parameter.
type SweepConfig struct {
	// WorkItem / Claim are the (schema-qualified) tables the reclaim scans and
	// mutates — the same two tables the coord.Coordinator binds.
	WorkItem string
	Claim    string

	// OpenState is the lifecycle value a reclaimed item returns to (claimable
	// again); DoneState is the terminal value a reclaim must NEVER reopen (a done
	// item whose lease expired is left alone — the acquire-reopens-done guard).
	OpenState string
	DoneState string

	// Interval is how often the sweeper scans for expired leases. This is a
	// bounded reconcile poll (unlike the resume timer's single durable wake): a
	// lease can expire at any instant with no scheduled event to hang a wake on,
	// so a periodic scan is the correct — and crash-safe, stateless — model.
	Interval time.Duration

	// Batch is the maximum number of expired claims one scan reclaims (LIMIT).
	// 0 means unlimited. Bounding it keeps a single cycle's transaction short
	// under a large backlog; the next cycle picks up the remainder.
	Batch int
}

// DefaultSweepConfig is the v1 policy: scan every 10s, reclaim up to 128 expired
// claims per cycle. The interval trades reclaim latency (worst case ≈ interval
// after a lease lapses, when nothing opportunistically claims first) against scan
// load; 10s is well under any human-visible recovery budget for a dead Run.
func DefaultSweepConfig() SweepConfig {
	return SweepConfig{
		Interval: 10 * time.Second,
		Batch:    128,
	}
}

// validate rejects an incomplete binding up front (fail-closed, the NewProdClaimer
// / NewResumeStore discipline).
func (c SweepConfig) validate() error {
	if c.WorkItem == "" || c.Claim == "" {
		return errors.New("coord.SweepConfig: WorkItem and Claim tables are required")
	}
	if c.OpenState == "" || c.DoneState == "" {
		return errors.New("coord.SweepConfig: OpenState and DoneState are required")
	}
	if c.Interval <= 0 {
		return fmt.Errorf("coord.SweepConfig: Interval must be > 0 (got %s)", c.Interval)
	}
	return nil
}

func (c SweepConfig) batchParam() int {
	if c.Batch <= 0 {
		return math.MaxInt32 // unlimited (PG LIMIT NULL is not universally usable)
	}
	return c.Batch
}

// ---------------------------------------------------------------------------
// Store: the pinned reclaim statement
// ---------------------------------------------------------------------------

// Reclaimed is one item the sweeper returned to the pool: its id and the fresh
// (post-bump) fence token. The fence lets an OnReclaim consumer name the exact
// generation it fenced when it kills/cordons the surviving holder.
type Reclaimed struct {
	Item  int
	Fence int64
}

// SweepStore runs the reclaim statement against the bound schema. It holds no
// mutable Go state beyond the *sql.DB, its config and an atomic scan counter, so
// its methods are safe for concurrent use by many goroutines (each sweeper cycle,
// and each replica, opens its own statement).
type SweepStore struct {
	db   *sql.DB
	cfg  SweepConfig
	scan atomic.Int64 // ReclaimExpired statements executed (the scan-count proof)
}

// NewSweepStore binds the reclaim statement to cfg.
func NewSweepStore(db *sql.DB, cfg SweepConfig) (*SweepStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewSweepStore: nil db")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &SweepStore{db: db, cfg: cfg}, nil
}

// ReclaimExpired reclaims every claim whose lease has lapsed — ONE data-modifying
// CTE: the expired rows are selected FOR UPDATE OF claim SKIP LOCKED (concurrent
// sweepers partition the due set), the claim is fence-bumped + stamped + released,
// and the work item returns to OpenState, all co-committed. A done item is never
// reopened (state <> DoneState guard). Returns the batch of reclaimed items, or an
// error on infrastructure failure (the caller's cycle logs/surfaces it and retries
// next tick — the scan is stateless, so a failed cycle loses nothing).
func (s *SweepStore) ReclaimExpired(ctx context.Context) ([]Reclaimed, error) {
	s.scan.Add(1)
	q := fmt.Sprintf(`
		WITH expired AS (
		    SELECT c.work_item_id
		      FROM %[1]s c
		      JOIN %[2]s w ON w.id = c.work_item_id
		     WHERE c.holder_principal IS NOT NULL
		       AND c.lease_expires_at IS NOT NULL
		       AND c.lease_expires_at < clock_timestamp()
		       AND w.state <> $2
		     ORDER BY c.lease_expires_at
		     FOR UPDATE OF c SKIP LOCKED
		     LIMIT $3),
		reclaimed AS (
		    UPDATE %[1]s c
		       SET holder_principal = NULL,
		           run_id           = NULL,
		           fence_token      = c.fence_token + 1,
		           reclaim_fenced_at = clock_timestamp(),
		           lease_expires_at = NULL
		      FROM expired
		     WHERE c.work_item_id = expired.work_item_id
		    RETURNING c.work_item_id, c.fence_token)
		UPDATE %[2]s w
		   SET state = $1
		  FROM reclaimed
		 WHERE w.id = reclaimed.work_item_id
		RETURNING w.id, reclaimed.fence_token`,
		s.cfg.Claim, s.cfg.WorkItem)

	rows, err := s.db.QueryContext(ctx, q, s.cfg.OpenState, s.cfg.DoneState, s.cfg.batchParam())
	if err != nil {
		return nil, fmt.Errorf("coord.ReclaimExpired: %w", err)
	}
	defer rows.Close()

	var out []Reclaimed
	for rows.Next() {
		var r Reclaimed
		if err := rows.Scan(&r.Item, &r.Fence); err != nil {
			return nil, fmt.Errorf("coord.ReclaimExpired: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Scans returns the number of ReclaimExpired statements executed — the scan-count
// proof surface (an idle sweeper between ticks freezes it; a busy-loop cannot).
func (s *SweepStore) Scans() int64 { return s.scan.Load() }

// DB exposes the backing handle (same surface as Coordinator.DB / ResumeStore.DB).
func (s *SweepStore) DB() *sql.DB { return s.db }

// ---------------------------------------------------------------------------
// Sweeper: the background reclaim loop
// ---------------------------------------------------------------------------

// reclaimSource is exactly the one statement the loop needs — an interface so the
// loop is unit-testable without Postgres (the chaos gate binds it to SweepStore).
type reclaimSource interface {
	ReclaimExpired(ctx context.Context) ([]Reclaimed, error)
}

// Sweeper is the background reclaim loop: on each tick it reclaims the batch of
// expired claims and hands it to OnReclaim. At most one loop per process is the
// norm; across processes/replicas, ReclaimExpired's SKIP LOCKED claim keeps each
// reclaim exactly-once, so running one per replica is safe.
type Sweeper struct {
	store    reclaimSource
	interval time.Duration
	metrics  SweeperMetrics

	// OnReclaim is invoked once per cycle with the batch reclaimed by THIS cycle,
	// AFTER the durable reclaim has committed — never with an empty batch. The
	// operator wiring hooks resource-layer kill/cordon of each surviving holder
	// here (the DB fence is already committed; this fences out-of-band writes).
	OnReclaim func(ctx context.Context, reclaimed []Reclaimed)

	// OnError is invoked with any infrastructure error from a cycle. The loop does
	// NOT terminate on a cycle error (a transient DB blip must not tear down a
	// durable background sweeper) — it surfaces the error here and retries next
	// tick. nil means errors are swallowed (still counted via metrics is not
	// applicable — reclaim count only counts successes). Only ctx cancellation
	// ends Run.
	OnError func(err error)

	// ticker is the injection point for the unit-tested fake clock: it returns the
	// tick channel and a stop func. nil means a real time.Ticker at interval.
	ticker func(d time.Duration) (<-chan time.Time, func())
}

// NewSweeper wires a Sweeper over a SweepStore, scanning at the store's configured
// interval. metrics may be nil (defaults to the no-op emitter). onReclaim may be
// nil (the durable reclaim still happens; nothing is notified).
func NewSweeper(s *SweepStore, metrics SweeperMetrics, onReclaim func(context.Context, []Reclaimed)) *Sweeper {
	if metrics == nil {
		metrics = nopSweeperMetrics{}
	}
	return &Sweeper{
		store:     s,
		interval:  s.cfg.Interval,
		metrics:   metrics,
		OnReclaim: onReclaim,
	}
}

// Run is the reclaim loop:
//
//	tick → reclaim the expired batch → notify OnReclaim → repeat
//
// It runs until ctx is cancelled, then returns ctx.Err() (the Timer.Run
// discipline) — clean context cancellation with no dangling ticker. A cycle's
// infrastructure error is surfaced to OnError and the loop continues: the scan is
// stateless, so a failed cycle simply retries whatever is still expired next tick.
// A process crash mid-cycle is safe for the same reason — the successor re-derives
// the entire due set from the durable claim table.
func (sw *Sweeper) Run(ctx context.Context) error {
	tickC, stop := sw.newTicker(sw.interval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tickC:
			sw.sweepOnce(ctx)
		}
	}
}

// sweepOnce runs a single reclaim cycle and records its metrics. Broken out so the
// unit test can drive individual cycles deterministically.
func (sw *Sweeper) sweepOnce(ctx context.Context) {
	sw.metrics.IncSweepCycle()
	start := time.Now()
	reclaimed, err := sw.store.ReclaimExpired(ctx)
	sw.metrics.ObserveSweepDuration(time.Since(start).Seconds())
	if err != nil {
		if sw.OnError != nil {
			sw.OnError(err)
		}
		return
	}
	if len(reclaimed) == 0 {
		return
	}
	sw.metrics.AddSweepReclaims(len(reclaimed))
	if sw.OnReclaim != nil {
		sw.OnReclaim(ctx, reclaimed)
	}
}

func (sw *Sweeper) newTicker(d time.Duration) (<-chan time.Time, func()) {
	if sw.ticker != nil {
		return sw.ticker(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// NewSweepForTest binds the reclaim statement to the self-contained int-keyed
// harness schema the chaos gate provisions (the same work_item/claim tables
// freshSchema builds and NewForTest's Coordinator drives), with a short scan
// interval so the chaos cases run in real wall-clock time.
func NewSweepForTest(db *sql.DB) (*SweepStore, error) {
	cfg := DefaultSweepConfig()
	cfg.WorkItem = "work_item"
	cfg.Claim = "claim"
	cfg.OpenState = "open"
	cfg.DoneState = "done"
	cfg.Interval = 50 * time.Millisecond
	cfg.Batch = 128
	return NewSweepStore(db, cfg)
}
