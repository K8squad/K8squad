package orgops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/taskio"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 7)
	}
	return k
}

// fakeStore records the last verb reached (so a test can assert a call was
// ADMITTED past the scope gate) and returns a canned result/error.
type fakeStore struct {
	reached string
	err     error
}

func (f *fakeStore) CreateAgent(_ context.Context, _ taskio.RunToken, _ AgentInput) (Result, error) {
	f.reached = "create-agent"
	return Result{Kind: "Agent", Name: "a", Namespace: "ns", Operation: "created"}, f.err
}
func (f *fakeStore) CreateSkill(_ context.Context, _ taskio.RunToken, _ SkillInput) (Result, error) {
	f.reached = "create-skill"
	return Result{Kind: "Skill", Name: "s", Namespace: "ns", Operation: "created"}, f.err
}
func (f *fakeStore) CreateProject(_ context.Context, _ taskio.RunToken, _ ProjectInput) (Result, error) {
	f.reached = "create-project"
	return Result{Kind: "Project", Name: "p", Namespace: "ns", Operation: "created"}, f.err
}
func (f *fakeStore) ArchiveProject(_ context.Context, _ taskio.RunToken, name string) (Result, error) {
	f.reached = "archive-project"
	return Result{Kind: "Project", Name: name, Namespace: "ns", Operation: "archived"}, f.err
}
func (f *fakeStore) Assign(_ context.Context, _ taskio.RunToken, req AssignInput) (AssignResult, error) {
	f.reached = "assign"
	return AssignResult{ToAgent: req.ToAgent, A2A: A2ACarrier{Verb: "SubmitTask", TargetAgent: req.ToAgent, AgentCardRef: "ns/" + req.ToAgent}, CoordEnqueue: "/api/task-io/subtask"}, f.err
}

func newTestHandler(t *testing.T, store Store) (http.Handler, *taskio.Minter) {
	t.Helper()
	m, err := taskio.NewMinter(testKey(), time.Hour)
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return NewHandler(m, store).Mux(), m
}

func mint(t *testing.T, m *taskio.Minter, scopes []string) string {
	t.Helper()
	tok, err := m.MintWithScopes("run-A", "wi-1", "agent", scopes)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

func do(t *testing.T, h http.Handler, op, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+op, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const agentBody = `{"name":"new-agent","runtimeRef":{"name":"rt"},"roleRef":{"name":"coder"},"model":"m","credentialSecretRef":{"name":"cred"}}`

// No bearer token → 401, and the store is never reached.
func TestNoTokenIs401(t *testing.T) {
	fs := &fakeStore{}
	h, _ := newTestHandler(t, fs)
	rec := do(t, h, "agents", "", agentBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	if fs.reached != "" {
		t.Fatalf("store reached without auth: %q", fs.reached)
	}
}

// A verified token with NO scope (an IC run) is denied 403 on every privileged
// verb — the store is never reached. This is the loophole-closing gate: even if
// the caller holds the org-ops skill body, its token lacks the scope.
func TestICTokenDeniedAllVerbs(t *testing.T) {
	for _, op := range []string{"agents", "skills", "assign", "projects", "archive-project"} {
		fs := &fakeStore{}
		h, m := newTestHandler(t, fs)
		rec := do(t, h, op, mint(t, m, nil), `{}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s with IC token: got %d, want 403", op, rec.Code)
		}
		if fs.reached != "" {
			t.Fatalf("%s: store reached despite missing scope: %q", op, fs.reached)
		}
	}
}

// org:write authorizes create-agent/create-skill but NOT create-project/
// archive-project — a manager cannot create or archive projects.
func TestOrgWriteScopeBoundary(t *testing.T) {
	// org:write authorizes agents/skills/assign; projects/archive-project need
	// project:write. wantCode 201 = created, 200 = assign ok, 403 = out of scope.
	cases := []struct {
		op       string
		body     string
		wantCode int
	}{
		{"agents", agentBody, http.StatusCreated},
		{"skills", `{"name":"sk","source":{"type":"inline","inline":"body"}}`, http.StatusCreated},
		{"assign", `{"toAgent":"coder"}`, http.StatusOK},
		{"projects", `{"name":"p","repo":{"url":"https://x"}}`, http.StatusForbidden},
		{"archive-project", `{"name":"p"}`, http.StatusForbidden},
	}
	for _, c := range cases {
		fs := &fakeStore{}
		h, m := newTestHandler(t, fs)
		rec := do(t, h, c.op, mint(t, m, []string{ScopeOrgWrite}), c.body)
		if rec.Code != c.wantCode {
			t.Fatalf("%s with org:write: got %d, want %d (body %s)", c.op, rec.Code, c.wantCode, rec.Body.String())
		}
		if c.wantCode == http.StatusForbidden && fs.reached != "" {
			t.Fatalf("%s: project verb reached store with only org:write: %q", c.op, fs.reached)
		}
		if c.wantCode != http.StatusForbidden && fs.reached == "" {
			t.Fatalf("%s: store not reached", c.op)
		}
	}
}

// project:write authorizes create-project/archive-project.
func TestProjectWriteAllowsProjectVerbs(t *testing.T) {
	fs := &fakeStore{}
	h, m := newTestHandler(t, fs)
	tok := mint(t, m, []string{ScopeOrgWrite, ScopeProjectWrite})

	rec := do(t, h, "projects", tok, `{"name":"p","repo":{"url":"https://x"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("projects with project:write: got %d, want 201", rec.Code)
	}
	rec = do(t, h, "archive-project", tok, `{"name":"p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive-project with project:write: got %d, want 200", rec.Code)
	}
}

// A tampered/foreign token fails authn (401) before scope is even consulted.
func TestBadTokenIs401(t *testing.T) {
	fs := &fakeStore{}
	h, _ := newTestHandler(t, fs)
	rec := do(t, h, "create-agent", "not-a-jwt", agentBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d, want 401", rec.Code)
	}
}

// GET on a POST-only verb is 405 (after passing auth+scope).
func TestMethodNotAllowed(t *testing.T) {
	fs := &fakeStore{}
	h, m := newTestHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, "/create-agent", nil)
	req.Header.Set("Authorization", "Bearer "+mint(t, m, []string{ScopeOrgWrite}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET create-agent: got %d, want 405", rec.Code)
	}
}

// Store sentinel errors map to the documented status codes.
func TestStoreErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{ErrValidation, http.StatusUnprocessableEntity},
		{ErrConflict, http.StatusConflict},
		{ErrNamespaceUnresolved, http.StatusNotFound},
	}
	for _, c := range cases {
		fs := &fakeStore{err: c.err}
		h, m := newTestHandler(t, fs)
		rec := do(t, h, "create-agent", mint(t, m, []string{ScopeOrgWrite}), agentBody)
		if rec.Code != c.code {
			t.Fatalf("store err %v: got %d, want %d", c.err, rec.Code, c.code)
		}
	}
}

// archive-project with an empty name is a 400 before the store is consulted.
func TestArchiveRequiresName(t *testing.T) {
	fs := &fakeStore{}
	h, m := newTestHandler(t, fs)
	rec := do(t, h, "archive-project", mint(t, m, []string{ScopeProjectWrite}), `{"name":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("archive empty name: got %d, want 400", rec.Code)
	}
	if fs.reached != "" {
		t.Fatalf("store reached with empty name: %q", fs.reached)
	}
}

// assign (org:write) returns the A2A carrier; an empty toAgent is 400 before the store.
func TestAssignCarrier(t *testing.T) {
	fs := &fakeStore{}
	h, m := newTestHandler(t, fs)
	tok := mint(t, m, []string{ScopeOrgWrite})

	rec := do(t, h, "assign", tok, `{"toAgent":"coder","workItemId":"wi-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign with org:write: got %d, want 200", rec.Code)
	}
	var res AssignResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode assign result: %v", err)
	}
	if res.A2A.Verb != "SubmitTask" || res.A2A.TargetAgent != "coder" || res.CoordEnqueue == "" {
		t.Fatalf("assign carrier not populated: %+v", res)
	}

	fs2 := &fakeStore{}
	h2, m2 := newTestHandler(t, fs2)
	rec = do(t, h2, "assign", mint(t, m2, []string{ScopeOrgWrite}), `{"toAgent":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("assign empty toAgent: got %d, want 400", rec.Code)
	}
	if fs2.reached != "" {
		t.Fatalf("store reached with empty toAgent: %q", fs2.reached)
	}
}

// The canonical noun route and the verb alias hit the same handler/scope.
func TestNounRouteAndVerbAliasParity(t *testing.T) {
	for _, op := range []string{"agents", "create-agent"} {
		fs := &fakeStore{}
		h, m := newTestHandler(t, fs)
		rec := do(t, h, op, mint(t, m, []string{ScopeOrgWrite}), agentBody)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: got %d, want 201", op, rec.Code)
		}
	}
}

// The 403 body names the scope required (operational legibility, no secret leak).
func TestForbiddenBodyNamesScope(t *testing.T) {
	h, m := newTestHandler(t, &fakeStore{})
	rec := do(t, h, "projects", mint(t, m, []string{ScopeOrgWrite}), `{}`)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body["error"], ScopeProjectWrite) {
		t.Fatalf("403 body should name %q, got %q", ScopeProjectWrite, body["error"])
	}
}
