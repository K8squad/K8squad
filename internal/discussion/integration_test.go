//go:build discussion_integration

// Apiserver integration test for the discussion surface against a REAL Postgres (Story 10.1 / ISI-2709,
// AC3 + AC5). Build-tag gated so it never runs in the default unit lane; CI provisions Postgres and runs
//
//	go test -tags=discussion_integration ./internal/discussion/...
//
// It applies the SHIPPED migration (db/migrations/0004_discussion_schema.sql) — not inline DDL — so a
// drift between the migration and the code goes RED here. When DATABASE_URL is unset the test SKIPS
// (mirrors the coord chaos gate), so a developer without Postgres is not blocked.
package discussion

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

// headerAuth authenticates from test headers, standing in for the §13 BFF: X-Test-Principal →
// principal, X-Test-Team → Team scope, X-Test-Agent → agent-authored (optional), X-Test-Run → Run
// linkage (optional). This is the ONLY place identity/tenancy enters — exactly the production seam.
type headerAuth struct{}

func (headerAuth) Authenticate(r *http.Request) (AuthorContext, bool) {
	p := r.Header.Get("X-Test-Principal")
	if p == "" {
		return AuthorContext{}, false
	}
	team, err := uuid.Parse(r.Header.Get("X-Test-Team"))
	if err != nil {
		return AuthorContext{}, false
	}
	a := AuthorContext{Principal: p, TeamID: team}
	if v := r.Header.Get("X-Test-Agent"); v != "" {
		a.AgentID = &v
	}
	if v := r.Header.Get("X-Test-Run"); v != "" {
		a.RunID = &v
	}
	return a, true
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the discussion apiserver integration test (needs real Postgres)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return db
}

// applyMigration applies the SHIPPED 0004 migration into a clean `discussion` schema.
func applyMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS discussion CASCADE`); err != nil {
		t.Fatalf("reset discussion schema: %v", err)
	}
	candidates := []string{
		filepath.Join("..", "..", "db", "migrations", "0004_discussion_schema.sql"),
		filepath.Join("db", "migrations", "0004_discussion_schema.sql"),
	}
	if d := os.Getenv("DISCUSSION_MIGRATIONS_DIR"); d != "" {
		candidates = append([]string{filepath.Join(d, "0004_discussion_schema.sql")}, candidates...)
	}
	var sqlBytes []byte
	var err error
	for _, c := range candidates {
		if sqlBytes, err = os.ReadFile(c); err == nil {
			break
		}
	}
	if sqlBytes == nil {
		t.Fatalf("could not locate 0004_discussion_schema.sql (tried %v); set DISCUSSION_MIGRATIONS_DIR", candidates)
	}
	if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("the shipped discussion migration failed to apply against real Postgres: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = db.ExecContext(cctx, `DROP SCHEMA IF EXISTS discussion CASCADE`)
	})
}

func newServer(db *sql.DB) *httptest.Server {
	r := mux.NewRouter()
	NewHandler(NewStore(db)).Mount(r, headerAuth{})
	return httptest.NewServer(r)
}

type postResp struct {
	ID              uuid.UUID `json:"id"`
	AuthorPrincipal string    `json:"authorPrincipal"`
	Messages        []struct {
		ID              uuid.UUID `json:"id"`
		AuthorPrincipal string    `json:"authorPrincipal"`
	} `json:"messages"`
}

// TestServerStampsAuthorIgnoringForgedBody is AC3 against real Postgres: an authenticated caller posts
// a body that ALSO carries forged author_* fields; the STORED row must show the AUTHENTICATED principal,
// never the forged value. Impersonation is un-representable.
func TestServerStampsAuthorIgnoringForgedBody(t *testing.T) {
	db := openTestDB(t)
	applyMigration(t, db)
	srv := newServer(db)
	defer srv.Close()

	teamA := uuid.New()
	projectA := uuid.New()

	// Open a thread as principal:real, but smuggle a forged author into the body.
	forged := `{"title":"design sync","body":"kickoff",` +
		`"author_principal":"principal:VICTIM","authorId":"` + uuid.NewString() + `",` +
		`"authorType":"human","authorName":"attacker","created_by":"principal:VICTIM"}`
	req := mustReq(t, http.MethodPost, srv.URL+"/api/projects/"+projectA.String()+"/discussion/threads",
		forged, "principal:real", teamA.String(), "", "")
	res := do(t, req)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("openThread: got %d, want 201", res.StatusCode)
	}
	var opened postResp
	decode(t, res, &opened)
	if opened.AuthorPrincipal != "principal:real" {
		t.Errorf("thread created_by: got %q, want principal:real (forged body author must be ignored)", opened.AuthorPrincipal)
	}
	if len(opened.Messages) != 1 || opened.Messages[0].AuthorPrincipal != "principal:real" {
		t.Errorf("first message author: got %+v, want principal:real", opened.Messages)
	}

	// Confirm directly in the row that NO forged value landed.
	var storedAuthor, storedCreatedBy string
	if err := db.QueryRow(`
		SELECT m.author_principal, t.created_by
		FROM discussion.message m JOIN discussion.thread t ON t.id = m.thread_id
		WHERE t.id = $1`, opened.ID).Scan(&storedAuthor, &storedCreatedBy); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if storedAuthor != "principal:real" || storedCreatedBy != "principal:real" {
		t.Errorf("stored row shows forged author: author=%q created_by=%q", storedAuthor, storedCreatedBy)
	}
	if strings.Contains(storedAuthor, "VICTIM") {
		t.Error("AC3 VIOLATION: forged principal reached the stored row")
	}
}

// TestCrossTeamReadsAreEmpty is AC5 against real Postgres: Team A opens a thread in its Project; Team B
// (a different Team scope) reading the SAME Project sees zero threads and gets 404 (not 403) on the
// specific thread. Tenancy is enforced in the query, deny-by-default.
func TestCrossTeamReadsAreEmpty(t *testing.T) {
	db := openTestDB(t)
	applyMigration(t, db)
	srv := newServer(db)
	defer srv.Close()

	teamA := uuid.New()
	teamB := uuid.New()
	project := uuid.New() // same Project id probed by both teams

	// Team A opens a thread.
	req := mustReq(t, http.MethodPost, srv.URL+"/api/projects/"+project.String()+"/discussion/threads",
		`{"title":"team A only","body":"secret"}`, "principal:a", teamA.String(), "", "")
	res := do(t, req)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("teamA openThread: got %d, want 201", res.StatusCode)
	}
	var opened postResp
	decode(t, res, &opened)

	// Team A lists — sees its thread.
	var aThreads []json.RawMessage
	listReq := mustReq(t, http.MethodGet, srv.URL+"/api/projects/"+project.String()+"/discussion/threads",
		"", "principal:a", teamA.String(), "", "")
	decode(t, do(t, listReq), &aThreads)
	if len(aThreads) != 1 {
		t.Fatalf("teamA list: got %d threads, want 1", len(aThreads))
	}

	// Team B lists the SAME Project — must see ZERO threads (AC5).
	var bThreads []json.RawMessage
	bListReq := mustReq(t, http.MethodGet, srv.URL+"/api/projects/"+project.String()+"/discussion/threads",
		"", "principal:b", teamB.String(), "", "")
	decode(t, do(t, bListReq), &bThreads)
	if len(bThreads) != 0 {
		t.Errorf("AC5 VIOLATION: teamB saw %d threads from teamA's Project, want 0", len(bThreads))
	}

	// Team B fetches Team A's specific thread — must be 404-not-403 (absence, not forbidden).
	getReq := mustReq(t, http.MethodGet,
		srv.URL+"/api/projects/"+project.String()+"/discussion/threads/"+opened.ID.String(),
		"", "principal:b", teamB.String(), "", "")
	getRes := do(t, getReq)
	if getRes.StatusCode != http.StatusNotFound {
		t.Errorf("AC5: teamB GET teamA thread: got %d, want 404 (not 403)", getRes.StatusCode)
	}
}

// TestAgentVsHumanDerivedAndSoftRetract exercises the provenance triple + soft-retract end to end.
func TestAgentVsHumanDerivedAndSoftRetract(t *testing.T) {
	db := openTestDB(t)
	applyMigration(t, db)
	srv := newServer(db)
	defer srv.Close()

	team := uuid.New()
	project := uuid.New()

	// Agent-authored post (X-Test-Agent set): author_agent_id must be stamped, author_run_id too.
	req := mustReq(t, http.MethodPost, srv.URL+"/api/projects/"+project.String()+"/discussion/threads",
		`{"title":"t","body":"agent says hi"}`, "principal:agent", team.String(), "agent:coordinator", "run:123")
	var opened postResp
	decode(t, do(t, req), &opened)

	var agentID, runID sql.NullString
	if err := db.QueryRow(`SELECT author_agent_id, author_run_id FROM discussion.message
		WHERE thread_id = $1`, opened.ID).Scan(&agentID, &runID); err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if !agentID.Valid || agentID.String != "agent:coordinator" {
		t.Errorf("author_agent_id: got %v, want agent:coordinator", agentID)
	}
	if !runID.Valid || runID.String != "run:123" {
		t.Errorf("author_run_id: got %v, want run:123", runID)
	}

	// Soft-retract the first message; a re-read of the thread must exclude it.
	msgID := opened.Messages[0].ID
	patch := mustReq(t, http.MethodPatch,
		srv.URL+"/api/projects/"+project.String()+"/discussion/threads/"+opened.ID.String()+"/messages/"+msgID.String(),
		"", "principal:agent", team.String(), "agent:coordinator", "run:123")
	if pr := do(t, patch); pr.StatusCode != http.StatusOK {
		t.Fatalf("retract: got %d, want 200", pr.StatusCode)
	}
	var live int
	if err := db.QueryRow(`SELECT count(*) FROM discussion.message
		WHERE thread_id = $1 AND invalidated_at IS NULL`, opened.ID).Scan(&live); err != nil {
		t.Fatalf("count live: %v", err)
	}
	if live != 0 {
		t.Errorf("after retract: %d live messages, want 0", live)
	}
}

// ── tiny HTTP helpers ───────────────────────────────────────────────────────

func mustReq(t *testing.T, method, url, body, principal, team, agent, run string) *http.Request {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if principal != "" {
		req.Header.Set("X-Test-Principal", principal)
	}
	if team != "" {
		req.Header.Set("X-Test-Team", team)
	}
	if agent != "" {
		req.Header.Set("X-Test-Agent", agent)
	}
	if run != "" {
		req.Header.Set("X-Test-Run", run)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return res
}

func decode(t *testing.T, res *http.Response, v any) {
	t.Helper()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
