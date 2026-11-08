package discussionindex

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/memory"
)

// fakeRetractor captures GAP-3 reset retraction calls.
type fakeRetractor struct {
	calls []retractCall
}

type retractCall struct {
	squad, thread string
	before        time.Time
}

func (f *fakeRetractor) RetractDiscussionThreadRecords(_ context.Context, squadID, threadID string, before time.Time) (int64, error) {
	f.calls = append(f.calls, retractCall{squadID, threadID, before})
	return 2, nil
}

// fakeResolver maps any slug ref to a fixed uid for the GAP-1 normalization test.
type fakeResolver struct{ uid string }

func (f fakeResolver) ResolveProjectUID(context.Context, string) (string, bool) { return f.uid, true }

// TestSweep_ResetMarkerRetractsNotIndexes (ISI-7228 GAP-3): a kind='reset' marker is NEVER
// embedded into recall, and retracts the thread's projected records authored strictly before it;
// messages after the marker in the same thread still index normally.
func TestSweep_ResetMarkerRetractsNotIndexes(t *testing.T) {
	team, thread := uuid.New(), uuid.New()
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	mk := func(kind, body string, at time.Time) discussion.MemoryIndexable {
		return discussion.MemoryIndexable{
			MessageID: uuid.New(), ThreadID: thread, ProjectID: "ns/proj", TeamID: team,
			AuthorPrincipal: "henrik", Audience: "party", Kind: kind, Body: body, CreatedAt: at,
		}
	}
	markerBody := "— discussion cleared —"
	src := &fakeSource{msgs: []discussion.MemoryIndexable{
		mk("text", "old message", t0),
		mk("reset", markerBody, t0.Add(time.Minute)),
		mk("text", "post-reset message", t0.Add(2*time.Minute)),
	}}
	sink := &fakeSink{}
	rt := &fakeRetractor{}
	ix := NewIndexer(src, sink, memory.NewHashingEmbedder(), 0).WithRetractor(rt)

	n, err := ix.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("indexed = %d, want 2 (the marker is excluded)", n)
	}
	for _, w := range sink.writes {
		if w.Content == markerBody {
			t.Fatalf("reset marker was indexed into recall")
		}
	}
	if len(rt.calls) != 1 {
		t.Fatalf("retract calls = %d, want 1", len(rt.calls))
	}
	if got := rt.calls[0]; got.thread != thread.String() || got.squad != team.String() || !got.before.Equal(t0.Add(time.Minute)) {
		t.Fatalf("retract scope = (%s,%s,%v), want (%s,%s,marker time)", got.squad, got.thread, got.before, team, thread)
	}
}

// TestIndex_SlugProjectRefNormalizedToUID (ISI-7228 GAP-1): a slug-form project ref stamps the
// resolved Project CR uid so the record lands in the project-narrowed recall scope; with no
// resolver attached the ref passes through verbatim (fail-open — never less recallable).
func TestIndex_SlugProjectRefNormalizedToUID(t *testing.T) {
	team := uuid.New()
	uid := "912e88e2-1111-4222-8333-444455556666"
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{msgs: []discussion.MemoryIndexable{{
		MessageID: uuid.New(), ThreadID: uuid.New(), ProjectID: "bmad-squad/bmad-todo-demo", TeamID: team,
		AuthorPrincipal: "henrik", Audience: "party", Body: "plan", CreatedAt: at,
	}}}

	sink := &fakeSink{}
	ix := NewIndexer(src, sink, memory.NewHashingEmbedder(), 0).WithProjectResolver(fakeResolver{uid: uid})
	if _, err := ix.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(sink.writes) != 1 || sink.writes[0].ProjectID == nil || *sink.writes[0].ProjectID != uid {
		t.Fatalf("project id = %v, want resolved uid %s", sink.writes[0].ProjectID, uid)
	}

	sink2 := &fakeSink{}
	ix2 := NewIndexer(src, sink2, memory.NewHashingEmbedder(), 0) // no resolver: verbatim fail-open
	if _, err := ix2.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep (no resolver): %v", err)
	}
	if len(sink2.writes) != 1 || *sink2.writes[0].ProjectID != "bmad-squad/bmad-todo-demo" {
		t.Fatalf("verbatim stamp lost: %v", sink2.writes[0].ProjectID)
	}
}
