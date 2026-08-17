package discussionindex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/memory"
)

// fakeSource is a scripted discussion projection: it returns the messages at/after `since`, oldest
// first, capped by limit — the same contract as discussion.Store.AllForMemoryIndex.
type fakeSource struct {
	msgs []discussion.MemoryIndexable
}

func (f *fakeSource) AllForMemoryIndex(_ context.Context, since time.Time, limit int) ([]discussion.MemoryIndexable, error) {
	var out []discussion.MemoryIndexable
	for _, m := range f.msgs {
		if !m.CreatedAt.Before(since) {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// fakeSink captures memory.WriteRequests (and can be told to fail specific bodies to exercise the
// best-effort skip path). It implements just enough of memory.Backend for the indexer.
type fakeSink struct {
	writes   []memory.WriteRequest
	failBody string
}

func (f *fakeSink) Write(_ context.Context, req memory.WriteRequest) (memory.Record, error) {
	if req.Content == f.failBody {
		return memory.Record{}, errors.New("simulated write failure")
	}
	f.writes = append(f.writes, req)
	return memory.Record{ID: uuid.NewString()}, nil
}
func (f *fakeSink) Ready(context.Context) error { return nil }
func (f *fakeSink) Search(context.Context, memory.SearchQuery) ([]memory.SearchHit, error) {
	return nil, nil
}
func (f *fakeSink) Invalidate(context.Context, string) (bool, error) { return false, nil }
func (f *fakeSink) Close()                                           {}

func msg(id uuid.UUID, project, team uuid.UUID, principal string, agentID, runID *string, body string, at time.Time) discussion.MemoryIndexable {
	return discussion.MemoryIndexable{
		MessageID: id, ThreadID: uuid.New(), ProjectID: project, TeamID: team,
		AuthorPrincipal: principal, AuthorAgentID: agentID, AuthorRunID: runID, Body: body, CreatedAt: at,
	}
}

func str(s string) *string { return &s }

// TestIndex_ProvenanceInEqualsOut asserts the indexer mirrors the server-stamped 10.1 provenance triple
// VERBATIM into the memory record (kind="discussion", scope preserved, author never invented). This is
// the structural AC5 property the falsification bench pins: provenance in = provenance out.
func TestIndex_ProvenanceInEqualsOut(t *testing.T) {
	team, project := uuid.New(), uuid.New()
	at := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	src := &fakeSource{msgs: []discussion.MemoryIndexable{
		msg(uuid.New(), project, team, "agent:planner", str("agent-planner"), str("run-77"),
			"deploy plan for the release", at),
	}}
	sink := &fakeSink{}
	ix := NewIndexer(src, sink, memory.NewHashingEmbedder(), 0)

	n, err := ix.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 || len(sink.writes) != 1 {
		t.Fatalf("indexed %d writes %d, want 1/1", n, len(sink.writes))
	}
	w := sink.writes[0]
	if w.Kind != memory.KindDiscussion {
		t.Fatalf("kind = %q, want %q", w.Kind, memory.KindDiscussion)
	}
	if w.SquadID != team.String() {
		t.Fatalf("squad_id = %q, want team %q (tenancy preserved)", w.SquadID, team)
	}
	if w.ProjectID == nil || *w.ProjectID != project.String() {
		t.Fatalf("project_id = %v, want %q", w.ProjectID, project)
	}
	if len(w.Embedding) != memory.EmbeddingDim {
		t.Fatalf("embedding dim = %d, want %d", len(w.Embedding), memory.EmbeddingDim)
	}
	// The honest text triple must be carried in provenance (the uuid columns can't hold it).
	var p struct {
		Source          string  `json:"source"`
		AuthorPrincipal string  `json:"author_principal"`
		AuthorAgentID   *string `json:"author_agent_id"`
		AuthorRunID     *string `json:"author_run_id"`
		WrittenAt       string  `json:"written_at"`
	}
	if err := json.Unmarshal(w.Provenance, &p); err != nil {
		t.Fatalf("provenance not json: %v", err)
	}
	if p.Source != memory.ProvenanceSourceDiscussion {
		t.Fatalf("provenance.source = %q, want %q", p.Source, memory.ProvenanceSourceDiscussion)
	}
	if p.AuthorPrincipal != "agent:planner" {
		t.Fatalf("provenance principal = %q, want agent:planner (server-stamped, not client)", p.AuthorPrincipal)
	}
	if p.AuthorAgentID == nil || *p.AuthorAgentID != "agent-planner" {
		t.Fatalf("provenance agent_id = %v, want agent-planner", p.AuthorAgentID)
	}
	if p.AuthorRunID == nil || *p.AuthorRunID != "run-77" {
		t.Fatalf("provenance run_id = %v, want run-77", p.AuthorRunID)
	}
	if p.WrittenAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("provenance written_at = %q, want %q", p.WrittenAt, at.Format(time.RFC3339Nano))
	}
	// A human row (no agent id) must NOT get an agent substrate column.
	if w.AgentID == nil {
		t.Fatal("agent row must derive a substrate agent uuid")
	}
}

// TestSweep_NoDoubleIndex asserts a second sweep re-reading the same watermark-boundary rows does not
// re-index them (the seen-set dedup), so recall isn't polluted by duplicates.
func TestSweep_NoDoubleIndex(t *testing.T) {
	team, project := uuid.New(), uuid.New()
	at := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	src := &fakeSource{msgs: []discussion.MemoryIndexable{
		msg(uuid.New(), project, team, "alice@corp", nil, nil, "one", at),
		msg(uuid.New(), project, team, "bob@corp", nil, nil, "two", at), // same ts — a boundary tie
	}}
	sink := &fakeSink{}
	ix := NewIndexer(src, sink, memory.NewHashingEmbedder(), 0)

	if n, _ := ix.Sweep(context.Background()); n != 2 {
		t.Fatalf("first sweep indexed %d, want 2", n)
	}
	if n, _ := ix.Sweep(context.Background()); n != 0 {
		t.Fatalf("second sweep indexed %d, want 0 (dedup on the watermark boundary)", n)
	}
	if len(sink.writes) != 2 {
		t.Fatalf("total writes = %d, want 2 (no duplicates)", len(sink.writes))
	}
}

// TestSweep_BestEffortSkip asserts a single failing write is logged-and-skipped (never aborting the
// batch) and is RETRIED on the next sweep once it would succeed — the AC5 best-effort posture. The
// poison row is EARLIER than a later success, which locks the watermark-freeze: the later success must
// not advance the watermark past the earlier failure, or the failure would be lost forever.
func TestSweep_BestEffortSkip(t *testing.T) {
	team, project := uuid.New(), uuid.New()
	at := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	src := &fakeSource{msgs: []discussion.MemoryIndexable{
		msg(uuid.New(), project, team, "bob@corp", nil, nil, "poison", at),                  // earlier — fails
		msg(uuid.New(), project, team, "alice@corp", nil, nil, "good", at.Add(time.Second)), // later — succeeds
	}}
	sink := &fakeSink{failBody: "poison"}
	ix := NewIndexer(src, sink, memory.NewHashingEmbedder(), 0)

	if n, _ := ix.Sweep(context.Background()); n != 1 {
		t.Fatalf("first sweep indexed %d, want 1 (the poison row is skipped, not fatal)", n)
	}
	// The later success must NOT have advanced the watermark past the earlier failure. Once the poison
	// row can succeed, a retry indexes it (and does NOT re-index the already-seen later row).
	sink.failBody = ""
	if n, _ := ix.Sweep(context.Background()); n != 1 {
		t.Fatalf("retry sweep indexed %d, want 1 (the earlier poison row is retried, later row not re-indexed)", n)
	}
	if len(sink.writes) != 2 {
		t.Fatalf("total writes = %d, want 2 (each row indexed exactly once)", len(sink.writes))
	}
}
