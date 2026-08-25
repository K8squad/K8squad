//go:build chaos

// credpause_chaos_test.go — the Stories 7.4+7.6 / ISI-2898 credential
// pause/resume chaos gate (C1..C5), executed by the same spine-chaos workflow
// run as TestSpine (the workflow's -run 'TestSpine' filter matches
// TestSpineCredentialPause* unanchored). Every case runs against a REAL
// Postgres with -race on, because the properties under test ARE the durability
// properties the credential stories add on top of the 2.11/3.7 resume machine:
//
//	C1 rate_limited durable single wake     resume_at = now + Retry-After, ONE fire, exactly-once claim
//	C2 per-credential attribution           two credentials keep INDEPENDENT Retry-After windows;
//	                                        only the due one resumes (attribution is the key, 7.6)
//	C3 refresh-mode holds carry NO timer     expired/rotated/unreachable are invisible to NextWake /
//	                                        ResumeDue; ResumeOnRefresh clears them EXACTLY once (7.4)
//	C4 concurrent timers exactly-once        N due episodes, two ResumeDue racers: every credential
//	                                        resumed EXACTLY once (SKIP LOCKED partitioning)
//	C5 backoff envelope + pending refresh     no Retry-After → equal-jitter over base·2^(attempt-1);
//	                                        a re-signalled PENDING episode refreshes in place (keeps
//	                                        the attempt) and PausedSet / SelectAlternate see the hold
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
// TestSpineCredentialPause — the credential pause/resume gate entrypoint.
// ===========================================================================
func TestSpineCredentialPause(t *testing.T) {
	dsn := dsnOrFatal(t)

	t.Run("C1_rate_limited_durable_single_wake", func(t *testing.T) { credC1SingleWake(t, dsn) })
	t.Run("C2_per_credential_attribution", func(t *testing.T) { credC2Attribution(t, dsn) })
	t.Run("C3_refresh_mode_no_timer", func(t *testing.T) { credC3RefreshMode(t, dsn) })
	t.Run("C4_concurrent_exactly_once", func(t *testing.T) { credC4ExactlyOnce(t, dsn) })
	t.Run("C5_backoff_envelope_pending_refresh", func(t *testing.T) { credC5Backoff(t, dsn) })
}

// newCredSUT provisions a fresh self-contained credential_pause schema and
// store. rand is fixed at 0.5 so the equal-jitter envelope is exactly
// assertable (delay = 0.75·exp). Policy is NewCredPauseForTest's: base=50ms,
// cap=800ms, reset=500ms, batch=64.
func newCredSUT(t *testing.T, dsn string) (*coord.CredentialPauseStore, *sql.DB) {
	t.Helper()
	db := openDB(t, dsn)
	s, err := coord.NewCredPauseForTest(db, func() float64 { return 0.5 })
	if err != nil {
		t.Fatalf("NewCredPauseForTest: %v", err)
	}
	return s, db
}

func ref(id string) coord.CredentialRef {
	return coord.CredentialRef{ID: id, Principal: "seat:" + id, Class: coord.ClassClaudeOAuth}
}

// ---------------------------------------------------------------------------
// C1 — rate_limited resume_at = now + Retry-After; ONE fire; exactly-once claim.
// ---------------------------------------------------------------------------
func credC1SingleWake(t *testing.T, dsn string) {
	s, db := newCredSUT(t, dsn)
	defer db.Close()
	ctx := context.Background()

	ra := 20 * time.Millisecond
	info, err := s.PauseCredential(ctx, coord.CredPauseRequest{
		Credential: ref("ns/c1"), Reason: coord.ReasonRateLimited, RetryAfter: &ra,
		Item: "item-1", Run: "run-1",
	})
	if err != nil {
		t.Fatalf("PauseCredential: %v", err)
	}
	if info.Attempt != 1 || info.ResumeAt == nil || info.RetryAfter == nil || *info.RetryAfter != ra {
		t.Fatalf("info = %+v, want attempt 1, Retry-After %v echoed, resume_at set", info, ra)
	}

	// Before the deadline: nothing is due.
	if due, _ := s.ResumeDue(ctx); len(due) != 0 {
		t.Fatalf("ResumeDue before deadline = %v, want empty", due)
	}
	// NextWake re-derives the durable deadline from resume_at alone.
	if at, ok, err := s.NextWake(ctx); err != nil || !ok || at.IsZero() {
		t.Fatalf("NextWake = (%v,%v,%v), want a real durable deadline", at, ok, err)
	}

	time.Sleep(ra + 40*time.Millisecond)

	first, err := s.ResumeDue(ctx)
	if err != nil {
		t.Fatalf("ResumeDue: %v", err)
	}
	if len(first) != 1 || first[0].Credential != "ns/c1" || first[0].Attempt != 1 {
		t.Fatalf("first ResumeDue = %+v, want single episode ns/c1 attempt 1", first)
	}
	if first[0].RetryAfter == nil || *first[0].RetryAfter != ra {
		t.Fatalf("resumed RetryAfter = %v, want %v", first[0].RetryAfter, ra)
	}

	second, err := s.ResumeDue(ctx)
	if err != nil {
		t.Fatalf("second ResumeDue: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second ResumeDue = %v — the wake was NOT exactly-once", second)
	}
}

// ---------------------------------------------------------------------------
// C2 — two credentials keep INDEPENDENT windows; only the due one resumes. The
// attribution key is the credential (7.6), never the shared work item.
// ---------------------------------------------------------------------------
func credC2Attribution(t *testing.T, dsn string) {
	s, db := newCredSUT(t, dsn)
	defer db.Close()
	ctx := context.Background()

	soon := 20 * time.Millisecond
	late := 10 * time.Second
	// Both surfaced on the SAME work item — the ledger must still key on the
	// credential, so the windows do not merge.
	if _, err := s.PauseCredential(ctx, coord.CredPauseRequest{
		Credential: ref("ns/soon"), Reason: coord.ReasonRateLimited, RetryAfter: &soon, Item: "shared", Run: "run-soon",
	}); err != nil {
		t.Fatalf("pause soon: %v", err)
	}
	if _, err := s.PauseCredential(ctx, coord.CredPauseRequest{
		Credential: ref("ns/late"), Reason: coord.ReasonRateLimited, RetryAfter: &late, Item: "shared", Run: "run-late",
	}); err != nil {
		t.Fatalf("pause late: %v", err)
	}

	time.Sleep(soon + 40*time.Millisecond)

	due, err := s.ResumeDue(ctx)
	if err != nil {
		t.Fatalf("ResumeDue: %v", err)
	}
	if len(due) != 1 || due[0].Credential != "ns/soon" {
		t.Fatalf("due = %+v, want ONLY ns/soon — the late window must not resume early", due)
	}

	// The late credential is still held; the advisory read and the pure
	// re-route helpers see exactly it.
	paused, err := s.PausedSet(ctx)
	if err != nil {
		t.Fatalf("PausedSet: %v", err)
	}
	if _, held := paused["ns/late"]; !held || len(paused) != 1 {
		t.Fatalf("PausedSet = %v, want ONLY ns/late still held", paused)
	}
	if alt, ok := coord.SelectAlternate([]string{"ns/late", "ns/free"}, paused); !ok || alt != "ns/free" {
		t.Fatalf("SelectAlternate = (%q,%v), want it to route around the held ns/late to ns/free", alt, ok)
	}
	if hz := coord.EarliestResume(paused); hz == nil {
		t.Fatalf("EarliestResume = nil, want the late timer horizon")
	}
}

// ---------------------------------------------------------------------------
// C3 — refresh-mode holds (expired/rotated/unreachable) carry NO timer and
// clear ONLY on ResumeOnRefresh, exactly once.
// ---------------------------------------------------------------------------
func credC3RefreshMode(t *testing.T, dsn string) {
	s, db := newCredSUT(t, dsn)
	defer db.Close()
	ctx := context.Background()

	for _, reason := range []coord.CredPauseReason{
		coord.ReasonCredentialExpired, coord.ReasonCredentialRotated, coord.ReasonEndpointUnreachable,
	} {
		id := "ns/" + string(reason)
		info, err := s.PauseCredential(ctx, coord.CredPauseRequest{
			Credential: ref(id), Reason: reason, Item: "i", Run: "r",
		})
		if err != nil {
			t.Fatalf("pause %s: %v", reason, err)
		}
		if info.ResumeAt != nil || info.RetryAfter != nil {
			t.Fatalf("%s info = %+v, want NO timer (refresh-mode hold)", reason, info)
		}
	}

	// No refresh-mode hold is ever timer-visible.
	if _, ok, err := s.NextWake(ctx); err != nil || ok {
		t.Fatalf("NextWake = (ok=%v,%v), want NO durable timer wake for refresh-mode holds", ok, err)
	}
	time.Sleep(30 * time.Millisecond)
	if due, err := s.ResumeDue(ctx); err != nil || len(due) != 0 {
		t.Fatalf("ResumeDue = (%v,%v), want empty — a refresh-mode hold must never fire a timer", due, err)
	}

	// ResumeOnRefresh clears the pending hold exactly once; a re-refresh is a
	// no-op success (a rotation write-back racing a resume cannot resurrect it).
	was, err := s.ResumeOnRefresh(ctx, "ns/credential_rotated")
	if err != nil || !was {
		t.Fatalf("first ResumeOnRefresh = (%v,%v), want wasPending true", was, err)
	}
	was, err = s.ResumeOnRefresh(ctx, "ns/credential_rotated")
	if err != nil || was {
		t.Fatalf("second ResumeOnRefresh = (%v,%v), want wasPending false (idempotent)", was, err)
	}
	// Refreshing a credential with no pending episode is a no-op success.
	if was, err := s.ResumeOnRefresh(ctx, "ns/never-paused"); err != nil || was {
		t.Fatalf("ResumeOnRefresh(unknown) = (%v,%v), want no-op false", was, err)
	}
}

// ---------------------------------------------------------------------------
// C4 — N due credentials, two ResumeDue racers: every credential resumes
// EXACTLY once (SKIP LOCKED partitioning), never dropped, never doubled.
// ---------------------------------------------------------------------------
func credC4ExactlyOnce(t *testing.T, dsn string) {
	s, db := newCredSUT(t, dsn)
	defer db.Close()
	ctx := context.Background()

	const n = 40
	ra := 15 * time.Millisecond
	for i := 0; i < n; i++ {
		id := "ns/c4-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if _, err := s.PauseCredential(ctx, coord.CredPauseRequest{
			Credential: ref(id), Reason: coord.ReasonRateLimited, RetryAfter: &ra, Run: "run",
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	time.Sleep(ra + 40*time.Millisecond)

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				due, err := s.ResumeDue(ctx)
				if err != nil {
					t.Errorf("ResumeDue: %v", err)
					return
				}
				if len(due) == 0 {
					return
				}
				mu.Lock()
				for _, d := range due {
					seen[d.Credential]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Fatalf("resumed %d distinct credentials, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("credential %s resumed %d times — NOT exactly-once", id, c)
		}
	}
}

// ---------------------------------------------------------------------------
// C5 — no Retry-After → equal-jitter envelope over base·2^(attempt-1); a
// re-signalled PENDING episode refreshes in place (keeps the attempt, does not
// escalate); PausedSet reflects the current reason.
// ---------------------------------------------------------------------------
func credC5Backoff(t *testing.T, dsn string) {
	s, db := newCredSUT(t, dsn)
	defer db.Close()
	ctx := context.Background()

	// rate_limited with NO Retry-After → backoff. attempt 1, exp = base = 50ms,
	// equal-jitter envelope [25ms, 50ms]; with rand 0.5 → 37.5ms.
	info, err := s.PauseCredential(ctx, coord.CredPauseRequest{
		Credential: ref("ns/c5"), Reason: coord.ReasonRateLimited, Run: "run",
	})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if info.Attempt != 1 || info.RetryAfter == nil {
		t.Fatalf("info = %+v, want attempt 1 with a derived backoff window", info)
	}
	base := 50 * time.Millisecond
	if *info.RetryAfter < base/2 || *info.RetryAfter > base {
		t.Fatalf("backoff window %v outside equal-jitter envelope [%v,%v]", *info.RetryAfter, base/2, base)
	}

	// Re-signal the STILL-PENDING episode: refresh in place, attempt unchanged
	// (the provider re-sent a window for the SAME hold, not a new streak).
	info2, err := s.PauseCredential(ctx, coord.CredPauseRequest{
		Credential: ref("ns/c5"), Reason: coord.ReasonRateLimited, Run: "run",
	})
	if err != nil {
		t.Fatalf("re-pause: %v", err)
	}
	if info2.Attempt != 1 {
		t.Fatalf("re-signalled pending attempt = %d, want 1 (refresh, not escalate)", info2.Attempt)
	}

	// Now flip the same credential to a refresh-mode reason: one live episode
	// per credential (PK), so the advisory read reflects the NEW reason.
	if _, err := s.PauseCredential(ctx, coord.CredPauseRequest{
		Credential: ref("ns/c5"), Reason: coord.ReasonEndpointUnreachable, Run: "run",
	}); err != nil {
		t.Fatalf("flip reason: %v", err)
	}
	paused, err := s.PausedSet(ctx)
	if err != nil {
		t.Fatalf("PausedSet: %v", err)
	}
	v, held := paused["ns/c5"]
	if !held || v.Reason != coord.ReasonEndpointUnreachable || v.ResumeAt != nil {
		t.Fatalf("PausedSet[ns/c5] = %+v, want endpoint_unreachable with no timer", v)
	}
}
