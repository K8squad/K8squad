package apiserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/events"
)

// fakeRunReader is an in-memory events.RunEventReader (no Postgres), mirroring the
// relay_test.go fake-store approach: it holds an id-sorted slice of run events and
// answers the three read shapes off it. Errors are injectable to prove best-effort.
type fakeRunReader struct {
	mu   sync.Mutex
	rows []events.RunEvent // kept ascending by ID by the test

	errLatest, errAfter, errForRun error
	lastForRunAfterID              int64 // captured for the replay-adapter test
}

func (f *fakeRunReader) add(rows ...events.RunEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, rows...)
}

func (f *fakeRunReader) LatestRunEventID(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errLatest != nil {
		return 0, f.errLatest
	}
	var max int64
	for _, r := range f.rows {
		if r.ID > max {
			max = r.ID
		}
	}
	return max, nil
}

func (f *fakeRunReader) RunEventsAfter(_ context.Context, afterID int64, limit int) ([]events.RunEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errAfter != nil {
		return nil, f.errAfter
	}
	var out []events.RunEvent
	for _, r := range f.rows {
		if r.ID > afterID && r.RunID != "" {
			out = append(out, r)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRunReader) RunEventsForRun(_ context.Context, runID string, afterID int64, limit int) ([]events.RunEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastForRunAfterID = afterID
	if f.errForRun != nil {
		return nil, f.errForRun
	}
	var out []events.RunEvent
	for _, r := range f.rows {
		if r.RunID == runID && r.ID > afterID {
			out = append(out, r)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// recvWithin reads one Event from a subscriber channel or fails on timeout.
func recvWithin(t *testing.T, sub *subscriber, d time.Duration) Event {
	t.Helper()
	select {
	case ev, ok := <-sub.ch:
		if !ok {
			t.Fatal("subscriber channel closed unexpectedly")
		}
		return ev
	case <-time.After(d):
		t.Fatal("timeout waiting for a hub event")
		return Event{}
	}
}

// The projector seeds its watermark at the current tail (so pre-existing history is
// NOT re-fanned to live subscribers) and then fans only NEW rows, keyed by run_id.
func TestRunEventSource_FansOnlyNewEventsKeyedByRun(t *testing.T) {
	reader := &fakeRunReader{}
	// Pre-existing history the projector must NOT replay onto the live tail.
	reader.add(
		events.RunEvent{ID: 1, RunID: "run-a", EventType: "created", Payload: []byte(`{}`)},
		events.RunEvent{ID: 2, RunID: "run-a", EventType: "reconcile_advanced", Payload: []byte(`{"to_step":"x"}`)},
	)

	hub := NewHub()
	subA := hub.Subscribe("run-a")
	subB := hub.Subscribe("run-b")

	src := NewRunEventSource(reader, hub, WithProjectorPoll(5*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = src.Run(ctx) }()

	// Give the projector a moment to seed its watermark past the pre-existing rows,
	// then append two NEW events on different runs.
	time.Sleep(40 * time.Millisecond)
	reader.add(
		events.RunEvent{ID: 3, RunID: "run-a", EventType: "reconcile_advanced", Payload: []byte(`{"to_step":"y"}`)},
		events.RunEvent{ID: 4, RunID: "run-b", EventType: "completed", Payload: []byte(`{"ok":true}`)},
	)

	// run-a's subscriber sees id 3 first (never the pre-watermark 1/2); run-b sees id 4.
	if ev := recvWithin(t, subA, time.Second); ev.ID != "3" {
		t.Fatalf("run-a: want first event id 3 (history not re-fanned), got id %q", ev.ID)
	}
	if ev := recvWithin(t, subB, time.Second); ev.ID != "4" || ev.Type != "completed" {
		t.Fatalf("run-b: want id 4 completed, got id %q type %q", ev.ID, ev.Type)
	}
	// Cross-run isolation: run-b's event must not have leaked to run-a.
	select {
	case ev := <-subA.ch:
		t.Fatalf("run-a received an unexpected extra event id %q (cross-run leak?)", ev.ID)
	case <-time.After(50 * time.Millisecond):
	}
}

// A failed watermark seed is non-fatal: the projector starts at 0 and still fans
// the backlog to whoever is subscribed rather than crashing.
func TestRunEventSource_SeedErrorIsNonFatal(t *testing.T) {
	reader := &fakeRunReader{errLatest: errors.New("boom")}
	reader.add(events.RunEvent{ID: 7, RunID: "run-a", EventType: "created", Payload: []byte(`{}`)})

	hub := NewHub()
	sub := hub.Subscribe("run-a")
	src := NewRunEventSource(reader, hub, WithProjectorPoll(5*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = src.Run(ctx) }()

	if ev := recvWithin(t, sub, time.Second); ev.ID != "7" {
		t.Fatalf("want backlog id 7 fanned after seed error, got %q", ev.ID)
	}
}

// A tail scan error does not advance the watermark and does not crash: once the
// error clears, the same rows are fanned (nothing skipped).
func TestRunEventSource_TailErrorRetriesWithoutSkipping(t *testing.T) {
	// Force the seed to 0 deterministically (errLatest ⇒ start-at-0 path) so the row
	// added after startup is never below the watermark regardless of seed/add ordering.
	// The row arrives (id 9) while tail scans are erroring, so it can only be delivered
	// once the error clears.
	reader := &fakeRunReader{errLatest: errors.New("no seed"), errAfter: errors.New("db blip")}

	hub := NewHub()
	sub := hub.Subscribe("run-a")
	src := NewRunEventSource(reader, hub, WithProjectorPoll(5*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = src.Run(ctx) }()

	reader.add(events.RunEvent{ID: 9, RunID: "run-a", EventType: "created", Payload: []byte(`{}`)})
	time.Sleep(30 * time.Millisecond) // let a few failing ticks pass
	reader.mu.Lock()
	reader.errAfter = nil
	reader.mu.Unlock()

	if ev := recvWithin(t, sub, time.Second); ev.ID != "9" {
		t.Fatalf("want id 9 fanned after tail error clears, got %q", ev.ID)
	}
}

// The replay adapter converts per-run rows to SSE Events and forwards afterID/error.
func TestRunReplayAdapter_ConvertsAndForwards(t *testing.T) {
	reader := &fakeRunReader{}
	reader.add(
		events.RunEvent{ID: 1, RunID: "run-a", EventType: "created", Payload: []byte(`{}`)},
		events.RunEvent{ID: 2, RunID: "run-a", EventType: "reconcile_advanced", Payload: []byte(`{"to_step":"x"}`)},
		events.RunEvent{ID: 3, RunID: "run-b", EventType: "completed", Payload: []byte(`{}`)},
	)
	adapter := NewRunReplayer(reader)

	evs, err := adapter.replayRun(context.Background(), "run-a", 1)
	if err != nil {
		t.Fatalf("replayRun: %v", err)
	}
	if len(evs) != 1 || evs[0].ID != "2" || evs[0].Type != "reconcile_advanced" || evs[0].Data != `{"to_step":"x"}` {
		t.Fatalf("want single run-a event id 2 after id 1, got %+v", evs)
	}
	if reader.lastForRunAfterID != 1 {
		t.Fatalf("adapter did not forward afterID: got %d want 1", reader.lastForRunAfterID)
	}

	reader.errForRun = errors.New("cast error")
	if _, err := adapter.replayRun(context.Background(), "not-a-uuid", 0); err == nil {
		t.Fatal("want replay error propagated")
	}
}
