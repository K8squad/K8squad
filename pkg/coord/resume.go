// resume.go — the §8 tier-2 scheduled resume for Paused(rate_limited)
// (Story 2.11 / ISI-2527): the control plane sets resume_at = now + Retry-After
// and a SINGLE durable wake fires at that instant. No fallback, no polling.
//
// The three properties this file is load-bearing for:
//
//   - SINGLE DURABLE WAKE. resume_at is persisted ONCE at pause time
//     (Pause). The timer never polls the pause table on an interval: between
//     the wake-derivation and the deadline, the timer performs ZERO database
//     reads (Stats proves this in the chaos gate — an idle-but-pending timer's
//     query counters are frozen). The next wake is re-derived from the durable
//     resume_at only at (a) start, (b) after a wake fires, (c) on an
//     out-of-band Notify — the kick a control plane sends when a NEW pause
//     lands with an earlier deadline than the one being slept on.
//
//   - CRASH-SAFE RE-DERIVATION. The timer holds NO in-memory schedule: its
//     only state is "what does the durable pause table say is next". A process
//     that dies mid-sleep and restarts re-derives the identical wake time from
//     the persisted resume_at (TestSpineResumeCrashRestart). The wake itself
//     is exactly-once per pause episode: ResumeDue claims due rows with
//     FOR UPDATE SKIP LOCKED and stamps resumed_at in the same statement, so
//     two concurrent timers (or two controller replicas) can never resume the
//     same episode twice (TestSpineResumeConcurrentExactlyOnce).
//
//   - NO RETRY-AFTER → EXPONENTIAL BACKOFF WITH JITTER. When the provider
//     429s without a Retry-After header, Pause computes the delay as equal
//     jitter (AWS style) over base·2^(attempt-1), capped: delay ∈ [exp/2, exp]
//     with exp = min(cap, base·2^(attempt-1)). The attempt escalates across
//     CONSECUTIVE pause episodes (a re-pause within BackoffReset of the
//     previous resume is the same streak → attempt+1); a pause after a quiet
//     BackoffReset window starts a fresh streak at attempt 1. While an episode
//     is still pending, a re-signalled pause (provider refreshed its
//     Retry-After) rewrites resume_at in place and keeps the attempt — it is
//     the same episode, not an escalation.
//
// # Schema binding
//
// Same discipline as coord.go: the statements are parameterised only by a
// ResumeConfig binding (table name + policy), and the ONLY schema they are
// chaos-proven against in THIS story is the self-contained int-keyed harness
// schema NewResumeForTest provisions. Production binding (uuid-keyed coord
// pause state, Run.status.resumeAt reconciliation, LISTEN/NOTIFY kick instead
// of the in-process channel) lands with Story 3.7 / ISI-2531, the reconciler
// that moves Runs to Paused(rate_limited) on the Story 5.10 shim signal.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// ResumeConfig binds the resume statements to a concrete schema and pins the
// backoff policy. Table/column names are trusted, code-supplied identifiers —
// interpolated into statement text; every VALUE travels as a bound parameter.
type ResumeConfig struct {
	// Pause is the (schema-qualified) pause-episode table: one row per work
	// item, rewritten in place (the claim-row discipline — see coord.go).
	Pause string

	// BackoffBase is the delay for attempt 1 when no Retry-After was supplied.
	BackoffBase time.Duration

	// BackoffCap ceilings the exponential growth: exp = min(cap, base·2^n).
	BackoffCap time.Duration

	// BackoffReset is the quiet window after a resume that ends an escalation
	// streak: a new pause landing later than this starts back at attempt 1.
	BackoffReset time.Duration

	// ResumeBatch is the maximum number of due episodes one wake claims
	// (ResumeDue LIMIT). 0 means unlimited.
	ResumeBatch int
}

// DefaultResumeConfig is the v1 policy: 1s base, 5m cap, 10m streak-reset
// window, batch of 64 — Retry-After dominates whenever the provider sends it;
// these only bind when it does not.
func DefaultResumeConfig() ResumeConfig {
	return ResumeConfig{
		BackoffBase:  1 * time.Second,
		BackoffCap:   5 * time.Minute,
		BackoffReset: 10 * time.Minute,
		ResumeBatch:  64,
	}
}

// validate rejects an incomplete binding up front (fail-closed, same
// discipline as NewProdClaimer).
func (c ResumeConfig) validate() error {
	if c.Pause == "" {
		return errors.New("coord.ResumeConfig: Pause table is required")
	}
	if c.BackoffBase <= 0 || c.BackoffCap <= 0 || c.BackoffReset <= 0 {
		return fmt.Errorf("coord.ResumeConfig: BackoffBase, BackoffCap and "+
			"BackoffReset must all be > 0 (got base=%s cap=%s reset=%s)",
			c.BackoffBase, c.BackoffCap, c.BackoffReset)
	}
	if c.BackoffCap < c.BackoffBase {
		return fmt.Errorf("coord.ResumeConfig: BackoffCap (%s) < BackoffBase (%s)",
			c.BackoffCap, c.BackoffBase)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Store: the pinned SQL surface
// ---------------------------------------------------------------------------

// DuePause is one claimed (exactly-once) resume: the episode's item, its
// escalation attempt, and the durable resume_at it fired at.
type DuePause struct {
	Item       int
	Attempt    int
	ResumeAt   time.Time
	RetryAfter *time.Duration // the provider-supplied delay, nil when backoff was used
}

// PauseInfo reports the durable outcome of a Pause call.
type PauseInfo struct {
	Attempt    int
	ResumeAt   time.Time
	RetryAfter *time.Duration
}

// ResumeStore runs the resume statements against the bound schema. It holds no
// mutable Go state beyond the *sql.DB, its config and a jitter source, so its
// methods are safe for concurrent use by many goroutines.
type ResumeStore struct {
	db    *sql.DB
	cfg   ResumeConfig
	rand  func() float64 // uniform [0,1); injected so tests are deterministic
	wakes atomic.Int64   // nextWake statements executed (the no-polling proof)
	due   atomic.Int64   // resumeDue statements executed
}

// NewResumeStore binds the resume statements. rand may be nil (defaults to
// math/rand/V2-global); a deterministic source makes the jitter envelope
// exactly assertable in tests.
func NewResumeStore(db *sql.DB, cfg ResumeConfig, rand func() float64) (*ResumeStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewResumeStore: nil db")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if rand == nil {
		rand = func() float64 { return randv2() }
	}
	return &ResumeStore{db: db, cfg: cfg, rand: rand}, nil
}

// Pause durably records a Paused(rate_limited) episode and its single wake
// time. With a provider Retry-After, resume_at = clock_timestamp() + RA (the
// control plane honours the provider's window — no fallback). Without one,
// resume_at = clock_timestamp() + equalJitter(min(cap, base·2^(attempt-1))).
//
// One short transaction (the coord.go discipline): the prior episode row is
// read under FOR UPDATE (concurrent Pauses of the same item serialise), the
// attempt/delay is computed in Go — where the jitter source is injectable and
// unit-testable — and the row is rewritten in place with explicit values.
// Idempotent in the episode sense: re-signalling a STILL-PENDING episode
// refreshes resume_at (a refreshed Retry-After) and keeps the attempt; a
// re-pause after a resume escalates (or resets) the attempt.
func (s *ResumeStore) Pause(ctx context.Context, item int, retryAfter *time.Duration) (PauseInfo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PauseInfo{}, fmt.Errorf("coord.Pause: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The DB clock owns the schedule (same reason the spine guards read
	// clock_timestamp(), not now()): a pause recorded by a process whose
	// local clock is skewed still lands on the same wall the wake will be
	// claimed against.
	var priorAttempt sql.NullInt32
	var priorResumedAt sql.NullTime
	read := fmt.Sprintf(
		`SELECT attempt, resumed_at FROM %s WHERE work_item_id=$1 FOR UPDATE`, s.cfg.Pause)
	switch err := tx.QueryRowContext(ctx, read, item).Scan(&priorAttempt, &priorResumedAt); err {
	case nil, sql.ErrNoRows:
		// new episode below
	default:
		return PauseInfo{}, fmt.Errorf("coord.Pause: read prior: %w", err)
	}

	now := dbNow(ctx, tx)
	attempt := planAttempt(priorAttempt, priorResumedAt, now, s.cfg.BackoffReset)
	delay := s.planDelay(attempt, retryAfter)

	write := fmt.Sprintf(
		`INSERT INTO %s
		     (work_item_id, reason, retry_after_ms, attempt, resume_at, paused_at, resumed_at)
		 VALUES ($1, 'rate_limited', $2, $3, $4, $5, NULL)
		 ON CONFLICT (work_item_id) DO UPDATE SET
		     reason        = 'rate_limited',
		     retry_after_ms= EXCLUDED.retry_after_ms,
		     attempt       = EXCLUDED.attempt,
		     resume_at     = EXCLUDED.resume_at,
		     paused_at     = EXCLUDED.paused_at,
		     resumed_at    = NULL`,
		s.cfg.Pause)
	var raMs sql.NullInt64
	if retryAfter != nil {
		raMs = sql.NullInt64{Int64: retryAfter.Milliseconds(), Valid: true}
	}
	resumeAt := now.Add(delay)
	if _, err := tx.ExecContext(ctx, write, item, raMs, attempt, resumeAt, now); err != nil {
		return PauseInfo{}, fmt.Errorf("coord.Pause: write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PauseInfo{}, fmt.Errorf("coord.Pause: commit: %w", err)
	}
	return PauseInfo{Attempt: attempt, ResumeAt: resumeAt, RetryAfter: retryAfter}, nil
}

// NextWake returns the EARLIEST pending resume_at (resumed_at IS NULL), the
// wake the timer sleeps toward. ok=false: nothing is paused — the timer idles
// on its kick channel performing zero database reads.
func (s *ResumeStore) NextWake(ctx context.Context) (time.Time, bool, error) {
	s.wakes.Add(1)
	q := fmt.Sprintf(
		`SELECT resume_at FROM %s WHERE resumed_at IS NULL ORDER BY resume_at LIMIT 1`,
		s.cfg.Pause)
	var at time.Time
	switch err := s.db.QueryRowContext(ctx, q).Scan(&at); err {
	case nil:
		return at, true, nil
	case sql.ErrNoRows:
		return time.Time{}, false, nil
	default:
		return time.Time{}, false, fmt.Errorf("coord.NextWake: %w", err)
	}
}

// ResumeDue claims every episode whose resume_at has been reached — ONE
// statement: the due rows are selected FOR UPDATE SKIP LOCKED and stamped
// resumed_at in the same UPDATE, so concurrent timers partition the due set
// (the ClaimNext fan-out property applied to wakes) and each episode is
// resumed EXACTLY ONCE, process-crash or replica-race notwithstanding: the
// stamp is the durable marker, and a stamp is only ever written by the winner
// of the row lock.
func (s *ResumeStore) ResumeDue(ctx context.Context) ([]DuePause, error) {
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
		 RETURNING p.work_item_id, p.attempt, p.resume_at, p.retry_after_ms`,
		s.cfg.Pause, s.cfg.Pause)
	rows, err := s.db.QueryContext(ctx, q, s.cfg.batchParam())
	if err != nil {
		return nil, fmt.Errorf("coord.ResumeDue: %w", err)
	}
	defer rows.Close()

	var out []DuePause
	for rows.Next() {
		var d DuePause
		var raMs sql.NullInt64
		if err := rows.Scan(&d.Item, &d.Attempt, &d.ResumeAt, &raMs); err != nil {
			return nil, fmt.Errorf("coord.ResumeDue: scan: %w", err)
		}
		if raMs.Valid {
			ra := time.Duration(raMs.Int64) * time.Millisecond
			d.RetryAfter = &ra
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Stats returns the statement counters — the no-polling proof surface: an
// idle timer freezes these; a poller cannot.
func (s *ResumeStore) Stats() (wakes, due int64) {
	return s.wakes.Load(), s.due.Load()
}

// DB exposes the backing handle (same surface as Coordinator.DB).
func (s *ResumeStore) DB() *sql.DB { return s.db }

func (c ResumeConfig) batchParam() int {
	if c.ResumeBatch <= 0 {
		return math.MaxInt32 // unlimited (PG LIMIT NULL is not universally usable)
	}
	return c.ResumeBatch
}

// dbNow reads the DATABASE clock — the schedule's single authority.
func dbNow(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) time.Time {
	var now time.Time
	if err := q.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		panic(fmt.Errorf("coord.dbNow: %w", err)) // infra error: fail loud, spine style
	}
	return now
}

// planAttempt is the escalation/reset rule (pure — unit-tested):
//
//	no prior row                           → 1 (first episode)
//	prior pending (resumed_at NULL)        → same attempt (episode refresh)
//	prior resumed within BackoffReset      → attempt + 1 (streak continues)
//	prior resumed longer ago               → 1 (fresh streak)
func planAttempt(priorAttempt sql.NullInt32, priorResumedAt sql.NullTime, now time.Time, reset time.Duration) int {
	if !priorAttempt.Valid {
		return 1
	}
	if !priorResumedAt.Valid {
		return int(priorAttempt.Int32) // still-pending episode re-signalled: refresh, not escalate
	}
	if now.Sub(priorResumedAt.Time) <= reset {
		return int(priorAttempt.Int32) + 1
	}
	return 1
}

// planDelay picks the wake delay (pure given the injected rand):
// Retry-After when the provider sent one, equal jitter over the capped
// exponential otherwise.
func (s *ResumeStore) planDelay(attempt int, retryAfter *time.Duration) time.Duration {
	if retryAfter != nil && *retryAfter > 0 {
		return *retryAfter
	}
	return EqualJitter(s.cfg.BackoffBase, s.cfg.BackoffCap, attempt, s.rand())
}

// EqualJitter is the AWS-style bounded exponential backoff: exp = min(cap,
// base·2^(attempt-1)); delay = exp/2 + rand·exp/2, so delay ∈ [exp/2, exp] —
// never below half the exponential (thundering-herd damping without the
// near-zero tail of full jitter). attempt is clamped ≥ 1; the shift guards
// int64 overflow by flooring at the cap.
func EqualJitter(base, cap time.Duration, attempt int, r float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := uint(attempt - 1)
	if shift > 62 { // base·2^63 overflows int64 ns: treat as capped
		return cap
	}
	exp := base << shift
	if exp <= 0 || exp > cap { // overflow sign-flip or past the ceiling
		exp = cap
	}
	half := time.Duration(float64(exp) * 0.5)
	jitter := time.Duration(r * float64(half))
	return half + jitter
}

// randv2 isolates the global source so tests can inject a deterministic one.
var randv2 = rand.Float64

// ---------------------------------------------------------------------------
// Timer: the single durable wake
// ---------------------------------------------------------------------------

// Timer is the single-wake scheduler. At most one Run loop per process; across
// processes, ResumeDue's SKIP LOCKED claim keeps every wake exactly-once.
type Timer struct {
	store wakeSource
	// OnDue is invoked once per fired wake with the batch of episodes claimed
	// by THIS timer. Production (Story 3.7) re-enters the Run reconciler here.
	OnDue func(ctx context.Context, due []DuePause)

	kick chan struct{}
	// sleep/now are injection points for the unit-tested fake clock; nil
	// means wall clock.
	sleep func(ctx context.Context, d time.Duration) <-chan time.Time
	now   func() time.Time
}

// wakeSource is exactly the two statements the timer needs — an interface so
// the scheduling loop is unit-testable without Postgres (the chaos gate binds
// it to ResumeStore).
type wakeSource interface {
	NextWake(ctx context.Context) (time.Time, bool, error)
	ResumeDue(ctx context.Context) ([]DuePause, error)
}

// NewTimer wires a Timer over a ResumeStore.
func NewTimer(s *ResumeStore, onDue func(context.Context, []DuePause)) *Timer {
	return &Timer{store: s, OnDue: onDue, kick: make(chan struct{}, 1)}
}

// Notify kicks the timer: a new pause may have landed with an EARLIER
// resume_at than the one being slept on. Non-blocking — a pending kick
// already forces re-derivation; a dropped kick is harmless (the timer's
// current derivation still fires at or after the new deadline only if the
// new deadline is later, in which case re-derivation is not needed).
// Production wiring (Story 3.7) replaces this with LISTEN/NOTIFY.
func (t *Timer) Notify() {
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

// Run is the wake loop — the entire "no polling" contract lives here:
//
//	derive next wake  →  sleep until it (or a kick)  →  fire ONCE  →  repeat
//
// With nothing paused the loop blocks on kick alone: ZERO database reads
// while idle. A crash anywhere in the loop is safe: the next process (or the
// next replica) derives the same wake from the same durable resume_at, and
// ResumeDue's SKIP LOCKED stamp makes the fire itself exactly-once.
//
// The only degenerate case is clock skew: if the local clock reaches the
// deadline BEFORE the database clock does, resumeDue claims nothing and the
// loop re-derives a deadline still in the local past — it then sleeps the
// small skew guard rather than spinning (a bounded reconcile, not a poll).
func (t *Timer) Run(ctx context.Context) error {
	// skewGuards counts consecutive deadline fires that claimed nothing
	// because the LOCAL clock reached the deadline before the DATABASE clock
	// did. While > 0, the re-derived (locally-past) deadline sleeps the small
	// guard instead of zero — a bounded reconcile (~20/s worst case under a
	// sustained full-second skew), never a spin, and zero cost when the
	// clocks agree.
	skewGuards := 0
	for {
		next, ok, err := t.store.NextWake(ctx)
		if err != nil {
			return err
		}

		var timerC <-chan time.Time
		var kickReceived bool
		if ok {
			d := t.until(next)
			if d <= 0 && skewGuards > 0 {
				d = 50 * time.Millisecond // the skew guard (see above)
			}
			skewGuards = 0
			timerC = t.sleeper()(ctx, d)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.kick:
			kickReceived = true
		case <-timerC:
			// the durable deadline: fire the single wake
			due, err := t.store.ResumeDue(ctx)
			if err != nil {
				return err
			}
			if len(due) == 0 && ok {
				skewGuards = 1 // claimed nothing at a real deadline: clock skew
			}
			if len(due) > 0 && t.OnDue != nil {
				t.OnDue(ctx, due)
			}
		}
		
		// loop: re-derive only if there was a kick (or skew guard)
		// - fire resumes sleep on the same deadline (no re-derive)
		// - kick triggers immediate re-derive for earlier deadline
		if kickReceived || skewGuards > 0 {
			continue
		}
	}
}

// until converts the DB-authored deadline into a local sleep duration,
// flooring at 0 and ceiling at the skew guard when the local clock is behind
// the deadline already having passed per the DB clock's claim.
func (t *Timer) until(next time.Time) time.Duration {
	d := next.Sub(t.nowf())
	if d < 0 {
		return 0
	}
	return d
}

func (t *Timer) sleeper() func(context.Context, time.Duration) <-chan time.Time {
	if t.sleep != nil {
		return t.sleep
	}
	return func(ctx context.Context, d time.Duration) <-chan time.Time {
		c := make(chan time.Time, 1)
		go func() {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case now := <-t.C:
				c <- now
			}
		}()
		return c
	}
}

func (t *Timer) nowf() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// NewResumeForTest binds the resume statements to the self-contained
// int-keyed harness schema the chaos gate provisions (mirrors NewForTest in
// coord.go), with a short backoff policy the chaos cases can run in real
// wall-clock time. rand may be nil.
func NewResumeForTest(db *sql.DB, rand func() float64) (*ResumeStore, error) {
	ctx := context.Background()
	for _, s := range []string{
		`DROP TABLE IF EXISTS resume_pause`,
		`CREATE TABLE resume_pause (
		     work_item_id  int        PRIMARY KEY,
		     reason        text       NOT NULL,
		     retry_after_ms bigint,
		     attempt       int        NOT NULL,
		     resume_at     timestamptz NOT NULL,
		     paused_at     timestamptz NOT NULL,
		     resumed_at    timestamptz)`,
		`CREATE INDEX idx_resume_pause_pending ON resume_pause (resume_at)
		     WHERE resumed_at IS NULL`,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return nil, fmt.Errorf("coord.NewResumeForTest: provision: %w", err)
		}
	}
	cfg := DefaultResumeConfig()
	cfg.Pause = "resume_pause"
	cfg.BackoffBase = 50 * time.Millisecond
	cfg.BackoffCap = 800 * time.Millisecond
	cfg.BackoffReset = 500 * time.Millisecond
	cfg.ResumeBatch = 64
	return NewResumeStore(db, cfg, rand)
}
