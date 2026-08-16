//go:build chaos

// resume_chaos_test.go — the Story 2.11 / ISI-2527 resume-timer chaos gate
// (R1..R6), executed by the same spine-chaos workflow run as TestSpine (the
// workflow's -run 'TestSpine' filter matches TestSpineResume* unanchored).
// Every case runs against a REAL Postgres with -race on, because the
// properties under test ARE the durability properties:
//
//	R1 Retry-After durable single wake      resume_at = now + RA, ONE fire, exactly-once claim
//	R2 crash-restart re-derivation          timer dies mid-sleep; its successor re-derives the
//	                                        SAME wake from durable resume_at and fires once
//	R3 no polling                           query counters FROZEN while sleeping toward a
//	                                        deadline (an interval poller cannot freeze)
//	R4 concurrent timers exactly-once       N due episodes, two timers: every episode resumed
//	                                        EXACTLY once (SKIP LOCKED partitioning)
//	R5 kick re-derivation                   an out-of-band earlier pause + Notify beats the
//	                                        stale, later deadline being slept on
//	R6 backoff escalation / reset / refresh no Retry-After → equal-jitter envelope over
//	                                        base·2^(attempt-1), streak reset after the quiet
//	                                        window, pending-refresh keeps the attempt
package coord_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

// ===========================================================================
// TestSpineResume — the resume-timer gate entrypoint.
// ===========================================================================
func TestSpineResume(t *testing.T) {
	dsn := dsnOrFatal(t)

	t.Run("R1_retry_after_durable_single_wake", func(t *testing.T) { resumeR1SingleWake(t, dsn) })
	t.Run("R2_crash_restart_rederivation", func(t *testing.T) { resumeR2CrashRestart(t, dsn) })
	t.Run("R3_no_polling_frozen_counters", func(t *testing.T) { resumeR3NoPolling(t, dsn) })
	t.Run("R4_concurrent_timers_exactly_once", func(t *testing.T) { resumeR4ExactlyOnce(t, dsn) })
	t.Run("R5_kick_earlier_deadline", func(t *testing.T) { resumeR5Kick(t, dsn) })
	t.Run("R6_backoff_escalation_reset_refresh", func(t *testing.T) { resumeR6Backoff(t, dsn) })
}

// newResumeSUT provisions a fresh harness schema and store. rand is fixed at
// 0.5 so the jitter envelope is exactly assertable (delay = 0.75·exp).
func newResumeSUT(t *testing.T, dsn string) (*coord.ResumeStore, *sql.DB) {
	t.Helper()
	db := openDB(t, dsn)
	s, err := coord.NewResumeForTest(db, func() float64 { return 0.5 })
	if err != nil {
		t.Fatalf("NewResumeForTest: %v", err)
	}
	return s, db
}

// collector is the OnDue sink shared by (possibly many) timers.
type collector struct {
	mu      sync.Mutex
	batches [][]coord.DuePause
}

func (c *collector) onDue(ctx context.Context, due []coord.DuePause) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batches = append(c.batches, due)
}

func (c *collector) items() (flat []coord.DuePause) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.batches {
		flat = append(flat, b...)
	}
	return flat
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, b := range c.batches {
		n += len(b)
	}
	return n
}

func runTimer(t *testing.T, s *coord.ResumeStore, c *collector) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tm := coord.NewTimer(s, c.onDue)
	errc := make(chan error, 1)
	go func() { errc <- tm.Run(ctx) }()
	return cancel, errc
}

// ---------------------------------------------------------------------------
// R1 — Retry-After is the wake: resume_at = now + RA, one fire, exactly-once.
// ---------------------------------------------------------------------------
func resumeR1SingleWake(t *testing.T, dsn string) {
	s, db := newResumeSUT(t, dsn)
	ctx := context.Background()

	ra := 300 * time.Millisecond
	before := time.Now()
	info, err := s.Pause(ctx, 1, &ra)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// resume_at honours Retry-After from the DB clock.
	gotDelay := info.ResumeAt.Sub(before)
	if gotDelay < 250*time.Millisecond || gotDelay > time.Second {
		t.Fatalf("resume_at = now+%v, want ≈ now+300ms", gotDelay)
	}
	if info.Attempt != 1 || info.RetryAfter == nil || *info.RetryAfter != ra {
		t.Fatalf("PauseInfo = %+v, want attempt 1, Retry-After echoed", info)
	}

	// The durable row is the single source of the wake.
	var pending int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM resume_pause WHERE resumed_at IS NULL`).Scan(&pending); err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending episodes = %d, want 1", pending)
	}

	c := &collector{}
	cancel, errc := runTimer(t, s, c)
	defer cancel()

	waitForResume(t, 3*time.Second, func() bool { return c.count() == 1 })

	fired := c.items()
	if len(fired) != 1 || fired[0].Item != 1 || fired[0].Attempt != 1 {
		t.Fatalf("wake fired %v, want single episode item 1 attempt 1", fired)
	}
	if fired[0].RetryAfter == nil || *fired[0].RetryAfter != ra {
		t.Fatalf("fired episode RetryAfter = %v, want %v", fired[0].RetryAfter, ra)
	}

	// Exactly-once: a manual ResumeDue AFTER the timer's claim finds nothing.
	due, err := s.ResumeDue(ctx)
	if err != nil {
		t.Fatalf("ResumeDue: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("second ResumeDue returned %v — the wake was not exactly-once", due)
	}

	cancel()
	if err := <-errc; err != context.Canceled {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// R2 — crash-safety: the wake is re-derived from durable resume_at, and the
// fire is still exactly-once across the restart.
// ---------------------------------------------------------------------------
func resumeR2CrashRestart(t *testing.T, dsn string) {
	s, _ := newResumeSUT(t, dsn)
	ctx := context.Background()

	ra := 600 * time.Millisecond
	info, err := s.Pause(ctx, 7, &ra)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Timer A "crashes" (cancel) 150ms into the sleep — well before the
	// deadline, having fired nothing.
	cA := &collector{}
	cancelA, errcA := runTimer(t, s, cA)
	time.Sleep(150 * time.Millisecond)
	cancelA()
	<-errcA
	if cA.count() != 0 {
		t.Fatalf("timer A fired %d before crash, want 0", cA.count())
	}

	// Timer B restarts against the SAME durable state — no in-memory handoff.
	cB := &collector{}
	cancelB, errcB := runTimer(t, s, cB)
	defer cancelB()

	waitForResume(t, 3*time.Second, func() bool { return cB.count() == 1 })

	if got := cB.items(); len(got) != 1 || got[0].Item != 7 {
		t.Fatalf("timer B fired %v, want single episode item 7", got)
	}

	// The fire stamped the ORIGINAL durable deadline: resumed_at must sit at
	// the pause-time resume_at (a schedule re-derived from the RESTART would
	// fire a full RA after the crash — 150ms+600ms out — and land late).
	var resumedAt time.Time
	if err := s.DB().QueryRowContext(ctx,
		`SELECT resumed_at FROM resume_pause WHERE work_item_id=7`).Scan(&resumedAt); err != nil {
		t.Fatalf("read resumed_at: %v", err)
	}
	if late := resumedAt.Sub(info.ResumeAt); late < 0 || late > 400*time.Millisecond {
		t.Fatalf("resumed_at landed %v after the durable resume_at (want ≤400ms — the restart re-derived the ORIGINAL deadline)", late)
	}

	// Still exactly-once across the restart: nothing left to resume.
	due, err := s.ResumeDue(ctx)
	if err != nil {
		t.Fatalf("ResumeDue: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("post-restart ResumeDue returned %v", due)
	}
	cancelB()
	<-errcB
}

// ---------------------------------------------------------------------------
// R3 — no polling: the statement counters are FROZEN while the timer sleeps
// toward the deadline. An interval poller cannot freeze.
// ---------------------------------------------------------------------------
func resumeR3NoPolling(t *testing.T, dsn string) {
	s, _ := newResumeSUT(t, dsn)
	ctx := context.Background()

	raEarly := 400 * time.Millisecond
	if _, err := s.Pause(ctx, 1, &raEarly); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	raLate := 5 * time.Second
	if _, err := s.Pause(ctx, 2, &raLate); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	c := &collector{}
	cancel, errc := runTimer(t, s, c)
	defer cancel()

	wakes := func() int64 {
		w, _ := s.Stats()
		return w
	}

	// Sample the derivation counter twice INSIDE the sleep window. A poller
	// at even 10Hz adds dozens of reads between these samples; the single
	// durable wake adds ZERO.
	time.Sleep(120 * time.Millisecond)
	mid1 := wakes()
	time.Sleep(200 * time.Millisecond)
	mid2 := wakes()
	if mid1 != mid2 {
		t.Fatalf("nextWake count moved %d → %d while sleeping toward the deadline — the timer polls",
			mid1, mid2)
	}
	if mid1 != 1 {
		t.Fatalf("nextWake count mid-sleep = %d, want exactly 1 (the single derivation)", mid1)
	}

	waitForResume(t, 3*time.Second, func() bool { return c.count() == 1 })
	_, dueCalls := s.Stats()
	if dueCalls != 1 {
		t.Fatalf("resumeDue count after fire = %d, want 1", dueCalls)
	}

	// After the fire the loop re-derives once more (toward the 5s pause) and
	// freezes again — the counters stay put.
	time.Sleep(150 * time.Millisecond)
	if w, d := s.Stats(); w != 2 || d != 1 {
		t.Fatalf("post-fire counters nextWake=%d resumeDue=%d, want 2/1 (re-derive once, no polling)", w, d)
	}

	cancel()
	<-errc
}

// ---------------------------------------------------------------------------
// R4 — two concurrent timers, 24 due episodes: SKIP LOCKED partitions the due
// set; every episode is resumed EXACTLY once.
// ---------------------------------------------------------------------------
func resumeR4ExactlyOnce(t *testing.T, dsn string) {
	s, _ := newResumeSUT(t, dsn)
	ctx := context.Background()

	const n = 24
	ra := 1 * time.Millisecond
	for i := 0; i < n; i++ {
		if _, err := s.Pause(ctx, i+1, &ra); err != nil {
			t.Fatalf("Pause %d: %v", i+1, err)
		}
	}

	shared := &collector{}
	cancelA, errcA := runTimer(t, s, shared)
	cancelB, errcB := runTimer(t, s, shared)
	defer func() { cancelA(); cancelB(); <-errcA; <-errcB }()

	waitForResume(t, 5*time.Second, func() bool { return shared.count() == n })

	// Every episode exactly once, no duplicates, none lost.
	seen := make(map[int]int)
	for _, d := range shared.items() {
		seen[d.Item]++
	}
	dups := 0
	for item, c := range seen {
		if c != 1 {
			dups++
			t.Errorf("item %d resumed %d times (want exactly 1)", item, c)
		}
	}
	if len(seen) != n {
		t.Fatalf("distinct resumed episodes = %d, want %d", len(seen), n)
	}

	// The durable rows agree: all stamped resumed_at, none pending.
	var pending int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM resume_pause WHERE resumed_at IS NULL`).Scan(&pending); err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d episodes still pending after both timers claimed", pending)
	}
}

// ---------------------------------------------------------------------------
// R5 — a kick (out-of-band Notify) makes the timer re-derive and wake at an
// EARLIER deadline than the one it is sleeping toward.
// ---------------------------------------------------------------------------
func resumeR5Kick(t *testing.T, dsn string) {
	s, _ := newResumeSUT(t, dsn)
	ctx := context.Background()

	raLate := 5 * time.Second
	if _, err := s.Pause(ctx, 1, &raLate); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	c := &collector{}
	tm := coord.NewTimer(s, c.onDue)
	rctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- tm.Run(rctx) }()

	time.Sleep(50 * time.Millisecond) // let the timer derive + start the 5s sleep

	start := time.Now()
	raEarly := 150 * time.Millisecond
	if _, err := s.Pause(ctx, 2, &raEarly); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	tm.Notify()

	waitForResume(t, 3*time.Second, func() bool { return c.count() == 1 })

	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("earlier wake took %v — the kick did not re-derive (5s deadline governs)", elapsed)
	}
	got := c.items()
	if len(got) != 1 || got[0].Item != 2 {
		t.Fatalf("wake fired %v, want only the earlier episode (item 2)", got)
	}
	cancel()
	<-errc
}

// ---------------------------------------------------------------------------
// R6 — backoff: no Retry-After → equal jitter (r=0.5 → 0.75·exp) over the
// doubling ladder; consecutive re-pauses escalate; a pause after the reset
// window starts fresh; a refresh while STILL PENDING keeps the attempt.
// ---------------------------------------------------------------------------
func resumeR6Backoff(t *testing.T, dsn string) {
	s, _ := newResumeSUT(t, dsn)
	ctx := context.Background()
	// NewResumeForTest policy: base=50ms cap=800ms reset=500ms, rand≡0.5
	// → delay(n) = 0.75 · min(800ms, 50ms·2^(n-1)).
	wantDelay := func(attempt int) time.Duration {
		exp := 50 * time.Millisecond << uint(attempt-1)
		if exp > 800*time.Millisecond {
			exp = 800 * time.Millisecond
		}
		return exp * 3 / 4
	}

	// Escalation: four consecutive episodes (each resumed, immediately
	// re-paused within the 500ms reset window).
	for attempt := 1; attempt <= 4; attempt++ {
		info, err := s.Pause(ctx, 42, nil)
		if err != nil {
			t.Fatalf("Pause %d: %v", attempt, err)
		}
		if info.Attempt != attempt {
			t.Fatalf("episode %d: attempt = %d, want %d", attempt, info.Attempt, attempt)
		}
		d := info.ResumeAt.Sub(time.Now().UTC())
		want := wantDelay(attempt)
		if d < want-150*time.Millisecond || d > want+250*time.Millisecond {
			t.Fatalf("episode %d: backoff delay %v, want ≈ %v (equal jitter r=0.5)", attempt, d, want)
		}

		// Let the deadline pass and resume the episode before re-pausing —
		// the streak must see resumed_at within the reset window.
		time.Sleep(wantDelay(attempt) + 60*time.Millisecond)
		due, err := s.ResumeDue(ctx)
		if err != nil {
			t.Fatalf("ResumeDue %d: %v", attempt, err)
		}
		if len(due) != 1 || due[0].Item != 42 {
			t.Fatalf("ResumeDue %d returned %v", attempt, due)
		}
	}

	// Refresh while still pending: same episode → attempt kept, resume_at
	// rewritten to the new Retry-After.
	info, err := s.Pause(ctx, 42, nil)
	if err != nil {
		t.Fatalf("Pause (pending streak): %v", err)
	}
	if info.Attempt != 5 {
		t.Fatalf("pending re-pause after 4 resumes: attempt = %d, want 5", info.Attempt)
	}
	refresh := 2 * time.Second
	info2, err := s.Pause(ctx, 42, &refresh)
	if err != nil {
		t.Fatalf("Pause (refresh): %v", err)
	}
	if info2.Attempt != 5 {
		t.Fatalf("refresh of a pending episode: attempt = %d, want 5 (same episode)", info2.Attempt)
	}
	if info2.RetryAfter == nil || *info2.RetryAfter != refresh {
		t.Fatalf("refresh did not adopt the new Retry-After: %+v", info2)
	}

	// Streak reset: resume the pending episode, wait out the 500ms reset
	// window, pause again → attempt back to 1.
	time.Sleep(2*time.Second + 100*time.Millisecond) // let the refreshed deadline pass
	if due, err := s.ResumeDue(ctx); err != nil || len(due) != 1 {
		t.Fatalf("ResumeDue (refreshed): %v %v", due, err)
	}
	time.Sleep(600 * time.Millisecond) // > BackoffReset (500ms)
	final, err := s.Pause(ctx, 42, nil)
	if err != nil {
		t.Fatalf("Pause (post-reset): %v", err)
	}
	if final.Attempt != 1 {
		t.Fatalf("pause after the reset window: attempt = %d, want 1 (fresh streak)", final.Attempt)
	}
}

// waitForResume polls a condition on the test side only (the SUT never polls).
func waitForResume(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
