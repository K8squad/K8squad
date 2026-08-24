// resume_test.go — unit tests for the Story 2.11 / ISI-2527 resume timer that
// need NO Postgres: the escalation/reset rule, the equal-jitter envelope, and
// the Timer scheduling loop over a FAKE wakeSource (derivation/sleep/fire
// sequencing, the no-polling invariant, kick re-derivation).
//
// The database-backed properties (durable resume_at, SKIP LOCKED
// exactly-once, crash-restart re-derivation) live in resume_chaos_test.go
// behind -tags=chaos, the same split the spine uses.
package coord

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// planAttempt — escalation / refresh / reset
// ---------------------------------------------------------------------------

func TestPlanAttempt(t *testing.T) {
	now := time.Now()
	reset := 10 * time.Minute
	validAttempt := sql.NullInt32{Int32: 3, Valid: true}

	cases := []struct {
		name    string
		attempt sql.NullInt32
		resumed sql.NullTime
		want    int
	}{
		{"no prior row", sql.NullInt32{}, sql.NullTime{}, 1},
		{"prior pending (refresh keeps attempt)", validAttempt, sql.NullTime{}, 3},
		{"re-pause 1s after resume (streak +1)", validAttempt,
			sql.NullTime{Time: now.Add(-1 * time.Second), Valid: true}, 4},
		{"re-pause exactly at reset boundary (streak +1)", validAttempt,
			sql.NullTime{Time: now.Add(-reset), Valid: true}, 4},
		{"re-pause past reset window (fresh streak)", validAttempt,
			sql.NullTime{Time: now.Add(-reset - time.Millisecond), Valid: true}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planAttempt(tc.attempt, tc.resumed, now, reset); got != tc.want {
				t.Fatalf("planAttempt = %d, want %d", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// EqualJitter — the backoff envelope
// ---------------------------------------------------------------------------

func TestEqualJitterEnvelope(t *testing.T) {
	base := time.Second
	cap := 5 * time.Minute

	// delay(r) must sit in [exp/2, exp] for every attempt and every r.
	for attempt := 1; attempt <= 12; attempt++ {
		exp := base << uint(attempt-1)
		if exp > cap || exp <= 0 {
			exp = cap
		}
		for _, r := range []float64{0, 0.25, 0.5, 0.75, 0.999999} {
			d := EqualJitter(base, cap, attempt, r)
			if d < exp/2 || d > exp {
				t.Fatalf("attempt %d r=%v: delay %v outside [exp/2=%v, exp=%v]",
					attempt, r, d, exp/2, exp)
			}
		}
	}
}

func TestEqualJitterDoublingAndCap(t *testing.T) {
	base, cap := time.Second, 4*time.Second
	// r=0 → exactly exp/2: the doubling ladder and the cap are observable.
	for attempt, want := range map[int]time.Duration{
		1: 500 * time.Millisecond,
		2: time.Second,
		3: 2 * time.Second,
		4: 2 * time.Second, // base·2^3 = 8s capped to 4s → exp/2 = 2s
	} {
		if got := EqualJitter(base, cap, attempt, 0); got != want {
			t.Fatalf("attempt %d r=0: got %v want %v (exp/2)", attempt, got, want)
		}
	}
	// huge attempt must not overflow — it floors at the cap.
	if got := EqualJitter(base, cap, 63, 0); got != 2*time.Second {
		t.Fatalf("attempt 63 r=0: got %v, want cap/2 = 2s", got)
	}
	if got := EqualJitter(base, cap, 1, 1.0); got != time.Second {
		t.Fatalf("attempt 1 r=1: got %v, want exp (base) = 1s", got)
	}
	// attempt < 1 is clamped to 1.
	if got := EqualJitter(base, cap, 0, 0.5); got < 500*time.Millisecond || got > time.Second {
		t.Fatalf("attempt 0 clamped: got %v, want within [500ms, 1s]", got)
	}
}

// ---------------------------------------------------------------------------
// Timer over a fake wakeSource — derivation, fire, kick, no-polling
// ---------------------------------------------------------------------------

// fakeClock is a scripted clock: the Timer's sleep hook registers a wait at a
// virtual deadline and the TEST advances time explicitly via Set, which fires
// every registered wait whose deadline has been reached. Fully synchronous —
// no goroutine races between sleeping and advancing.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []clockWait
}

type clockWait struct {
	deadline time.Time
	ch       chan time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) sleepCh(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waits = append(c.waits, clockWait{deadline: c.now.Add(d), ch: ch})
	return ch
}

// Set advances the virtual clock to t and fires every wait due at or before
// t. Returns the number of waits fired.
func (c *fakeClock) Set(t time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.Before(c.now) {
		return 0 // virtual time never flows backward
	}
	c.now = t
	fired := 0
	kept := c.waits[:0]
	for _, w := range c.waits {
		if !w.deadline.After(t) {
			w.ch <- t
			fired++
		} else {
			kept = append(kept, w)
		}
	}
	c.waits = kept
	return fired
}

// pendingWaits reports how many sleeps are currently registered (the "is the
// timer actually sleeping on a deadline" probe).
func (c *fakeClock) pendingWaits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waits)
}

// fakeSource scripts nextWake/resumeDue and counts calls.
type fakeSource struct {
	mu       sync.Mutex
	pending  []time.Time // pending resume_at values, sorted
	wakeN    int
	dueN     int
	nowFn    func() time.Time
	firedDue [][]DuePause
}

func (f *fakeSource) NextWake(ctx context.Context) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakeN++
	if len(f.pending) == 0 {
		return time.Time{}, false, nil
	}
	return f.pending[0], true, nil
}

func (f *fakeSource) ResumeDue(ctx context.Context) ([]DuePause, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dueN++
	var out []DuePause
	kept := f.pending[:0]
	now := f.nowFn()
	for _, at := range f.pending {
		if now.Before(at) {
			kept = append(kept, at)
			continue
		}
		out = append(out, DuePause{ResumeAt: at})
	}
	f.pending = kept
	return out, nil
}

func (f *fakeSource) stats() (wakes, dues int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wakeN, f.dueN
}

func (f *fakeSource) fireCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.firedDue)
}

// addPending inserts at (the index keeping pending sorted ascending, so
// nextWake's pending[0] is the earliest — matching the SQL ORDER BY).
func (f *fakeSource) addPending(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := len(f.pending)
	for i > 0 && f.pending[i-1].After(at) {
		i--
	}
	f.pending = append(f.pending, time.Time{})
	copy(f.pending[i+1:], f.pending[i:])
	f.pending[i] = at
}

// newFakeTimer wires a Timer over the fake source and scripted clock. OnDue
// recording happens in the callback — the test observes the full loop.
func newFakeTimer() (*Timer, *fakeSource, *fakeClock) {
	fc := &fakeClock{now: time.Unix(1_000_000, 0)}
	fs := &fakeSource{nowFn: fc.Now}
	tm := &Timer{wakeLoop: wakeLoop[DuePause]{
		store: fs,
		OnDue: func(ctx context.Context, due []DuePause) {
			fs.mu.Lock()
			defer fs.mu.Unlock()
			fs.firedDue = append(fs.firedDue, due)
		},
		kick:  make(chan struct{}, 1),
		now:   fc.Now,
		sleep: func(ctx context.Context, d time.Duration) <-chan time.Time { return fc.sleepCh(d) },
	}}
	return tm, fs, fc
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// The core scheduling proof: ONE pending pause → exactly ONE derivation, ONE
// sleep targeting exactly the deadline, ONE fire, then the loop goes idle
// with no further derivations (no polling).
func TestTimerSingleWake(t *testing.T) {
	tm, fs, fc := newFakeTimer()

	deadline := fc.Now().Add(500 * time.Millisecond)
	fs.addPending(deadline)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- tm.Run(ctx) }()

	// It derives the wake and starts sleeping — BEFORE any time passes.
	waitFor(t, 2*time.Second, func() bool { return fc.pendingWaits() == 1 })
	if w, _ := fs.stats(); w != 1 {
		t.Fatalf("nextWake=%d before any time passed, want 1", w)
	}

	// Advance exactly to the deadline: the single wake fires.
	if n := fc.Set(deadline); n != 1 {
		t.Fatalf("Set(deadline) fired %d waits, want 1", n)
	}
	waitFor(t, 2*time.Second, func() bool { return fs.fireCount() == 1 })
	if fc.Now() != deadline {
		t.Fatalf("clock at %v, want exactly the deadline %v", fc.Now(), deadline)
	}

	// Idle after the fire: one re-derivation (the loop's next pass), then the
	// timer must NOT register another sleep (nothing pending) and must NOT
	// touch the source again.
	waitFor(t, 2*time.Second, func() bool {
		w, _ := fs.stats()
		return w >= 2
	})
	time.Sleep(50 * time.Millisecond)
	if w, d := fs.stats(); w != 2 || d != 1 {
		t.Fatalf("after fire: nextWake=%d resumeDue=%d, want exactly 2/1 — a poller would keep growing", w, d)
	}
	if n := fc.pendingWaits(); n != 0 {
		t.Fatalf("idle timer registered %d sleeps with nothing paused, want 0", n)
	}
}

// Nothing paused: the timer must perform exactly ONE derivation and then
// block — zero source activity while idle (the strongest no-polling
// statement: with no wake pending there is nothing to poll toward).
func TestTimerIdleNoQueries(t *testing.T) {
	tm, fs, fc := newFakeTimer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- tm.Run(ctx) }()

	time.Sleep(75 * time.Millisecond)
	if w, d := fs.stats(); w != 1 || d != 0 {
		t.Fatalf("idle timer: nextWake=%d resumeDue=%d, want 1/0", w, d)
	}
	if n := fc.pendingWaits(); n != 0 {
		t.Fatalf("idle timer sleeping on %d waits with nothing paused", n)
	}
	cancel()
	<-errc
}

// A kick between derivation and deadline forces an immediate re-derivation:
// an earlier pause landing out-of-band must not wait out the stale, later
// deadline the timer is currently sleeping toward.
func TestTimerKickReDerives(t *testing.T) {
	tm, fs, fc := newFakeTimer()

	later := fc.Now().Add(10 * time.Second)
	fs.addPending(later)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- tm.Run(ctx) }()

	// Derive + start the long sleep.
	waitFor(t, 2*time.Second, func() bool { return fc.pendingWaits() == 1 })

	// An EARLIER pause lands out-of-band; the control plane kicks. Time has
	// NOT reached either deadline yet.
	earlier := fc.Now().Add(100 * time.Millisecond)
	fs.addPending(earlier)
	tm.Notify()

	// The kick wakes the loop; it re-derives (nextWake returns `earlier`
	// first — ORDER BY resume_at) and re-sleeps on the EARLIER deadline. The
	// first, stale sleep is abandoned (its wait stays registered in the fake
	// but its deadline is never reached) — pendingWaits is now 2: stale +
	// earlier.
	waitFor(t, 2*time.Second, func() bool {
		w, _ := fs.stats()
		return w >= 2
	})
	waitFor(t, 2*time.Second, func() bool { return fc.pendingWaits() == 2 })

	// Advance only to the earlier deadline: exactly one wait fires — the one
	// sleeping toward the earlier deadline — and the wake fires having NEVER
	// reached the later one.
	if n := fc.Set(earlier); n != 1 {
		t.Fatalf("Set(earlier) fired %d waits, want 1 (the re-derived earlier deadline)", n)
	}
	waitFor(t, 2*time.Second, func() bool { return fs.fireCount() == 1 })
	if fs.fireCount() != 1 {
		t.Fatal("wake did not fire at the earlier deadline")
	}
	fs.mu.Lock()
	resumed := fs.firedDue[0]
	fs.mu.Unlock()
	if len(resumed) != 1 || !resumed[0].ResumeAt.Equal(earlier) {
		t.Fatalf("fired %v, want the earlier deadline only", resumed)
	}
	if fc.Now().Equal(later) {
		t.Fatal("virtual time reached the later deadline — kick did not re-derive")
	}
}

// planDelay honours Retry-After verbatim and falls back to jitter only in its
// absence (the "no fallback" is about the RUN, not the delay source).
func TestPlanDelay(t *testing.T) {
	s := &ResumeStore{cfg: ResumeConfig{BackoffBase: time.Second, BackoffCap: time.Minute, BackoffReset: time.Minute}, rand: func() float64 { return 0.5 }}
	ra := 7 * time.Second
	if got := s.planDelay(1, &ra); got != 7*time.Second {
		t.Fatalf("Retry-After present: delay %v, want 7s", got)
	}
	if got := s.planDelay(3, nil); got != 3*time.Second {
		t.Fatalf("no Retry-After attempt 3 r=0.5: delay %v, want 3s (equal jitter: exp=4s → 2s + 0.5·2s)", got)
	}
	zero := time.Duration(0)
	if got := s.planDelay(1, &zero); got != 750*time.Millisecond {
		t.Fatalf("zero Retry-After treated as absent: delay %v, want 750ms", got)
	}
}

func TestResumeConfigValidation(t *testing.T) {
	valid := ResumeConfig{Pause: "p", BackoffBase: time.Second, BackoffCap: time.Minute, BackoffReset: time.Minute}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := []ResumeConfig{
		{},
		{Pause: "p"},
		{Pause: "p", BackoffBase: -time.Second, BackoffCap: time.Minute, BackoffReset: time.Minute},
		{Pause: "p", BackoffBase: time.Minute, BackoffCap: time.Second, BackoffReset: time.Minute}, // cap < base
	}
	for i, cfg := range bad {
		if err := cfg.validate(); err == nil {
			t.Fatalf("invalid config %d accepted: %+v", i, cfg)
		}
	}
	if _, err := NewResumeStore(nil, valid, nil); err == nil {
		t.Fatal("nil db accepted")
	}
}
