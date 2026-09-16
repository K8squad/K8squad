package coord

import (
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

// TestStateAsOf — an item created into backlog, moved to in_progress then done,
// reports the correct state at instants before creation, between moves, and after.
func TestStateAsOf(t *testing.T) {
	h := itemHistory{
		createdAt:    day(2026, 9, 10),
		initialState: "backlog",
		transitions: []stateAt{
			{at: day(2026, 9, 12), state: "in_progress"},
			{at: day(2026, 9, 15), state: "done"},
		},
	}
	cases := []struct {
		when time.Time
		want string
	}{
		{day(2026, 9, 9), ""},             // before it existed
		{day(2026, 9, 10), "backlog"},     // creation day, pre-move
		{day(2026, 9, 11), "backlog"},     // still backlog
		{day(2026, 9, 12), "in_progress"}, // move day counts
		{day(2026, 9, 14), "in_progress"},
		{day(2026, 9, 15), "done"},
		{day(2026, 9, 20), "done"}, // sticks after last move
	}
	for _, c := range cases {
		if got := h.stateAsOf(c.when); got != c.want {
			t.Errorf("stateAsOf(%s) = %q, want %q", c.when.Format("01-02"), got, c.want)
		}
	}
}

// TestDailyInstants — inclusive day walk, each instant is end-of-day UTC.
func TestDailyInstants(t *testing.T) {
	got := dailyInstants(day(2026, 9, 10), day(2026, 9, 12))
	if len(got) != 3 {
		t.Fatalf("instants: got %d, want 3", len(got))
	}
	wantDates := []string{"2026-09-10", "2026-09-11", "2026-09-12"}
	for i, inst := range got {
		if d := inst.Format("2006-01-02"); d != wantDates[i] {
			t.Errorf("instant[%d] date = %s, want %s", i, d, wantDates[i])
		}
		// end-of-day: a transition at 23:00 on the day is <= the instant.
		eod := time.Date(2026, 9, 10+i, 23, 0, 0, 0, time.UTC)
		if eod.After(inst) {
			t.Errorf("instant[%d] %s precedes same-day 23:00 — not end-of-day", i, inst)
		}
	}
}

// TestDailyInstantsSingleDay — from==to yields exactly one snapshot.
func TestDailyInstantsSingleDay(t *testing.T) {
	if got := dailyInstants(day(2026, 9, 10), day(2026, 9, 10)); len(got) != 1 {
		t.Fatalf("single-day instants: got %d, want 1", len(got))
	}
}

// TestReconstructStatusSnapshots — the stacked-area series over a 4-day window
// with two items whose lanes change mid-window; every canonical state is
// zero-filled so a band never disappears.
func TestReconstructStatusSnapshots(t *testing.T) {
	items := []itemHistory{
		{ // created backlog on the 10th, → in_progress on the 12th
			createdAt:    day(2026, 9, 10),
			initialState: "backlog",
			transitions:  []stateAt{{at: day(2026, 9, 12), state: "in_progress"}},
		},
		{ // created in_progress on the 11th, → done on the 13th
			createdAt:    day(2026, 9, 11),
			initialState: "in_progress",
			transitions:  []stateAt{{at: day(2026, 9, 13), state: "done"}},
		},
	}
	snaps := reconstructStatusSnapshots(items, dailyInstants(day(2026, 9, 10), day(2026, 9, 13)))
	if len(snaps) != 4 {
		t.Fatalf("snaps: got %d, want 4", len(snaps))
	}
	// Every snapshot carries all canonical states (zero-filled).
	for _, s := range snaps {
		for _, st := range canonicalStates {
			if _, ok := s.Counts[st]; !ok {
				t.Fatalf("snapshot %s missing canonical state %q", s.Date, st)
			}
		}
	}
	want := []map[string]int{
		{"backlog": 1, "todo": 0, "in_progress": 0, "in_review": 0, "done": 0}, // 09-10: only item1, backlog
		{"backlog": 1, "todo": 0, "in_progress": 1, "in_review": 0, "done": 0}, // 09-11: item1 backlog, item2 in_progress
		{"backlog": 0, "todo": 0, "in_progress": 2, "in_review": 0, "done": 0}, // 09-12: item1 moved to in_progress
		{"backlog": 0, "todo": 0, "in_progress": 1, "in_review": 0, "done": 1}, // 09-13: item2 → done
	}
	for i, s := range snaps {
		for st, n := range want[i] {
			if s.Counts[st] != n {
				t.Errorf("snapshot %s[%s] = %d, want %d (full=%v)", s.Date, st, s.Counts[st], n, s.Counts)
			}
		}
	}
}

// TestReconstructEmpty — no items still yields one zero-filled snapshot per day
// (the honest empty series, not a nil).
func TestReconstructEmpty(t *testing.T) {
	snaps := reconstructStatusSnapshots(nil, dailyInstants(day(2026, 9, 10), day(2026, 9, 11)))
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
		if len(s.Counts) != len(canonicalStates) {
			t.Errorf("snapshot %s should zero-fill %d states, got %d", s.Date, len(canonicalStates), len(s.Counts))
		}
	}
}
