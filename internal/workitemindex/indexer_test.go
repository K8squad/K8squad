package workitemindex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/K8squad/K8squad/internal/memory"
)

// fakeSource serves items re-fetching anything whose ActivityAt is at/after the watermark it is asked
// for — the same contract as SQLSource.AllForMemoryIndex (oldest-activity-first, activity >= since).
type fakeSource struct {
	items []WorkItemIndexable
}

func (f *fakeSource) AllForMemoryIndex(_ context.Context, since time.Time, limit int) ([]WorkItemIndexable, error) {
	var out []WorkItemIndexable
	for _, w := range f.items {
		if !w.ActivityAt.Before(since) {
			out = append(out, w)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// fakeSink records every write keyed by the dedupe id, modelling the store's ON CONFLICT DO NOTHING
// idempotency: a re-write of an id already present is a no-op returning the existing row. failOn fails
// the n-th distinct write to drive the best-effort freeze path.
type fakeSink struct {
	byID   map[string]memory.WriteRequest
	order  []string
	writes int
	failOn int // 1-based: fail the n-th distinct write attempt
}

func newFakeSink() *fakeSink { return &fakeSink{byID: map[string]memory.WriteRequest{}} }

func (f *fakeSink) Write(_ context.Context, req memory.WriteRequest) (memory.Record, error) {
	id := ""
	if req.DedupeID != nil {
		id = *req.DedupeID
	}
	if _, ok := f.byID[id]; ok {
		return memory.Record{ID: id}, nil // idempotent no-op
	}
	f.writes++
	if f.failOn > 0 && f.writes == f.failOn {
		return memory.Record{}, fmt.Errorf("sink outage (best-effort: must not block the board)")
	}
	f.byID[id] = req
	f.order = append(f.order, id)
	return memory.Record{ID: id, CreatedAt: time.Now()}, nil
}

func (f *fakeSink) Ready(context.Context) error { return nil }
func (f *fakeSink) Search(context.Context, memory.SearchQuery) ([]memory.SearchHit, error) {
	return nil, nil
}
func (f *fakeSink) Invalidate(context.Context, string) (bool, error) { return false, nil }
func (f *fakeSink) Close()                                           {}

// fakeSuperseder records SupersedeWorkItemRecords calls.
type supCall struct{ squad, wi, keep string }
type fakeSuperseder struct{ calls []supCall }

func (f *fakeSuperseder) SupersedeWorkItemRecords(_ context.Context, squad, wi, keep string) (int64, error) {
	f.calls = append(f.calls, supCall{squad, wi, keep})
	return int64(len(f.calls)), nil
}

// fakeCursor is an in-memory CursorStore. saveErr models a crash between a batch commit and the
// watermark persist — the gap restart-survival must close.
type fakeCursor struct {
	saved   *memory.ProjectionCursor
	saveErr error
	saves   int
}

func (c *fakeCursor) LoadProjectionCursor(context.Context, string) (memory.ProjectionCursor, bool, error) {
	if c.saved == nil {
		return memory.ProjectionCursor{}, false, nil
	}
	return *c.saved, true, nil
}

func (c *fakeCursor) SaveProjectionCursor(_ context.Context, _ string, at time.Time, id string) error {
	c.saves++
	if c.saveErr != nil {
		return c.saveErr
	}
	c.saved = &memory.ProjectionCursor{LastProjectedAt: at, LastProjectedID: id}
	return nil
}

func item(id, project, team, title, body, state, by string, created, activity time.Time) WorkItemIndexable {
	return WorkItemIndexable{
		WorkItemID: id, ProjectID: project, TeamID: team,
		Title: title, Body: body, State: state, CreatedBy: by,
		CreatedAt: created, ActivityAt: activity,
	}
}

// TestSweep_ProjectsProvenancedScopedWrite is the core deliverable: one committed board work item
// becomes ONE provenanced, project/squad-scoped memory write — kind work-item, authored by the coord
// created_by principal's deterministic uuid substrate + verbatim text provenance, content the
// title+body+comments, idempotent on a derived revision id.
func TestSweep_ProjectsProvenancedScopedWrite(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	w := item("wi-1", "proj-A", "team-1", "Fix the login flow", "Users cannot sign in with SSO.", "in_progress", "agent:coder", at, at)
	w.CommentCount = 1
	w.CommentBodies = "alice@corp: repro on staging"
	src := &fakeSource{items: []WorkItemIndexable{w}}
	sink := newFakeSink()
	sup := &fakeSuperseder{}
	ix := NewIndexer(src, sink, memory.NewHashingEmbedder(), sup, 0)

	n, err := ix.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 || len(sink.order) != 1 {
		t.Fatalf("indexed %d, wrote %d — want 1/1", n, len(sink.order))
	}
	got := sink.byID[sink.order[0]]
	if got.SquadID != "team-1" {
		t.Errorf("SquadID = %q, want team-1 (tenancy root)", got.SquadID)
	}
	if got.ProjectID == nil || *got.ProjectID != "proj-A" {
		t.Errorf("ProjectID = %v, want proj-A", got.ProjectID)
	}
	if got.Kind != memory.KindWorkItem {
		t.Errorf("Kind = %q, want %q", got.Kind, memory.KindWorkItem)
	}
	if got.AgentID != nil || got.RunID != nil {
		t.Errorf("coord item has no agent/run identity — want nil/nil, got agent=%v run=%v", got.AgentID, got.RunID)
	}
	if want := deriveUUID("principal", "agent:coder"); got.PrincipalID != want {
		t.Errorf("PrincipalID = %q, want derived %q", got.PrincipalID, want)
	}
	for _, frag := range []string{"Fix the login flow", "Users cannot sign in with SSO.", "repro on staging"} {
		if !strings.Contains(got.Content, frag) {
			t.Errorf("content missing %q:\n%s", frag, got.Content)
		}
	}
	// Honest text provenance (provenance in = provenance out) — and the record flows through the
	// untrusted envelope with the coord principal surfaced verbatim, agent_id nil.
	var p struct {
		Source          string `json:"source"`
		WorkItemID      string `json:"work_item_id"`
		State           string `json:"state"`
		AuthorPrincipal string `json:"author_principal"`
		CommentCount    int    `json:"comment_count"`
	}
	if err := json.Unmarshal(got.Provenance, &p); err != nil {
		t.Fatalf("provenance not json: %v", err)
	}
	if p.Source != memory.ProvenanceSourceWorkItem || p.WorkItemID != "wi-1" || p.AuthorPrincipal != "agent:coder" || p.State != "in_progress" || p.CommentCount != 1 {
		t.Errorf("provenance = %+v — want source=work-item wi-1 agent:coder in_progress count=1", p)
	}
	// Supersede retires earlier revisions of the SAME item, excluding the just-written revision.
	if len(sup.calls) != 1 || sup.calls[0].wi != "wi-1" || sup.calls[0].squad != "team-1" || sup.calls[0].keep != sink.order[0] {
		t.Errorf("supersede calls = %+v — want one (team-1, wi-1, keep=%s)", sup.calls, sink.order[0])
	}
}

// TestSweep_ReindexOnChange is the mutability property discussionindex does NOT have: when a work
// item's title/body is edited or a comment is appended, its ActivityAt advances, a NEW revision id is
// derived, and the item is re-projected as a fresh row (the supersede then retires the stale revision).
func TestSweep_ReindexOnChange(t *testing.T) {
	t0 := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	w := item("wi-1", "proj-A", "team-1", "Title v1", "body v1", "todo", "alice@corp", t0, t0)
	src := &fakeSource{items: []WorkItemIndexable{w}}
	sink := newFakeSink()
	sup := &fakeSuperseder{}
	ix := NewIndexer(src, sink, memory.NewHashingEmbedder(), sup, 0)

	if n, _ := ix.Sweep(context.Background()); n != 1 {
		t.Fatalf("first sweep indexed %d, want 1", n)
	}
	// Re-sweep with NO change: same revision id ⇒ idempotent no-op, nothing new indexed.
	if n, _ := ix.Sweep(context.Background()); n != 0 {
		t.Fatalf("unchanged re-sweep indexed %d, want 0 (idempotent)", n)
	}
	// Now the item is edited: title + body change and a comment lands, bumping ActivityAt.
	src.items[0].Title = "Title v2"
	src.items[0].Body = "body v2"
	src.items[0].CommentCount = 1
	src.items[0].CommentBodies = "bob@corp: fixed in PR#9"
	src.items[0].ActivityAt = t0.Add(time.Minute)

	if n, _ := ix.Sweep(context.Background()); n != 1 {
		t.Fatalf("post-edit sweep indexed %d, want 1 (new revision)", n)
	}
	if len(sink.order) != 2 {
		t.Fatalf("want 2 distinct revision rows, got %d", len(sink.order))
	}
	latest := sink.byID[sink.order[1]]
	if !strings.Contains(latest.Content, "Title v2") || !strings.Contains(latest.Content, "fixed in PR#9") {
		t.Errorf("latest revision content stale:\n%s", latest.Content)
	}
	// Each real projection asked the superseder to retire earlier revisions of wi-1.
	if len(sup.calls) != 2 {
		t.Errorf("supersede calls = %d, want 2 (one per real projection)", len(sup.calls))
	}
}

// TestSweep_SkipsTeamlessWithoutFreezing: a work item with no team scope yet is unindexable (squad_id
// is NOT NULL), skipped WITHOUT freezing the watermark so a newer item still indexes — the documented
// best-effort tradeoff (a permanently-teamless item must never wedge liveness).
func TestSweep_SkipsTeamlessWithoutFreezing(t *testing.T) {
	t0 := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	teamless := item("wi-teamless", "proj-A", "", "orphan", "", "backlog", "alice@corp", t0, t0)
	withTeam := item("wi-2", "proj-A", "team-1", "real ticket", "has a team", "todo", "bob@corp", t0.Add(time.Second), t0.Add(time.Second))
	src := &fakeSource{items: []WorkItemIndexable{teamless, withTeam}}
	sink := newFakeSink()
	ix := NewIndexer(src, sink, memory.NewHashingEmbedder(), &fakeSuperseder{}, 0)

	n, err := ix.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 || len(sink.order) != 1 {
		t.Fatalf("indexed %d/%d — want the teamed item only (1/1)", n, len(sink.order))
	}
	if got := sink.byID[sink.order[0]]; got.SquadID != "team-1" {
		t.Errorf("indexed the wrong item (squad %q) — want the teamed wi-2", got.SquadID)
	}
}

// TestSweep_BestEffortFreezeOnWriteFailure: a write failure freezes the watermark so the failed item is
// re-fetched next sweep (self-healing), while later items in the SAME batch still index.
func TestSweep_BestEffortFreezeOnWriteFailure(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)
	a := item("wi-a", "proj-A", "team-1", "first", "", "todo", "x", t0, t0)
	b := item("wi-b", "proj-A", "team-1", "second", "", "todo", "y", t0.Add(time.Second), t0.Add(time.Second))
	src := &fakeSource{items: []WorkItemIndexable{a, b}}
	sink := newFakeSink()
	sink.failOn = 1 // wi-a's first write fails
	ix := NewIndexer(src, sink, memory.NewHashingEmbedder(), &fakeSuperseder{}, 0)

	// First sweep: wi-a fails (frozen), wi-b still indexes.
	if n, _ := ix.Sweep(context.Background()); n != 1 {
		t.Fatalf("first sweep indexed %d, want 1 (wi-b only; wi-a failed)", n)
	}
	// Second sweep: the frozen watermark re-fetches wi-a (now the sink recovers) — it indexes, and wi-b
	// is a seen idempotent no-op.
	if n, _ := ix.Sweep(context.Background()); n != 1 {
		t.Fatalf("second sweep indexed %d, want 1 (wi-a recovered)", n)
	}
	if len(sink.order) != 2 {
		t.Fatalf("want both items eventually indexed, got %d", len(sink.order))
	}
}

// TestCursor_ResumeAcrossRestart: a restart resumes from the PERSISTED watermark, not from zero. A
// fresh indexer (new seen-set, same durable cursor + sink) picks up where the first left off.
func TestCursor_ResumeAcrossRestart(t *testing.T) {
	base := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	items := []WorkItemIndexable{
		item("wi-1", "proj-A", "team-1", "one", "", "todo", "a", base, base),
		item("wi-2", "proj-A", "team-1", "two", "", "todo", "b", base.Add(time.Second), base.Add(time.Second)),
		item("wi-3", "proj-A", "team-1", "three", "", "todo", "c", base.Add(2*time.Second), base.Add(2*time.Second)),
		item("wi-4", "proj-A", "team-1", "four", "", "todo", "d", base.Add(3*time.Second), base.Add(3*time.Second)),
	}
	src := &fakeSource{items: items}
	sink := newFakeSink()
	cur := &fakeCursor{}
	embed := memory.NewHashingEmbedder()

	// Process 1: batchSize 2 ⇒ first sweep projects the first two and persists the cursor.
	ix1 := NewIndexer(src, sink, embed, nil, 2).WithCursor(cur)
	if n, err := ix1.Sweep(context.Background()); err != nil || n != 2 {
		t.Fatalf("process-1 sweep indexed %d (err %v), want 2", n, err)
	}
	if cur.saved == nil {
		t.Fatal("process-1 must have persisted a watermark")
	}
	// Process 2: a fresh indexer resumes from the durable cursor and projects the remaining two — no
	// full re-scan, every item ends in recall exactly once.
	ix2 := NewIndexer(src, sink, embed, nil, 2).WithCursor(cur)
	if n, err := ix2.Sweep(context.Background()); err != nil {
		t.Fatalf("process-2 sweep: %v", err)
	} else if n == 0 {
		t.Fatal("process-2 resumed empty — the cursor did not carry the watermark")
	}
	// Drain and confirm all four distinct items landed exactly once.
	for {
		n, err := ix2.Sweep(context.Background())
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if n == 0 {
			break
		}
	}
	if len(sink.order) != 4 {
		t.Fatalf("want 4 distinct items indexed across the restart, got %d", len(sink.order))
	}
}
