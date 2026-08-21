package apiserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// 2.6 audit-trail query API tests (ISI-2881). The RBAC scoping, filter parsing,
// and cursor contract are proven against a fake reader through the REAL §13
// BFFAuthz middleware + root router, so the mounted surface (not just the
// handler) is what the tests exercise. The Postgres reader's SQL is covered by
// audittrail_integration_test.go (DATABASE_URL-gated) plus auditWhereClause
// unit tests below.
// ============================================================================

// fakeAuditReader records the last query and answers a canned page.
type fakeAuditReader struct {
	got     AuditTrailQuery
	queries int
	page    AuditTrailPage
	err     error
}

func (f *fakeAuditReader) Query(_ context.Context, q AuditTrailQuery) (AuditTrailPage, error) {
	f.got = q
	f.queries++
	return f.page, f.err
}

// auditHarness builds the host with the audit route mounted behind the §13
// choke point, resolving every request to the given AuthorContext.
func auditHarness(author discussion.AuthorContext, ok bool, reader AuditTrailReader) *Server {
	return NewServer(Options{
		Authenticator: &stubAuthenticator{author: author, ok: ok},
		AuditTrail:    reader,
	})
}

func auditGet(srv *Server, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/audit"+query, nil))
	return rec
}

func TestAuditTrailRequiresAuthentication(t *testing.T) {
	srv := auditHarness(discussion.AuthorContext{}, false, &fakeAuditReader{})
	rec := auditGet(srv, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: got %d, want 401", rec.Code)
	}
}

func TestAuditTrailDocumented501WhenReaderNil(t *testing.T) {
	srv := auditHarness(discussion.AuthorContext{Principal: "user:admin"}, true, nil)
	rec := auditGet(srv, "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("reader-less host: got %d, want 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ISI-2881") {
		t.Fatalf("501 body should name the tracking issue, got %s", rec.Body.String())
	}
}

func TestAuditTrailNonAdminIsSelfScoped(t *testing.T) {
	reader := &fakeAuditReader{}
	srv := auditHarness(discussion.AuthorContext{Principal: "user:jane"}, true, reader)

	rec := auditGet(srv, "?work_item="+uuid.New().String())
	if rec.Code != http.StatusOK {
		t.Fatalf("implicit self-scope: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if reader.got.Actor != "user:jane" {
		t.Fatalf("non-admin query not pinned to own principal: got %q", reader.got.Actor)
	}
}

func TestAuditTrailNonAdminExplicitSelfAllowed(t *testing.T) {
	reader := &fakeAuditReader{}
	srv := auditHarness(discussion.AuthorContext{Principal: "user:jane"}, true, reader)

	if rec := auditGet(srv, "?actor=user:jane"); rec.Code != http.StatusOK {
		t.Fatalf("explicit own actor: got %d, want 200", rec.Code)
	}
}

func TestAuditTrailNonAdminForeignActorDenied(t *testing.T) {
	reader := &fakeAuditReader{}
	srv := auditHarness(discussion.AuthorContext{Principal: "user:jane"}, true, reader)

	rec := auditGet(srv, "?actor=user:someone-else")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign actor for non-admin: got %d, want 403", rec.Code)
	}
	if reader.queries != 0 {
		t.Fatalf("denied request must not reach the reader")
	}
}

func TestAuditTrailAdminQueriesAnyActor(t *testing.T) {
	reader := &fakeAuditReader{}
	srv := auditHarness(discussion.AuthorContext{Principal: "user:admin", IsAdmin: true}, true, reader)

	if rec := auditGet(srv, "?actor=agent:coder&event_type=claim_acquired"); rec.Code != http.StatusOK {
		t.Fatalf("admin filtered query: got %d, want 200", rec.Code)
	}
	if reader.got.Actor != "agent:coder" || reader.got.EventType != "claim_acquired" {
		t.Fatalf("admin filters not passed through: %+v", reader.got)
	}
	// No implicit scoping: an admin with no actor filter sees the whole trail.
	if rec := auditGet(srv, ""); rec.Code != http.StatusOK || reader.got.Actor != "" {
		t.Fatalf("admin unfiltered query should carry no actor scope, got %q", reader.got.Actor)
	}
}

func TestAuditTrailParsesFilters(t *testing.T) {
	reader := &fakeAuditReader{}
	srv := auditHarness(discussion.AuthorContext{Principal: "user:admin", IsAdmin: true}, true, reader)

	wi := uuid.New()
	run := uuid.New()
	from := "2026-08-01T00:00:00Z"
	to := "2026-08-21T23:59:59Z"
	rec := auditGet(srv, "?work_item="+wi.String()+"&run="+run.String()+"&from="+from+"&to="+to+"&before=42&limit=25")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	q := reader.got
	if q.WorkItemID == nil || *q.WorkItemID != wi {
		t.Fatalf("work_item filter not parsed: %+v", q.WorkItemID)
	}
	if q.RunID == nil || *q.RunID != run {
		t.Fatalf("run filter not parsed: %+v", q.RunID)
	}
	if q.From == nil || !q.From.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("from filter not parsed: %+v", q.From)
	}
	if q.To == nil || !q.To.Equal(time.Date(2026, 8, 21, 23, 59, 59, 0, time.UTC)) {
		t.Fatalf("to filter not parsed: %+v", q.To)
	}
	if q.Before != 42 {
		t.Fatalf("before cursor not parsed: %d", q.Before)
	}
	if q.Limit != 25 {
		t.Fatalf("limit not parsed: %d", q.Limit)
	}
}

func TestAuditTrailRejectsBadFilters(t *testing.T) {
	srv := auditHarness(discussion.AuthorContext{Principal: "user:admin", IsAdmin: true}, true, &fakeAuditReader{})
	cases := map[string]string{
		"bad work_item": "?work_item=not-a-uuid",
		"bad run":       "?run=not-a-uuid",
		"bad from":      "?from=august",
		"bad to":        "?to=2026-13-99",
		"bad before":    "?before=-3",
	}
	for name, query := range cases {
		if rec := auditGet(srv, query); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", name, rec.Code)
		}
	}
}

func TestAuditTrailLimitClamped(t *testing.T) {
	reader := &fakeAuditReader{}
	srv := auditHarness(discussion.AuthorContext{Principal: "user:admin", IsAdmin: true}, true, reader)

	for q, want := range map[string]int{"?limit=0": 50, "?limit=999": 50, "?limit=200": 200, "": 50} {
		if rec := auditGet(srv, q); rec.Code != http.StatusOK {
			t.Fatalf("limit %q: got %d, want 200", q, rec.Code)
		}
		if reader.got.Limit != want {
			t.Fatalf("limit %q: got %d, want %d", q, reader.got.Limit, want)
		}
	}
}

func TestAuditTrailCursorAndShape(t *testing.T) {
	next := int64(7)
	ts := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	wi := uuid.New().String()
	reader := &fakeAuditReader{page: AuditTrailPage{
		Events: []AuditEvent{{
			ID: 42, WorkItemID: &wi, EventType: "claim_acquired", Principal: "agent:coder",
			ToState: ptrStr("in_progress"), Payload: json.RawMessage(`{"detail":"ok"}`), CreatedAt: ts,
		}},
		NextBefore: &next,
	}}
	srv := auditHarness(discussion.AuthorContext{Principal: "user:admin", IsAdmin: true}, true, reader)

	rec := auditGet(srv, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var body struct {
		Events     []AuditEvent `json:"events"`
		NextBefore *int64       `json:"nextBefore"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Events) != 1 || body.Events[0].EventType != "claim_acquired" {
		t.Fatalf("event not relayed: %+v", body.Events)
	}
	if body.Events[0].WorkItemID == nil || *body.Events[0].WorkItemID != wi {
		t.Fatalf("work item id not relayed: %+v", body.Events[0].WorkItemID)
	}
	if body.NextBefore == nil || *body.NextBefore != 7 {
		t.Fatalf("nextBefore cursor not relayed: %+v", body.NextBefore)
	}
}

func TestAuditTrailReaderErrorSurfaces502(t *testing.T) {
	srv := auditHarness(discussion.AuthorContext{Principal: "user:admin", IsAdmin: true}, true,
		&fakeAuditReader{err: ErrAuditTrailUnavailable})
	if rec := auditGet(srv, ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("reader failure: got %d, want 502", rec.Code)
	}
}

func TestAuditWhereClause(t *testing.T) {
	wi := uuid.New()
	run := uuid.New()
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	clause, args := auditWhereClause(AuditTrailQuery{})
	if clause != "" || args != nil {
		t.Fatalf("empty query should have no WHERE, got %q %v", clause, args)
	}

	clause, args = auditWhereClause(AuditTrailQuery{
		WorkItemID: &wi, RunID: &run, Actor: "agent:coder", EventType: "claim_acquired",
		From: &from, Before: 9,
	})
	for _, want := range []string{
		"work_item_id = $1", "run_id = $2", "principal = $3", "event_type = $4",
		"created_at >= $5", "id < $6",
	} {
		if !strings.Contains(clause, want) {
			t.Fatalf("clause %q missing %q", clause, want)
		}
	}
	if len(args) != 6 {
		t.Fatalf("want 6 bound args, got %d (%v)", len(args), args)
	}
	if args[0] != wi.String() || args[2] != "agent:coder" || args[5] != int64(9) {
		t.Fatalf("bound args wrong: %v", args)
	}
}

func ptrStr(s string) *string { return &s }

// ── PostgresAuditTrailReader unit lane (canned rows) ──────────────────────────
//
// The scan/cursor/error paths of Query run here against a fake auditQueryer;
// the SQL text + column order against real Postgres live in the integration
// lane (audittrail_integration_test.go, discussion_integration tag).

// fakeAuditRows serves canned []any scan tuples.
type fakeAuditRows struct {
	tuples [][]any
	i      int
	err    error
}

func (f *fakeAuditRows) Next() bool { f.i++; return f.i <= len(f.tuples) }
func (f *fakeAuditRows) Scan(dest ...any) error {
	if len(dest) != len(f.tuples[f.i-1]) {
		panic("fakeAuditRows: tuple arity mismatch")
	}
	for i, d := range dest {
		switch dt := d.(type) {
		case *int64:
			*dt = f.tuples[f.i-1][i].(int64)
		case *string:
			*dt = f.tuples[f.i-1][i].(string)
		case *sql.NullString:
			*dt = f.tuples[f.i-1][i].(sql.NullString)
		case *sql.NullInt64:
			*dt = f.tuples[f.i-1][i].(sql.NullInt64)
		case *time.Time:
			*dt = f.tuples[f.i-1][i].(time.Time)
		default:
			panic("fakeAuditRows: unsupported dest type")
		}
	}
	return nil
}
func (f *fakeAuditRows) Err() error   { return f.err }
func (f *fakeAuditRows) Close() error { return nil }

type stubAuditQueryer struct {
	gotQuery string
	gotArgs  []any
	rows     *fakeAuditRows
	err      error
}

func (s *stubAuditQueryer) QueryContext(_ context.Context, query string, args ...any) (auditRows, error) {
	s.gotQuery, s.gotArgs = query, args
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func auditTuple(id int64, wi, run sql.NullString, eventType, principal string, payload sql.NullString, ts time.Time) []any {
	return []any{id, wi, run, eventType, principal, sql.NullString{}, sql.NullInt64{}, sql.NullString{}, sql.NullString{}, payload, ts}
}

func TestAuditTrailReaderScanAndCursor(t *testing.T) {
	ts := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	wi := uuid.New().String()
	run := uuid.New().String()

	// Limit 2 → fetch 3 (limit+1): two rows served, cursor = row 2's id.
	stub := &stubAuditQueryer{rows: &fakeAuditRows{tuples: [][]any{
		auditTuple(30, sql.NullString{String: wi, Valid: true}, sql.NullString{String: run, Valid: true}, "run_terminal", "agent:coder", sql.NullString{String: `{"to":"succeeded"}`, Valid: true}, ts),
		auditTuple(20, sql.NullString{}, sql.NullString{}, "comment_added", "user:jane", sql.NullString{}, ts),
		auditTuple(10, sql.NullString{}, sql.NullString{}, "completed", "user:jane", sql.NullString{}, ts),
	}}}
	reader := &PostgresAuditTrailReader{query: stub}

	page, err := reader.Query(context.Background(), AuditTrailQuery{Limit: 2})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("want 2 served rows, got %d", len(page.Events))
	}
	if page.NextBefore == nil || *page.NextBefore != 20 {
		t.Fatalf("cursor must be the last SERVED id (20), got %v", page.NextBefore)
	}
	first := page.Events[0]
	if first.ID != 30 || first.EventType != "run_terminal" || first.Principal != "agent:coder" {
		t.Fatalf("row 0 wrong: %+v", first)
	}
	if first.WorkItemID == nil || *first.WorkItemID != wi || first.RunID == nil || *first.RunID != run {
		t.Fatalf("uuid scans wrong: %+v %+v", first.WorkItemID, first.RunID)
	}
	if string(first.Payload) != `{"to":"succeeded"}` {
		t.Fatalf("payload scan wrong: %s", first.Payload)
	}
	if page.Events[1].WorkItemID != nil || page.Events[1].Payload != nil {
		t.Fatalf("NULL scans must leave optional fields nil: %+v", page.Events[1])
	}

	// Fetch arity: unfiltered query ⇒ the ONLY bound arg is the fetch limit (limit+1).
	if len(stub.gotArgs) != 1 || stub.gotArgs[0] != 3 {
		t.Fatalf("fetch must be limit+1 as the LAST bound arg: %v", stub.gotArgs)
	}
	if !strings.Contains(stub.gotQuery, "ORDER BY id DESC LIMIT $1") {
		t.Fatalf("query shape wrong: %s", stub.gotQuery)
	}
}

func TestAuditTrailReaderTailHasNoCursor(t *testing.T) {
	stub := &stubAuditQueryer{rows: &fakeAuditRows{tuples: [][]any{
		auditTuple(5, sql.NullString{}, sql.NullString{}, "completed", "user:jane", sql.NullString{}, time.Now()),
	}}}
	reader := &PostgresAuditTrailReader{query: stub}
	page, err := reader.Query(context.Background(), AuditTrailQuery{Limit: 50})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Events) != 1 || page.NextBefore != nil {
		t.Fatalf("short page must have no cursor: %d events, cursor %v", len(page.Events), page.NextBefore)
	}
}

func TestAuditTrailReaderZeroLimitDefaults(t *testing.T) {
	stub := &stubAuditQueryer{rows: &fakeAuditRows{}}
	reader := &PostgresAuditTrailReader{query: stub}
	if _, err := reader.Query(context.Background(), AuditTrailQuery{}); err != nil {
		t.Fatalf("query: %v", err)
	}
	if stub.gotArgs[len(stub.gotArgs)-1] != 51 {
		t.Fatalf("zero limit must default to 50 (fetch 51), got %v", stub.gotArgs[len(stub.gotArgs)-1])
	}
}

func TestAuditTrailReaderErrorsSurfaceUnavailable(t *testing.T) {
	reader := &PostgresAuditTrailReader{query: &stubAuditQueryer{err: ErrAuditTrailUnavailable}}
	if _, err := reader.Query(context.Background(), AuditTrailQuery{Limit: 5}); !errors.Is(err, ErrAuditTrailUnavailable) {
		t.Fatalf("exec error must map to ErrAuditTrailUnavailable, got %v", err)
	}

	// rows.Err() (network drop mid-scan) and a scan failure map the same way.
	reader = &PostgresAuditTrailReader{query: &stubAuditQueryer{
		rows: &fakeAuditRows{err: sql.ErrConnDone},
	}}
	if _, err := reader.Query(context.Background(), AuditTrailQuery{Limit: 5}); !errors.Is(err, ErrAuditTrailUnavailable) {
		t.Fatalf("rows error must map to ErrAuditTrailUnavailable, got %v", err)
	}
}

// Compile-time interface checks.
var (
	_ AuditTrailReader = (*PostgresAuditTrailReader)(nil)
	_ AuditTrailReader = (*fakeAuditReader)(nil)
	_ auditQueryer     = (*stubAuditQueryer)(nil)
	_ auditRows        = (*fakeAuditRows)(nil)
	_ error            = ErrAuditTrailUnavailable
)
