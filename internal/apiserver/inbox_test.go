package apiserver

// inbox_test.go — ISI-5535 (E1 of ISI-5531, ADR-0026 §3): unit coverage for the squadInbox
// handler's Go-side join. The handler unions the coord in_review arm + the discussion open-proposal
// arm, resolves each row's project path, orders by the run cache's claimedAt, and derives unread
// from the per-user read-marker store. These are the exact seams F3 flagged as untested, so the
// fakes below drive the join/sort/unread/live logic without a database or a cluster.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// --- fakes for the four handler seams -------------------------------------------------------------

type fakeReviews struct {
	items []coord.ReviewItem
	err   error
}

func (f fakeReviews) ListReviewItems(context.Context, string) ([]coord.ReviewItem, error) {
	return f.items, f.err
}

type fakeProposals struct {
	items []discussion.OpenProposalSummary
	err   error
}

func (f fakeProposals) ListOpenProposalsForTeam(context.Context, string) ([]discussion.OpenProposalSummary, error) {
	return f.items, f.err
}

type fakeMarkers struct {
	seen map[string]time.Time
}

func (f fakeMarkers) Seen(_ context.Context, _ string, keys []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for _, k := range keys {
		if t, ok := f.seen[k]; ok {
			out[k] = t
		}
	}
	return out, nil
}
func (f fakeMarkers) MarkSeen(context.Context, string, []string) error { return nil }

type fakeOverview struct{ ov SquadOverview }

func (f fakeOverview) Overview(context.Context, string, bool) (SquadOverview, error) {
	return f.ov, nil
}
func (f fakeOverview) Projects(context.Context, string, bool) (SquadProjectList, error) {
	return SquadProjectList{}, nil
}

// inboxReq builds a GET request whose context carries a non-admin, team-scoped AuthorContext so the
// handler's auth gate + authTeamScope pass exactly as the §13 choke point would stamp them.
func inboxReq(principal string, teamID uuid.UUID) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/squad/inbox", nil)
	return r.WithContext(discussion.WithAuth(r.Context(), discussion.AuthorContext{
		Principal: principal,
		TeamID:    teamID,
	}))
}

func callInbox(t *testing.T, reviews fakeReviews, proposals fakeProposals, markers fakeMarkers, ov fakeOverview, req *http.Request) InboxResponse {
	t.Helper()
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.squadInbox(reviews, proposals, markers, ov, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("squadInbox: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp InboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// TestSquadInboxJoinSortUnread — the core join: a review item and a proposal are resolved to their
// project paths, ordered by run claimedAt (proposal's createdAt beats the review's older run),
// unread is derived from the marker store (review seen-after-run ⇒ read; proposal unmarked ⇒
// unread), and the live-run marker rides the overview cache.
func TestSquadInboxJoinSortUnread(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	t0 := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC) // review item updatedAt
	t1 := time.Date(2026, 10, 6, 11, 0, 0, 0, time.UTC) // review item's run claimedAt
	t2 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) // seen_at for the review item (after its run)
	t3 := time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC) // proposal createdAt (newest ⇒ sorts first)

	ov := fakeOverview{ov: SquadOverview{
		Projects: []ProjectOverview{{
			Name: "web", Namespace: "squad-a", UID: "uid-web",
			Runs: []RunStatus{{Name: "run-1", WorkItem: "wi-1", Phase: "Running", ClaimedAt: &t1}},
		}},
	}}
	reviews := fakeReviews{items: []coord.ReviewItem{
		{ID: "wi-1", ProjectID: "uid-web", Title: "Review me", UpdatedAt: t0, Assignee: "coder"},
	}}
	proposals := fakeProposals{items: []discussion.OpenProposalSummary{
		{MessageID: "m-9", ProjectUID: "squad-a/web", AuthorAgent: "mary", Body: "Proposal body", CreatedAt: t3},
	}}
	markers := fakeMarkers{seen: map[string]time.Time{"inReview:wi-1": t2}}

	resp := callInbox(t, reviews, proposals, markers, ov, inboxReq("user:alice", teamID))

	if len(resp.Items) != 2 {
		t.Fatalf("items: got %d, want 2 (%+v)", len(resp.Items), resp.Items)
	}
	// Sort: proposal (createdAt t3) before review (run claimedAt t1).
	prop, rev := resp.Items[0], resp.Items[1]
	if prop.Key != "proposal:m-9" || rev.Key != "inReview:wi-1" {
		t.Fatalf("sort order wrong: %s then %s", prop.Key, rev.Key)
	}
	// Review item: project resolved via UID index, live run marker, seen-after-run ⇒ read.
	if rev.ProjectID != "squad-a/web" {
		t.Fatalf("review projectId: got %q, want squad-a/web", rev.ProjectID)
	}
	if rev.TicketID != "wi-1" || rev.DecisionType != "review" || rev.RaisedByAgent != "coder" {
		t.Fatalf("review row fields wrong: %+v", rev)
	}
	if !rev.Live {
		t.Fatalf("review item should carry the live-run marker (run is Running): %+v", rev)
	}
	if rev.Unread {
		t.Fatalf("review item was seen after its run ⇒ must be read: %+v", rev)
	}
	if rev.LastRunAt == nil || !rev.LastRunAt.Equal(t1) {
		t.Fatalf("review lastRunAt: got %v, want %v", rev.LastRunAt, t1)
	}
	// Proposal: project path passed through (thread.project_id is already ns/name), unmarked ⇒ unread.
	if prop.ProjectID != "squad-a/web" || prop.DecisionType != "proposal" || prop.RaisedByAgent != "mary" {
		t.Fatalf("proposal row fields wrong: %+v", prop)
	}
	if !prop.Unread {
		t.Fatalf("unmarked proposal must be unread: %+v", prop)
	}
	if prop.Live || prop.LastRunAt != nil {
		t.Fatalf("proposal with no ticket run must be non-live with nil lastRunAt: %+v", prop)
	}
}

// TestSquadInboxUnreadResurfacesOnNewRun — a marker older than the item's latest run re-surfaces the
// item as unread ("needs me again", ADR-0026 §6). Also asserts an arm error degrades to zero rows
// from that arm rather than failing the whole response.
func TestSquadInboxUnreadResurfacesOnNewRun(t *testing.T) {
	teamID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	seenAt := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	newRun := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC) // a fresh run AFTER the last seen

	ov := fakeOverview{ov: SquadOverview{
		Projects: []ProjectOverview{{
			Name: "web", Namespace: "squad-a", UID: "uid-web",
			Runs: []RunStatus{{Name: "run-2", WorkItem: "wi-1", Phase: "Succeeded", ClaimedAt: &newRun}},
		}},
	}}
	reviews := fakeReviews{items: []coord.ReviewItem{
		{ID: "wi-1", ProjectID: "uid-web", Title: "Re-surfaced", UpdatedAt: seenAt},
	}}
	// Proposal arm errors ⇒ must contribute zero rows without failing the response.
	proposals := fakeProposals{err: context.DeadlineExceeded}
	markers := fakeMarkers{seen: map[string]time.Time{"inReview:wi-1": seenAt}}

	resp := callInbox(t, reviews, proposals, markers, ov, inboxReq("user:bob", teamID))

	if len(resp.Items) != 1 {
		t.Fatalf("items: got %d, want 1 (proposal arm error must drop to zero rows) %+v", len(resp.Items), resp.Items)
	}
	if !resp.Items[0].Unread {
		t.Fatalf("a run newer than the last-seen marker must re-surface the item as unread: %+v", resp.Items[0])
	}
}

// TestSquadInboxUnauthenticated — no AuthorContext on the request ⇒ 401 before any store read.
func TestSquadInboxUnauthenticated(t *testing.T) {
	srv := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/squad/inbox", nil)
	srv.squadInbox(fakeReviews{}, fakeProposals{}, fakeMarkers{}, fakeOverview{}, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated inbox: got %d, want 401", rec.Code)
	}
}
