package coord

import "testing"

// TestRunHistoriesGroupsAndOrders — runHistories groups journal rows into one
// itemHistory per Run, chronologically ordered, with createdAt/initialState taken
// from each Run's EARLIEST edge. Rows are fed deliberately out of order (and
// interleaved across runs) to prove the defensive stable sort fixes the per-run
// timeline, not the SQL ORDER BY.
func TestRunHistoriesGroupsAndOrders(t *testing.T) {
	rows := []runPhaseRow{
		// run A edges, scrambled
		{runUID: "A", toPhase: RunPhaseComplete, at: day(2026, 9, 14)},
		{runUID: "A", toPhase: RunPhaseClaiming, at: day(2026, 9, 10)},
		// run B interleaved
		{runUID: "B", toPhase: RunPhaseFailed, at: day(2026, 9, 13)},
		{runUID: "A", toPhase: RunPhaseRunning, at: day(2026, 9, 12)},
		{runUID: "B", toPhase: RunPhaseClaiming, at: day(2026, 9, 11)},
	}
	hs := runHistories(rows)
	if len(hs) != 2 {
		t.Fatalf("runHistories: got %d runs, want 2", len(hs))
	}
	// First-seen order is preserved: A then B.
	a, b := hs[0], hs[1]
	if !a.createdAt.Equal(day(2026, 9, 10)) || a.initialState != RunPhaseClaiming {
		t.Errorf("run A: createdAt=%s initial=%q, want 09-10 claiming", a.createdAt.Format("01-02"), a.initialState)
	}
	if !b.createdAt.Equal(day(2026, 9, 11)) || b.initialState != RunPhaseClaiming {
		t.Errorf("run B: createdAt=%s initial=%q, want 09-11 claiming", b.createdAt.Format("01-02"), b.initialState)
	}
	// A's edges are time-ordered after the sort → state resolves correctly.
	if got := a.stateAsOf(day(2026, 9, 13)); got != RunPhaseRunning {
		t.Errorf("run A stateAsOf 09-13 = %q, want running (edges mis-sorted?)", got)
	}
	if got := a.stateAsOf(day(2026, 9, 14)); got != RunPhaseComplete {
		t.Errorf("run A stateAsOf 09-14 = %q, want complete", got)
	}
	// Before its first edge a run does not exist.
	if got := b.stateAsOf(day(2026, 9, 10)); got != "" {
		t.Errorf("run B stateAsOf 09-10 = %q, want \"\" (pre-first-edge)", got)
	}
}

// TestFleetRunPhaseDailyBuckets — the full reconstruction over the run-phase
// chip-token set: each day's snapshot counts every Run under the phase it SITS in
// at end-of-day, zero-filling all four tokens. Mirrors the tickets daily-bucket
// test (TestReconstructStatusSnapshots) but keyed by run status. A run first
// contributes only from its earliest edge (run C joins mid-window in `running`,
// simulating a missed claiming edge — never back-dated).
func TestFleetRunPhaseDailyBuckets(t *testing.T) {
	rows := []runPhaseRow{
		{runUID: "A", toPhase: RunPhaseClaiming, at: day(2026, 9, 10)},
		{runUID: "A", toPhase: RunPhaseRunning, at: day(2026, 9, 12)},
		{runUID: "A", toPhase: RunPhaseComplete, at: day(2026, 9, 14)},
		{runUID: "B", toPhase: RunPhaseClaiming, at: day(2026, 9, 11)},
		{runUID: "B", toPhase: RunPhaseFailed, at: day(2026, 9, 13)},
		{runUID: "C", toPhase: RunPhaseRunning, at: day(2026, 9, 13)}, // earliest-known is running
	}
	snaps := reconstructStatusSnapshots(runHistories(rows), dailyInstants(day(2026, 9, 10), day(2026, 9, 14)), canonicalRunPhases)
	if len(snaps) != 5 {
		t.Fatalf("snaps: got %d, want 5", len(snaps))
	}
	// Every snapshot zero-fills all four run-phase tokens (continuous band).
	for _, s := range snaps {
		if len(s.Counts) != len(canonicalRunPhases) {
			t.Fatalf("snapshot %s should zero-fill %d phases, got %d (%v)", s.Date, len(canonicalRunPhases), len(s.Counts), s.Counts)
		}
		for _, p := range canonicalRunPhases {
			if _, ok := s.Counts[p]; !ok {
				t.Fatalf("snapshot %s missing run phase %q", s.Date, p)
			}
		}
	}
	want := []map[string]int{
		{"claiming": 1, "running": 0, "complete": 0, "failed": 0}, // 09-10: A claiming
		{"claiming": 2, "running": 0, "complete": 0, "failed": 0}, // 09-11: A+B claiming
		{"claiming": 1, "running": 1, "complete": 0, "failed": 0}, // 09-12: A→running, B claiming
		{"claiming": 0, "running": 2, "complete": 0, "failed": 1}, // 09-13: A running, B→failed, C running
		{"claiming": 0, "running": 1, "complete": 1, "failed": 1}, // 09-14: A→complete, B failed, C running
	}
	for i, s := range snaps {
		for p, n := range want[i] {
			if s.Counts[p] != n {
				t.Errorf("snapshot %s[%s] = %d, want %d (full=%v)", s.Date, p, s.Counts[p], n, s.Counts)
			}
		}
	}
}

// TestFleetRunPhaseEmpty — no journal rows still yields one zero-filled snapshot
// per day (the honest empty series, never nil), exactly like the tickets path.
func TestFleetRunPhaseEmpty(t *testing.T) {
	snaps := reconstructStatusSnapshots(runHistories(nil), dailyInstants(day(2026, 9, 10), day(2026, 9, 11)), canonicalRunPhases)
	if len(snaps) != 2 {
		t.Fatalf("empty snaps: got %d, want 2", len(snaps))
	}
	for _, s := range snaps {
		total := 0
		for _, n := range s.Counts {
			total += n
		}
		if total != 0 {
			t.Errorf("snapshot %s should be empty, got %v", s.Date, s.Counts)
		}
		if len(s.Counts) != len(canonicalRunPhases) {
			t.Errorf("snapshot %s should zero-fill %d phases, got %d", s.Date, len(canonicalRunPhases), len(s.Counts))
		}
	}
}
