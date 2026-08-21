// resumeprod.go — the PRODUCTION uuid-keyed binding of the §8 tier-2 scheduled
// resume for Paused(rate_limited) (Story 3.7 / ISI-2531, wired by ISI-2883).
//
// Where resume.go proves the resume CONTRACT (single durable wake, crash-safe
// re-derivation, exactly-once resume claim, Retry-After-or-equal-jitter
// backoff) over a self-contained int-keyed harness schema, this file binds the
// SAME statements to the production coord schema added by
// db/migrations/0009_run_pause.sql:
//
//   - keys are the REAL coord uuids (work_item_id → coord.work_item FK,
//     run_id → the Run's uid), so episodes join the claim/audit/outbox spine;
//   - the wake loop is the SAME generic wakeLoop the harness Timer runs — the
//     no-polling proof carries over by construction, ProdTimer is just the
//     uuid-keyed instantiation;
//   - the pure policy core (planAttempt escalation/reset, EqualJitter backoff,
//     the DB-clock authority) is shared verbatim from resume.go.
//
// The Go-side consumers (Story 3.7 wiring, pkg/controller/rundrive):
//
//   - the DRIVER records an episode when it observes a Run parked on the
//     paused(rate_limited) step with no pending episode (Retry-After rides the
//     5.10 shim signal when it exists; nil ⇒ equal-jitter backoff);
//   - the ProdTimer (one per operator leader) fires the single wake at
//     resume_at, claims due episodes exactly-once, re-enters the Run into
//     dispatching (guarded step move), and kicks the Run CR back into the
//     drive loop — resume "the instant the limit clears", zero wasted calls.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// ProdDuePause is one claimed (exactly-once) production resume: the episode's
// real coord identifiers, its escalation attempt, and the durable resume_at it
// fired at.
type ProdDuePause struct {
	WorkItemID string
	RunID      string
	Attempt    int
	ResumeAt   time.Time
	RetryAfter *time.Duration // the provider-supplied window, nil when backoff was used
}

// ProdResumeStore runs the resume statements against the production
// coord.run_pause schema (migration 0009). Same state discipline as ResumeStore:
// no mutable Go state beyond the *sql.DB, its config and jitter source, safe
// for concurrent use.
type ProdResumeStore struct {
	db    *sql.DB
	cfg   ResumeConfig
	rand  func() float64 // uniform [0,1); injected so tests are deterministic
	wakes atomic.Int64   // nextWake statements executed (the no-polling proof)
	due   atomic.Int64   // resumeDue statements executed
}

// DefaultProdResumeConfig pins the production binding: the coord.run_pause
// table with the v1 policy (Retry-After dominates; backoff only when the
// provider sent no window).
func DefaultProdResumeConfig() ResumeConfig {
	cfg := DefaultResumeConfig()
	cfg.Pause = "coord.run_pause"
	return cfg
}

// NewProdResumeStore binds the production resume statements. cfg.Pause must
// name the uuid-keyed episode table (DefaultProdResumeConfig is the checked-in
// shape). rand may be nil.
func NewProdResumeStore(db *sql.DB, cfg ResumeConfig, rand func() float64) (*ProdResumeStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdResumeStore: nil db")
	}
	if cfg.Pause == "" {
		return nil, errors.New("coord.NewProdResumeStore: Pause table is required (use DefaultProdResumeConfig)")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if rand == nil {
		rand = func() float64 { return randv2() }
	}
	return &ProdResumeStore{db: db, cfg: cfg, rand: rand}, nil
}

// Pause durably records (or refreshes) the Paused(rate_limited) episode for
// workItemID and its single wake time — the production twin of ResumeStore.Pause:
// same transaction shape (read prior FOR UPDATE, plan in Go, rewrite in place),
// same escalation policy, uuid keys. The Run is stamped on the episode so the
// wake joins the audit/projection spine without a work_item lookup.
func (s *ProdResumeStore) Pause(ctx context.Context, workItemID, runID string, retryAfter *time.Duration) (PauseInfo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PauseInfo{}, fmt.Errorf("coord.ProdResumeStore.Pause: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var priorAttempt sql.NullInt32
	var priorResumedAt sql.NullTime
	read := fmt.Sprintf(
		`SELECT attempt, resumed_at FROM %s WHERE work_item_id=$1::uuid FOR UPDATE`, s.cfg.Pause)
	switch err := tx.QueryRowContext(ctx, read, workItemID).Scan(&priorAttempt, &priorResumedAt); err {
	case nil, sql.ErrNoRows:
		// new (or still-pending) episode below
	default:
		return PauseInfo{}, fmt.Errorf("coord.ProdResumeStore.Pause: read prior: %w", err)
	}

	now := dbNow(ctx, tx)
	attempt := planAttempt(priorAttempt, priorResumedAt, now, s.cfg.BackoffReset)
	delay := s.planDelay(attempt, retryAfter)

	write := fmt.Sprintf(
		`INSERT INTO %s
		     (work_item_id, run_id, reason, retry_after_ms, attempt, resume_at, paused_at, resumed_at)
		 VALUES ($1::uuid, $2::uuid, 'rate_limited', $3, $4, $5, $6, NULL)
		 ON CONFLICT (work_item_id) DO UPDATE SET
		     run_id         = EXCLUDED.run_id,
		     reason         = 'rate_limited',
		     retry_after_ms = EXCLUDED.retry_after_ms,
		     attempt        = EXCLUDED.attempt,
		     resume_at      = EXCLUDED.resume_at,
		     paused_at      = EXCLUDED.paused_at,
		     resumed_at     = NULL`,
		s.cfg.Pause)
	var raMs sql.NullInt64
	if retryAfter != nil {
		raMs = sql.NullInt64{Int64: retryAfter.Milliseconds(), Valid: true}
	}
	resumeAt := now.Add(delay)
	if _, err := tx.ExecContext(ctx, write, workItemID, runID, raMs, attempt, resumeAt, now); err != nil {
		return PauseInfo{}, fmt.Errorf("coord.ProdResumeStore.Pause: write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PauseInfo{}, fmt.Errorf("coord.ProdResumeStore.Pause: commit: %w", err)
	}
	return PauseInfo{Attempt: attempt, ResumeAt: resumeAt, RetryAfter: retryAfter}, nil
}

// Pending reports the pending episode for workItemID, if one exists: the
// driver asks this before recording a fresh episode (a still-pending episode
// already has its wake; re-recording would refresh it, which is the caller's
// call only when the provider re-signalled).
func (s *ProdResumeStore) Pending(ctx context.Context, workItemID string) (resumeAt time.Time, exists bool, err error) {
	q := fmt.Sprintf(
		`SELECT resume_at FROM %s WHERE work_item_id = $1::uuid AND resumed_at IS NULL`, s.cfg.Pause)
	switch err := s.db.QueryRowContext(ctx, q, workItemID).Scan(&resumeAt); err {
	case nil:
		return resumeAt, true, nil
	case sql.ErrNoRows:
		return time.Time{}, false, nil
	default:
		return time.Time{}, false, fmt.Errorf("coord.ProdResumeStore.Pending: %w", err)
	}
}

// NextWake returns the EARLIEST pending resume_at — the wake the timer sleeps
// toward (zero database reads between derivations; Stats is the proof).
func (s *ProdResumeStore) NextWake(ctx context.Context) (time.Time, bool, error) {
	s.wakes.Add(1)
	q := fmt.Sprintf(
		`SELECT resume_at FROM %s WHERE resumed_at IS NULL ORDER BY resume_at LIMIT 1`, s.cfg.Pause)
	var at time.Time
	switch err := s.db.QueryRowContext(ctx, q).Scan(&at); err {
	case nil:
		return at, true, nil
	case sql.ErrNoRows:
		return time.Time{}, false, nil
	default:
		return time.Time{}, false, fmt.Errorf("coord.ProdResumeStore.NextWake: %w", err)
	}
}

// ResumeDue claims every episode whose resume_at has been reached — the same
// exactly-once SKIP LOCKED claim as the harness store, returning the REAL
// coord identifiers (work_item_id/run_id as text) the Story 3.7 re-entry
// dispatches on.
func (s *ProdResumeStore) ResumeDue(ctx context.Context) ([]ProdDuePause, error) {
	s.due.Add(1)
	q := fmt.Sprintf(
		`WITH due AS (
		     SELECT work_item_id FROM %s
		      WHERE resumed_at IS NULL AND resume_at <= clock_timestamp()
		      ORDER BY resume_at
		      FOR UPDATE SKIP LOCKED
		      LIMIT $1)
		 UPDATE %s p SET resumed_at = clock_timestamp()
		  FROM due WHERE p.work_item_id = due.work_item_id
		 RETURNING p.work_item_id::text, p.run_id::text, p.attempt, p.resume_at, p.retry_after_ms`,
		s.cfg.Pause, s.cfg.Pause)
	rows, err := s.db.QueryContext(ctx, q, s.cfg.batchParam())
	if err != nil {
		return nil, fmt.Errorf("coord.ProdResumeStore.ResumeDue: %w", err)
	}
	defer rows.Close()

	var out []ProdDuePause
	for rows.Next() {
		var d ProdDuePause
		var raMs sql.NullInt64
		if err := rows.Scan(&d.WorkItemID, &d.RunID, &d.Attempt, &d.ResumeAt, &raMs); err != nil {
			return nil, fmt.Errorf("coord.ProdResumeStore.ResumeDue: scan: %w", err)
		}
		if raMs.Valid {
			ra := time.Duration(raMs.Int64) * time.Millisecond
			d.RetryAfter = &ra
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// planDelay picks the wake delay (pure given the injected rand): Retry-After
// when the provider sent one, equal jitter over the capped exponential
// otherwise — identical policy to ResumeStore.planDelay.
func (s *ProdResumeStore) planDelay(attempt int, retryAfter *time.Duration) time.Duration {
	if retryAfter != nil && *retryAfter > 0 {
		return *retryAfter
	}
	return EqualJitter(s.cfg.BackoffBase, s.cfg.BackoffCap, attempt, s.rand())
}

// Stats returns the statement counters — the no-polling proof surface.
func (s *ProdResumeStore) Stats() (wakes, due int64) { return s.wakes.Load(), s.due.Load() }

// DB exposes the backing handle (same surface as ResumeStore.DB).
func (s *ProdResumeStore) DB() *sql.DB { return s.db }

// ProdTimer is the production single-wake scheduler over ProdResumeStore —
// the uuid-keyed instantiation of the SAME wakeLoop the harness Timer runs.
type ProdTimer struct {
	wakeLoop[ProdDuePause]
}

// NewProdTimer wires a ProdTimer over a ProdResumeStore.
func NewProdTimer(s *ProdResumeStore, onDue func(context.Context, []ProdDuePause)) *ProdTimer {
	return &ProdTimer{wakeLoop[ProdDuePause]{
		store: s, OnDue: onDue, kick: make(chan struct{}, 1),
	}}
}

// Compile-time proofs that both timers satisfy manager-runnable-shaped uses
// and that the prod store feeds the generic wake loop.
var _ wakeSource[ProdDuePause] = (*ProdResumeStore)(nil)
