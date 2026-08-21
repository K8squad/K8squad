package handoffmirror

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/K8squad/K8squad/internal/memory"
)

// fakeSource serves rows in order, re-serving anything at/after the watermark it is asked for —
// the same contract as SQLSource.AllForMemoryMirror (oldest-first, created_at >= since, limited by limit).
type fakeSource struct {
	rows []MirrorSourceRow
}

func (f *fakeSource) AllForMemoryMirror(_ context.Context, since time.Time, limit int) ([]MirrorSourceRow, error) {
	var out []MirrorSourceRow
	for _, r := range f.rows {
		if !r.CreatedAt.Before(since) {
			out = append(out, r)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// fakeSink records every WriteRequest and can be told to fail the next write.
type fakeSink struct {
	writes []memory.WriteRequest
	ids    int
	failOn int // 1-based: fail the n-th write
}

func (f *fakeSink) Write(_ context.Context, req memory.WriteRequest) (memory.Record, error) {
	f.ids++
	if f.failOn > 0 && f.ids == f.failOn {
		return memory.Record{}, fmt.Errorf("sink outage (best-effort posture: must not roll back the artifact)")
	}
	f.writes = append(f.writes, req)
	return memory.Record{ID: fmt.Sprintf("rec-%d", f.ids), CreatedAt: time.Now()}, nil
}

func (f *fakeSink) Ready(context.Context) error { return nil }
func (f *fakeSink) Search(context.Context, memory.SearchQuery) ([]memory.SearchHit, error) {
	return nil, nil
}
func (f *fakeSink) Invalidate(context.Context, string) (bool, error) { return false, nil }
func (f *fakeSink) Close()                                           {}

// fakeSuperseder records SupersedeHandoffMirrors calls.
type fakeSuperseder struct {
	calls []struct{ squad, wi, run, keep string }
}

func (f *fakeSuperseder) SupersedeHandoffMirrors(_ context.Context, squad, wi, run, keep string) (int64, error) {
	f.calls = append(f.calls, struct{ squad, wi, run, keep string }{squad, wi, run, keep})
	return 1, nil
}

func row(auditID int64, wi, run, principal, team, project string, at time.Time) MirrorSourceRow {
	fence := int64(3)
	return MirrorSourceRow{
		AuditID: auditID, WorkItemID: wi, RunID: run, Principal: principal,
		FenceToken: &fence, Payload: `{"did":["shipped"],"next":["review"]}`,
		URI: fmt.Sprintf("coord+audit://%d", auditID), SHA256: "cafebabe",
		CreatedAt: at, ProjectID: project, TeamID: team,
	}
}

// TestMirror_SweepProjectsProvenancedScopedWrite is AC3: one committed publication becomes ONE
// provenanced, project/squad-scoped memory write — kind handoff-mirror, authored by the coord
// principal's deterministic uuid substrate + verbatim text provenance, content the canonical doc
// bytes, Run-linked, fence carried.
func TestMirror_SweepProjectsProvenancedScopedWrite(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	src := &fakeSource{rows: []MirrorSourceRow{row(42, "wi-1", "run-7", "agent-a", "team-1", "proj-A", at)}}
	sink := &fakeSink{}
	sup := &fakeSuperseder{}
	m := NewMirror(src, sink, memory.NewHashingEmbedder(), sup, 0)

	n, err := m.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 || len(sink.writes) != 1 {
		t.Fatalf("mirrored %d, wrote %d — want 1/1 (non-vacuity)", n, len(sink.writes))
	}
	w := sink.writes[0]
	if w.SquadID != "team-1" || w.ProjectID == nil || *w.ProjectID != "proj-A" {
		t.Fatalf("scope = %s/%v — the mirror MUST be recallable in the work item's own project/squad scope", w.SquadID, w.ProjectID)
	}
	if w.Kind != memory.KindHandoffMirror {
		t.Fatalf("kind = %q, want %q", w.Kind, memory.KindHandoffMirror)
	}
	if w.Content != `{"did":["shipped"],"next":["review"]}` {
		t.Fatalf("content = %q, want the canonical doc bytes verbatim", w.Content)
	}
	if w.RunID == nil || *w.RunID == "" {
		t.Fatal("run linkage missing (substrate run_id)")
	}
	if len(w.Embedding) != memory.EmbeddingDim {
		t.Fatalf("embedding dim = %d, want %d", len(w.Embedding), memory.EmbeddingDim)
	}

	// Supersede ran for the pair, keeping the just-written record live.
	if len(sup.calls) != 1 || sup.calls[0].wi != "wi-1" || sup.calls[0].run != "run-7" || sup.calls[0].keep != "rec-1" || sup.calls[0].squad != "team-1" {
		t.Fatalf("supersede calls = %+v, want one for wi-1/run-7 keeping rec-1 in team-1", sup.calls)
	}

	// Idempotence: the seen-set means a re-sweep of the same audit row writes nothing more.
	if n, _ := m.Sweep(context.Background()); n != 0 || len(sink.writes) != 1 {
		t.Fatalf("re-sweep mirrored %d, total writes %d — want 0/1 (seen-set)", n, len(sink.writes))
	}
}

// TestMirror_RepublishSupersedesEarlierMirror: a republished handoff arrives as a NEW audit row for
// the same (work item, run); the newest mirror is written and the supersede retires earlier mirrors,
// so recall surfaces exactly the newest publication.
func TestMirror_RepublishSupersedesEarlierMirror(t *testing.T) {
	t1 := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	src := &fakeSource{rows: []MirrorSourceRow{
		row(42, "wi-1", "run-7", "agent-a", "team-1", "proj-A", t1),
		row(99, "wi-1", "run-7", "agent-a", "team-1", "proj-A", t2),
	}}
	sink := &fakeSink{}
	sup := &fakeSuperseder{}
	m := NewMirror(src, sink, memory.NewHashingEmbedder(), sup, 0)

	if n, _ := m.Sweep(context.Background()); n != 2 {
		t.Fatalf("mirrored %d, want 2 (original + republish)", n)
	}
	if len(sup.calls) != 2 {
		t.Fatalf("supersede calls = %d, want 2 (one per publication)", len(sup.calls))
	}
	last := sup.calls[len(sup.calls)-1]
	if last.keep != "rec-2" {
		t.Fatalf("last supersede keeps %q, want rec-2 (the NEWEST mirror)", last.keep)
	}
	// The watermark advanced past both rows: a re-sweep re-serves nothing.
	if n, _ := m.Sweep(context.Background()); n != 0 {
		t.Fatalf("post-advance sweep mirrored %d, want 0", n)
	}
}

// TestMirror_NoTeamScopeSkipsWithoutForgingTenancy: a work item with no team yet cannot be mirrored
// (squad_id is NOT NULL and defaulting it would FORGE tenancy) — skipped, watermark frozen so the
// row is retried once the item gains a team.
func TestMirror_NoTeamScopeSkipsWithoutForgingTenancy(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	later := at.Add(time.Minute)
	src := &fakeSource{rows: []MirrorSourceRow{
		row(42, "wi-1", "run-7", "agent-a", "", "proj-A", at),          // no team — skip, freeze
		row(50, "wi-2", "run-8", "agent-b", "team-1", "proj-A", later), // still mirrors
	}}
	sink := &fakeSink{}
	m := NewMirror(src, sink, memory.NewHashingEmbedder(), nil, 0)

	if n, _ := m.Sweep(context.Background()); n != 1 {
		t.Fatalf("mirrored %d, want 1 (the teamless row skipped, the scoped row mirrored)", n)
	}
	if sink.writes[0].SquadID != "team-1" {
		t.Fatalf("the only write is for wi-2/run-8 in team-1 — the teamless row must NOT be mirrored to any scope")
	}
}

// TestMirror_SinkOutageIsBestEffortNotFatal is AC6: a memory write failure skips the row (frozen
// watermark, retried next sweep) and never errors the sweep — the committed 2.8 artifact is the
// source of truth and is never rolled back or blocked by the mirror.
func TestMirror_SinkOutageIsBestEffortNotFatal(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	src := &fakeSource{rows: []MirrorSourceRow{row(42, "wi-1", "run-7", "agent-a", "team-1", "proj-A", at)}}
	sink := &fakeSink{failOn: 1}
	m := NewMirror(src, sink, memory.NewHashingEmbedder(), nil, 0)

	n, err := m.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep must not fail on a sink outage (best-effort): %v", err)
	}
	if n != 0 || len(sink.writes) != 0 {
		t.Fatalf("mirrored %d, writes %d — want 0/0 on outage", n, len(sink.writes))
	}
	// Frozen watermark ⇒ the failed row is re-fetched and succeeds once the sink recovers.
	sink.failOn = 0
	if n, _ := m.Sweep(context.Background()); n != 1 || len(sink.writes) != 1 {
		t.Fatalf("recovery sweep mirrored %d, writes %d — want 1/1 (self-healing retry)", n, len(sink.writes))
	}
}

// TestMirror_NilSupersederStillMirrors: the superseder is optional — small sinks (tests, alt
// backends) mirror fine without republish-retire; production wiring passes the PgVectorStore.
func TestMirror_NilSupersederStillMirrors(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	src := &fakeSource{rows: []MirrorSourceRow{row(42, "wi-1", "run-7", "agent-a", "team-1", "proj-A", at)}}
	sink := &fakeSink{}
	m := NewMirror(src, sink, memory.NewHashingEmbedder(), nil, 0)
	if n, err := m.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v — want 1, nil (nil superseder is legal)", n, err)
	}
}

// TestMirror_WatermarkAdvancesPastTeamlessItems: teamless items don't freeze the watermark,
// allowing newer handoffs with teams to be mirrored while waiting for teamless items to gain teams.
func TestMirror_WatermarkAdvancesPastTeamlessItems(t *testing.T) {
	t1 := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC) // teamless item
	t2 := t1.Add(time.Minute)                           // teamless item
	t3 := t2.Add(time.Minute)                           // item with team
	t4 := t3.Add(time.Minute)                           // item with team

	src := &fakeSource{rows: []MirrorSourceRow{
		row(42, "wi-1", "run-7", "agent-a", "", "proj-A", t1),        // no team - should be deferred, watermark should advance
		row(43, "wi-2", "run-8", "agent-b", "", "proj-A", t2),        // no team - should be deferred, watermark should advance
		row(44, "wi-3", "run-9", "agent-c", "team-1", "proj-A", t3),  // has team - should be mirrored
		row(45, "wi-4", "run-10", "agent-d", "team-1", "proj-A", t4), // has team - should be mirrored
	}}
	sink := &fakeSink{}
	m := NewMirror(src, sink, memory.NewHashingEmbedder(), nil, 0)

	// First sweep should mirror the two team items and advance past all four
	n, err := m.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("mirrored %d, want 2 (only team items should mirror, teamless should be deferred)", n)
	}
	if len(sink.writes) != 2 {
		t.Fatalf("wrote %d, want 2", len(sink.writes))
	}

	// Watermark should be at t4 (latest item, even though teamless items were deferred)
	if !m.watermark.Equal(t4) {
		t.Fatalf("watermark = %v, want %v (should advance past all items)", m.watermark, t4)
	}

	// Second sweep should mirror nothing (all items seen, teamless still deferred but not frozen)
	n, err = m.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("mirrored %d, want 0 (all items should be seen or deferred)", n)
	}
	if len(sink.writes) != 2 {
		t.Fatalf("wrote %d, want 2 (no new writes)", len(sink.writes))
	}
}
