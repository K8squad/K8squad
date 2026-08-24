package recallsource

import (
	"context"
	"testing"
	"time"

	"github.com/K8squad/K8squad/internal/memory"
)

// fakeSearcher satisfies the ReadService's searcher seam AND the idSearcher slice the
// pinned arm type-asserts, so both arms run without a live pgvector.
type fakeSearcher struct {
	gotQuery   memory.SearchQuery
	gotText    string
	gotIDs     []string
	hits       []memory.SearchHit
	byIDHits   []memory.SearchHit
	failSearch bool
}

func (f *fakeSearcher) Search(_ context.Context, q memory.SearchQuery) ([]memory.SearchHit, error) {
	f.gotQuery = q
	if f.failSearch {
		return nil, errBoom
	}
	return f.hits, nil
}

func (f *fakeSearcher) SearchByIDs(_ context.Context, q memory.SearchQuery, ids []string) ([]memory.SearchHit, error) {
	f.gotQuery = q
	f.gotIDs = ids
	return f.byIDHits, nil
}

var errBoom = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "boom" }

func hit(id, team, project string) memory.SearchHit {
	var h memory.SearchHit
	h.ID = id
	h.SquadID = team
	h.ProjectID = &project
	h.PrincipalID = "00000000-0000-0000-0000-0000000000aa"
	h.Kind = memory.KindHandoffMirror
	h.Content = `{"did":["shipped"]}`
	h.CreatedAt = time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	h.Distance = 0.5
	h.Provenance = memory.NewHandoffProvenance("coord+audit://7", "sha", "wi-1", "run-7", 7, nil, "agent-a", nil, h.CreatedAt)
	return h
}

// TestFreshArm_ScopedQueryUntrustedDoc: the fresh arm pushes the Run's tenancy into the
// query, synthesizes the query text via the QueryBuilder, and projects hits into
// envelope-verbatim RecallDocs with a distance-derived score.
func TestFreshArm_ScopedQueryUntrustedDoc(t *testing.T) {
	fake := &fakeSearcher{hits: []memory.SearchHit{hit("rec-1", "team-1", "proj-A")}}
	svc := memory.NewReadService(fake, memory.NewHashingEmbedder())
	var gotTeam, gotProject string
	src := NewRecallSource(svc, func(_ context.Context, team, project string) string {
		gotTeam, gotProject = team, project
		return "work item title + run inputs"
	})

	docs, err := src.MemoryRecall(context.Background(), "team-1", "proj-A", nil, 8)
	if err != nil {
		t.Fatalf("MemoryRecall fresh: %v", err)
	}
	if gotTeam != "team-1" || gotProject != "proj-A" {
		t.Fatalf("query builder saw %s/%s, want team-1/proj-A", gotTeam, gotProject)
	}
	if fake.gotQuery.SquadID != "team-1" || fake.gotQuery.ProjectID == nil || *fake.gotQuery.ProjectID != "proj-A" {
		t.Fatalf("query plan = %+v — scope must be pushed into the store, never widened", fake.gotQuery)
	}
	if fake.gotText != "work item title + run inputs" && fake.gotQuery.Limit != 8 {
		t.Fatalf("fresh recall must run the synthesized query at topK=8")
	}
	if len(docs) != 1 {
		t.Fatalf("docs = %d, want 1 (non-vacuity)", len(docs))
	}
	d := docs[0]
	if d.ID != "rec-1" || d.Author != "agent-a" || d.Scope != "team-1/proj-A" {
		t.Fatalf("doc = %+v, want envelope-verbatim {rec-1, agent-a, team-1/proj-A}", d)
	}
	if d.Score <= 0 || d.Score > 1 {
		t.Fatalf("score %v out of (0,1] for distance 0.5", d.Score)
	}
}

// TestPinnedArm_ExactIDsNoRanking: a non-empty ids slice takes the exact-id path — the
// SAME order requested, score 1.0 (no ranking), still scope-enforced.
func TestPinnedArm_ExactIDsNoRanking(t *testing.T) {
	fake := &fakeSearcher{byIDHits: []memory.SearchHit{hit("rec-9", "team-1", "proj-A")}}
	svc := memory.NewReadService(fake, memory.NewHashingEmbedder())
	src := NewRecallSource(svc, nil) // pinned arm needs no QueryBuilder

	docs, err := src.MemoryRecall(context.Background(), "team-1", "proj-A", []string{"rec-9"}, 8)
	if err != nil {
		t.Fatalf("MemoryRecall pinned: %v", err)
	}
	if len(fake.gotIDs) != 1 || fake.gotIDs[0] != "rec-9" {
		t.Fatalf("by-id read = %v, want exactly the pinned ids", fake.gotIDs)
	}
	if fake.gotQuery.SquadID != "team-1" {
		t.Fatalf("pinned read lost the tenancy scope: %+v", fake.gotQuery)
	}
	if len(docs) != 1 || docs[0].ID != "rec-9" {
		t.Fatalf("docs = %+v, want the pinned doc", docs)
	}
	if docs[0].Score != 1.0 {
		t.Fatalf("pinned score = %v, want 1.0 (no ranking on the exact-id path)", docs[0].Score)
	}
}

// TestFreshArm_RefusesWithoutQueryBuilder: no QueryBuilder + empty ids is an explicit
// error — never a silently-empty recall (that would read as "no memory exists").
func TestFreshArm_RefusesWithoutQueryBuilder(t *testing.T) {
	svc := memory.NewReadService(&fakeSearcher{}, memory.NewHashingEmbedder())
	src := NewRecallSource(svc, nil)
	if _, err := src.MemoryRecall(context.Background(), "team-1", "proj-A", nil, 8); err == nil {
		t.Fatal("expected refusal when fresh recall has no QueryBuilder wired")
	}
}

// TestRefusesEmptyTeam: the tenancy root is mandatory on both arms — recall itself
// must never become a cross-tenant surface.
func TestRefusesEmptyTeam(t *testing.T) {
	svc := memory.NewReadService(&fakeSearcher{}, memory.NewHashingEmbedder())
	src := NewRecallSource(svc, func(context.Context, string, string) string { return "q" })
	if _, err := src.MemoryRecall(context.Background(), "", "proj-A", nil, 8); err == nil {
		t.Fatal("expected refusal on empty team")
	}
	if _, err := src.MemoryRecall(context.Background(), "", "proj-A", []string{"rec-1"}, 8); err == nil {
		t.Fatal("expected refusal on empty team (pinned arm)")
	}
}

// TestSquadWideFreshRecall: an empty project narrows to nothing — squad-wide fresh
// recall is legal and the doc's Scope renders team-only.
func TestSquadWideFreshRecall(t *testing.T) {
	h := hit("rec-1", "team-1", "proj-A")
	h.ProjectID = nil // squad-scoped record
	fake := &fakeSearcher{hits: []memory.SearchHit{h}}
	svc := memory.NewReadService(fake, memory.NewHashingEmbedder())
	src := NewRecallSource(svc, func(context.Context, string, string) string { return "q" })
	docs, err := src.MemoryRecall(context.Background(), "team-1", "", nil, 5)
	if err != nil {
		t.Fatalf("squad-wide: %v", err)
	}
	if fake.gotQuery.ProjectID != nil {
		t.Fatalf("project predicate = %v, want nil (squad-wide)", fake.gotQuery.ProjectID)
	}
	if docs[0].Scope != "team-1" {
		t.Fatalf("scope = %q, want team-only", docs[0].Scope)
	}
}
