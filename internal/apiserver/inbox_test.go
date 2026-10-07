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
	"strings"
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

type fakeDecisions struct {
	items []discussion.OpenDecisionSummary
	err   error
}

func (f fakeDecisions) ListOpenDecisionRequestsForTeam(context.Context, string) ([]discussion.OpenDecisionSummary, error) {
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
func (f fakeMarkers) MarkSeen(context.Context, string, []string) error   { return nil }
func (f fakeMarkers) MarkUnread(context.Context, string, []string) error { return nil }

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
	return callInboxWithDecisions(t, reviews, proposals, fakeDecisions{}, markers, ov, req)
}

func callInboxWithDecisions(t *testing.T, reviews fakeReviews, proposals fakeProposals, decisions fakeDecisions, markers fakeMarkers, ov fakeOverview, req *http.Request) InboxResponse {
	t.Helper()
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.squadInbox(reviews, proposals, decisions, markers, ov, nil).ServeHTTP(rec, req)
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

// TestSquadInboxDecisionArm — ISI-5536 BE-7: an open decision_request surfaces as its own row, keyed
// decision:{messageId}, with decisionType=the card's mode, and run-joined (lastRunAt/live) through the
// bound work item exactly like the proposal arm (ADR-0026 §3.3/§3.4).
func TestSquadInboxDecisionArm(t *testing.T) {
	teamID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	run := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)

	ov := fakeOverview{ov: SquadOverview{
		Projects: []ProjectOverview{{
			Name: "web", Namespace: "squad-a", UID: "uid-web",
			Runs: []RunStatus{{Name: "run-d", WorkItem: "wi-7", Phase: "Running", ClaimedAt: &run}},
		}},
	}}
	decisions := fakeDecisions{items: []discussion.OpenDecisionSummary{
		{MessageID: "dm-1", ProjectUID: "squad-a/web", AuthorAgent: "winston",
			Title: "Which HTTP client?", Mode: "choose_one", TicketID: "wi-7",
			CreatedAt: time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC),
			Options: []discussion.DecisionOption{
				{ID: "reqwest", Label: "reqwest", Recommended: true},
				{ID: "hyper", Label: "hyper"},
			},
			AllowFreeText: true, AllowReject: true, RejectRequiresReason: false},
	}}

	resp := callInboxWithDecisions(t, fakeReviews{}, fakeProposals{}, decisions, fakeMarkers{}, ov, inboxReq("user:alice", teamID))

	if len(resp.Items) != 1 {
		t.Fatalf("items: got %d, want 1 (%+v)", len(resp.Items), resp.Items)
	}
	d := resp.Items[0]
	if d.Key != "decision:dm-1" || d.DecisionType != "choose_one" {
		t.Fatalf("decision row key/type wrong: %+v", d)
	}
	if d.TicketID != "wi-7" || d.ProjectID != "squad-a/web" || d.RaisedByAgent != "winston" || d.Title != "Which HTTP client?" {
		t.Fatalf("decision row fields wrong: %+v", d)
	}
	if !d.Live || d.LastRunAt == nil || !d.LastRunAt.Equal(run) {
		t.Fatalf("decision row should run-join its bound ticket (live + lastRunAt=%v): %+v", run, d)
	}
	if !d.Unread {
		t.Fatalf("unmarked decision must be unread: %+v", d)
	}
	// ISI-5537 E3: the inline-answer affordance is carried for decision rows.
	if d.Decision == nil {
		t.Fatalf("decision row must carry the inline-answer Decision block: %+v", d)
	}
	if d.Decision.MessageID != "dm-1" || !d.Decision.AllowFreeText || !d.Decision.AllowReject {
		t.Fatalf("inline decision fields wrong: %+v", d.Decision)
	}
	if len(d.Decision.Options) != 2 || d.Decision.Options[0].ID != "reqwest" || !d.Decision.Options[0].Recommended {
		t.Fatalf("inline options wrong: %+v", d.Decision.Options)
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
	srv.squadInbox(fakeReviews{}, fakeProposals{}, fakeDecisions{}, fakeMarkers{}, fakeOverview{}, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated inbox: got %d, want 401", rec.Code)
	}
}

// recordingReviews captures the teamID the inbox handler scopes the review arm to, so we can prove
// ?scope=mine narrows an admin's fleet read back to their own team (ISI-5537 E3, OQ5).
type recordingReviews struct{ gotTeam string }

func (f *recordingReviews) ListReviewItems(_ context.Context, teamID string) ([]coord.ReviewItem, error) {
	f.gotTeam = teamID
	return nil, nil
}

// adminInboxReq builds an admin-scoped GET with the given query string so scope=mine can be exercised.
func adminInboxReq(principal string, teamID uuid.UUID, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/squad/inbox"+query, nil)
	return r.WithContext(discussion.WithAuth(r.Context(), discussion.AuthorContext{
		Principal: principal,
		TeamID:    teamID,
		IsAdmin:   true,
	}))
}

// TestSquadInboxScopeMineNarrowsAdmin — an admin's default read is fleet-wide (team scope ""), but
// ?scope=mine narrows it back to the admin's own team and flips the Fleet flag off.
func TestSquadInboxScopeMineNarrowsAdmin(t *testing.T) {
	teamID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	srv := &Server{}

	// Default (fleet): the review arm is scoped to "" and the response is a fleet view.
	rev := &recordingReviews{}
	rec := httptest.NewRecorder()
	srv.squadInbox(rev, fakeProposals{}, fakeDecisions{}, fakeMarkers{}, fakeOverview{}, nil).
		ServeHTTP(rec, adminInboxReq("user:admin", teamID, ""))
	if rev.gotTeam != "" {
		t.Fatalf("admin default must read fleet (team \"\"), got %q", rev.gotTeam)
	}
	var fleetResp InboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &fleetResp); err != nil {
		t.Fatalf("decode fleet resp: %v", err)
	}
	if !fleetResp.Fleet {
		t.Fatalf("admin default response must be Fleet=true")
	}

	// scope=mine: the review arm is scoped to the admin's team and Fleet flips off.
	revMine := &recordingReviews{}
	recMine := httptest.NewRecorder()
	srv.squadInbox(revMine, fakeProposals{}, fakeDecisions{}, fakeMarkers{}, fakeOverview{}, nil).
		ServeHTTP(recMine, adminInboxReq("user:admin", teamID, "?scope=mine"))
	if revMine.gotTeam != teamID.String() {
		t.Fatalf("scope=mine must narrow to admin team %s, got %q", teamID, revMine.gotTeam)
	}
	var mineResp InboxResponse
	if err := json.Unmarshal(recMine.Body.Bytes(), &mineResp); err != nil {
		t.Fatalf("decode mine resp: %v", err)
	}
	if mineResp.Fleet {
		t.Fatalf("scope=mine response must be Fleet=false")
	}
}

// recordingMarker captures the last MarkUnread call so the unseen handler can be asserted end to end.
type recordingMarker struct {
	fakeMarkers
	unreadKeys []string
}

func (m *recordingMarker) MarkUnread(_ context.Context, _ string, keys []string) error {
	m.unreadKeys = keys
	return nil
}

// TestSquadInboxUnseen — POST /api/squad/inbox/unseen drops the given keys' markers (the "mark
// unread" half of the toggle); empty body is 400, missing auth is 401.
func TestSquadInboxUnseen(t *testing.T) {
	srv := &Server{}
	mk := &recordingMarker{}

	// Happy path: keys forwarded to MarkUnread, 200.
	body := strings.NewReader(`{"keys":["decision:dm-1","inReview:wi-2"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/squad/inbox/unseen", body)
	req = req.WithContext(discussion.WithAuth(req.Context(), discussion.AuthorContext{Principal: "user:alice"}))
	rec := httptest.NewRecorder()
	srv.squadInboxUnseen(mk).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unseen: got %d, want 200", rec.Code)
	}
	if len(mk.unreadKeys) != 2 || mk.unreadKeys[0] != "decision:dm-1" {
		t.Fatalf("unseen must forward keys to MarkUnread, got %+v", mk.unreadKeys)
	}

	// Empty keys ⇒ 400.
	recBad := httptest.NewRecorder()
	reqBad := httptest.NewRequest(http.MethodPost, "/api/squad/inbox/unseen", strings.NewReader(`{"keys":[]}`))
	reqBad = reqBad.WithContext(discussion.WithAuth(reqBad.Context(), discussion.AuthorContext{Principal: "user:alice"}))
	srv.squadInboxUnseen(mk).ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("unseen empty keys: got %d, want 400", recBad.Code)
	}

	// Missing auth ⇒ 401.
	recNoAuth := httptest.NewRecorder()
	reqNoAuth := httptest.NewRequest(http.MethodPost, "/api/squad/inbox/unseen", strings.NewReader(`{"keys":["x"]}`))
	srv.squadInboxUnseen(mk).ServeHTTP(recNoAuth, reqNoAuth)
	if recNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("unseen no auth: got %d, want 401", recNoAuth.Code)
	}
}
