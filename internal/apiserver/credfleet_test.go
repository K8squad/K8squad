package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/credential"
)

// ============================================================================
// Fleet-wide-admin credential surface (ISI-3937) — the write/test/list half of
// the ISI-3932 fleet-wide-admin pattern. A global admin has no home tenancy (its
// team_id backs no Team CR, ISI-3921), so its credential ops target an explicit
// team from the fleet, defaulting to the single team, resolved through the same
// Team-CR-uid + status.namespace discipline (no shared-namespace fallback).
// ============================================================================

// danglingAdmin is a fleet-wide admin AuthorContext whose Team UID resolves to no
// Team CR — the bootstrap admin's shape (ISI-3921).
func danglingAdmin() discussion.AuthorContext {
	return discussion.AuthorContext{Principal: "user:admin", TeamID: uuid.New(), IsAdmin: true}
}

// authReq stamps an AuthorContext onto a request context directly (the §13 BFF's
// job in production), so a handler-level test can exercise the admin branch the
// StaticSessionResolver harness cannot express.
func authReq(method, target, body string, auth discussion.AuthorContext) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	return req.WithContext(discussion.WithAuth(req.Context(), auth))
}

// withMuxName populates the {name} path var the credential-test handler reads
// (mux does this in production; a directly-invoked handler needs it set).
func withMuxName(req *http.Request, name string) *http.Request {
	return mux.SetURLVars(req, map[string]string{"name": name})
}

// ── fleetAdminTeam unit table ───────────────────────────────────────────────

func TestFleetAdminTeam(t *testing.T) {
	uidA := "aaaaaaaa-1111-1111-1111-111111111111"
	uidB := "bbbbbbbb-2222-2222-2222-222222222222"
	reconciledA := *teamWithStatus("isitobservable", "alpha", uidA, "isitobservable")
	reconciledB := *teamWithStatus("betasquad", "beta", uidB, "betasquad")
	// A Team the reconciler has NOT stamped a namespace on is not targetable.
	pending := *team("ksquad-system", "pending", "cccccccc-3333-3333-3333-333333333333")

	t.Run("single team defaults with no teamId", func(t *testing.T) {
		got, err := fleetAdminTeam([]ksquadv1.Team{reconciledA}, "")
		if err != nil {
			t.Fatalf("want the single team, got err %v", err)
		}
		if string(got.UID) != uidA {
			t.Fatalf("got team %q, want %q", got.UID, uidA)
		}
	})

	t.Run("explicit teamId selects that team", func(t *testing.T) {
		got, err := fleetAdminTeam([]ksquadv1.Team{reconciledA, reconciledB}, uidB)
		if err != nil {
			t.Fatalf("want team B, got err %v", err)
		}
		if string(got.UID) != uidB {
			t.Fatalf("got team %q, want %q", got.UID, uidB)
		}
	})

	t.Run("multi team no teamId is ErrSelectTeam", func(t *testing.T) {
		_, err := fleetAdminTeam([]ksquadv1.Team{reconciledA, reconciledB}, "")
		if !errors.Is(err, ErrSelectTeam) {
			t.Fatalf("got %v, want ErrSelectTeam", err)
		}
	})

	t.Run("unknown teamId is existence-hiding unresolved", func(t *testing.T) {
		_, err := fleetAdminTeam([]ksquadv1.Team{reconciledA, reconciledB}, "no-such-uid")
		if !errors.Is(err, ErrTeamNamespaceUnresolved) {
			t.Fatalf("got %v, want ErrTeamNamespaceUnresolved", err)
		}
	})

	t.Run("zero reconciled teams is unresolved not select", func(t *testing.T) {
		_, err := fleetAdminTeam([]ksquadv1.Team{pending}, "")
		if !errors.Is(err, ErrTeamNamespaceUnresolved) {
			t.Fatalf("got %v, want ErrTeamNamespaceUnresolved", err)
		}
	})

	t.Run("un-reconciled team is not targetable even by explicit teamId", func(t *testing.T) {
		_, err := fleetAdminTeam([]ksquadv1.Team{pending}, string(pending.UID))
		if !errors.Is(err, ErrTeamNamespaceUnresolved) {
			t.Fatalf("got %v, want ErrTeamNamespaceUnresolved", err)
		}
	})

	t.Run("a lone reconciled team wins past un-reconciled noise", func(t *testing.T) {
		got, err := fleetAdminTeam([]ksquadv1.Team{pending, reconciledA}, "")
		if err != nil {
			t.Fatalf("want the single reconciled team, got err %v", err)
		}
		if string(got.UID) != uidA {
			t.Fatalf("got team %q, want %q", got.UID, uidA)
		}
	})
}

// ── POST /api/credentials (write) ───────────────────────────────────────────

const fleetKeyBody = `{"name":"admin-anthropic","runtime":"claude-code","class":"service-account","value":"sk-ant-FLEET-canary"}`

func TestCredentialCreateFleetAdminSingleTeam(t *testing.T) {
	squad := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	svc, c := newSecretWriter(t, squad)

	rec := httptest.NewRecorder()
	svc.handleCredentialCreate(rec, authReq(http.MethodPost, "/api/credentials", fleetKeyBody, danglingAdmin()))

	if rec.Code != http.StatusCreated {
		t.Fatalf("fleet admin single-team create: got %d, want 201 — body %s", rec.Code, rec.Body.String())
	}
	// The Secret must land in the targeted team's reconciled squad namespace.
	var got corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "isitobservable", Name: "admin-anthropic"}, &got); err != nil {
		t.Fatalf("secret not in the single team's namespace: %v", err)
	}
	if got.Labels[credential.LabelManagedCredential] != credential.LabelManagedCredentialValue {
		t.Fatalf("managed-credential label not stamped: %v", got.Labels)
	}
	if strings.Contains(rec.Body.String(), "FLEET-canary") {
		t.Fatalf("value leaked into the response: %s", rec.Body.String())
	}
}

func TestCredentialCreateFleetAdminMultiTeamNeedsSelection(t *testing.T) {
	a := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	b := teamWithStatus("betasquad", "beta", "bbbbbbbb-2222-2222-2222-222222222222", "betasquad")
	svc, _ := newSecretWriter(t, a, b)

	rec := httptest.NewRecorder()
	svc.handleCredentialCreate(rec, authReq(http.MethodPost, "/api/credentials", fleetKeyBody, danglingAdmin()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("multi-team admin with no teamId: got %d, want 400 — body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "select a team") {
		t.Fatalf("400 must prompt team selection, got %s", rec.Body.String())
	}
}

func TestCredentialCreateFleetAdminExplicitTeam(t *testing.T) {
	a := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	b := teamWithStatus("betasquad", "beta", "bbbbbbbb-2222-2222-2222-222222222222", "betasquad")
	svc, c := newSecretWriter(t, a, b)

	body := `{"name":"admin-anthropic","runtime":"claude-code","class":"service-account","value":"sk-ant-x","teamId":"bbbbbbbb-2222-2222-2222-222222222222"}`
	rec := httptest.NewRecorder()
	svc.handleCredentialCreate(rec, authReq(http.MethodPost, "/api/credentials", body, danglingAdmin()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("explicit teamId create: got %d, want 201 — body %s", rec.Code, rec.Body.String())
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "betasquad", Name: "admin-anthropic"}, &corev1.Secret{}); err != nil {
		t.Fatalf("secret must land in the explicitly targeted team's namespace: %v", err)
	}
}

// TestCredentialCreateBoundNonAdminCannotTargetForeignTeam is the tenancy-hijack
// guardrail: a bound non-admin whose teamId names a foreign team writes to its
// OWN namespace, never the foreign one.
func TestCredentialCreateBoundNonAdminCannotTargetForeignTeam(t *testing.T) {
	ownUID := uuid.New()
	own := teamWithStatus("own-squad", "own", ownUID.String(), "own-squad")
	foreign := teamWithStatus("foreign-squad", "foreign", "ffffffff-9999-9999-9999-999999999999", "foreign-squad")
	svc, c := newSecretWriter(t, own, foreign)

	body := `{"name":"sneaky","runtime":"claude-code","class":"service-account","value":"sk-x","teamId":"ffffffff-9999-9999-9999-999999999999"}`
	bound := discussion.AuthorContext{Principal: "user:mallory", TeamID: ownUID, IsAdmin: false}
	rec := httptest.NewRecorder()
	svc.handleCredentialCreate(rec, authReq(http.MethodPost, "/api/credentials", body, bound))
	if rec.Code != http.StatusCreated {
		t.Fatalf("bound caller create: got %d, want 201 — body %s", rec.Code, rec.Body.String())
	}
	// Written to its OWN namespace...
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "own-squad", Name: "sneaky"}, &corev1.Secret{}); err != nil {
		t.Fatalf("bound caller must write to its own namespace: %v", err)
	}
	// ...and NOT to the foreign team the body tried to name.
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "foreign-squad", Name: "sneaky"}, &corev1.Secret{}); err == nil {
		t.Fatal("tenancy hijack: bound caller wrote into a foreign team namespace via teamId")
	}
}

// TestCredentialCreateNonAdminDanglingStays404 — a non-admin whose own team is
// unresolvable gets 404 even with a teamId (only an admin may target the fleet).
func TestCredentialCreateNonAdminDanglingStays404(t *testing.T) {
	squad := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	svc, _ := newSecretWriter(t, squad)

	body := `{"name":"x","runtime":"claude-code","class":"service-account","value":"sk-x","teamId":"aaaaaaaa-1111-1111-1111-111111111111"}`
	nonAdmin := discussion.AuthorContext{Principal: "user:bob", TeamID: uuid.New(), IsAdmin: false}
	rec := httptest.NewRecorder()
	svc.handleCredentialCreate(rec, authReq(http.MethodPost, "/api/credentials", body, nonAdmin))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin dangling caller: got %d, want 404 — body %s", rec.Code, rec.Body.String())
	}
}

// ── POST /api/credentials/{name}/test (probe) ───────────────────────────────

func TestCredentialTestFleetAdminSingleTeam(t *testing.T) {
	squad := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	// A stored, managed service-account credential in the single team's namespace.
	stored := &corev1.Secret{}
	stored.Namespace = "isitobservable"
	stored.Name = "admin-anthropic"
	stored.Labels = map[string]string{
		credential.LabelManagedCredential: credential.LabelManagedCredentialValue,
		credential.LabelCredentialClass:   "service-account",
	}
	stored.Data = map[string][]byte{"apiKey": []byte("sk-ant-stored")}
	svc, _, fp := newCredentialTester(t, squad, stored)
	fp.status = http.StatusOK

	req := authReq(http.MethodPost, "/api/credentials/admin-anthropic/test", `{"runtime":"claude-code"}`, danglingAdmin())
	req = withMuxName(req, "admin-anthropic")
	rec := httptest.NewRecorder()
	svc.handleCredentialTest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("fleet admin single-team test: got %d, want 200 — body %s", rec.Code, rec.Body.String())
	}
	var out credentialTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !out.OK {
		t.Fatalf("probe should be green, got %+v", out)
	}
}

func TestCredentialTestFleetAdminMultiTeamNeedsSelection(t *testing.T) {
	a := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	b := teamWithStatus("betasquad", "beta", "bbbbbbbb-2222-2222-2222-222222222222", "betasquad")
	svc, _, _ := newCredentialTester(t, a, b)

	req := authReq(http.MethodPost, "/api/credentials/some-cred/test", `{"runtime":"claude-code"}`, danglingAdmin())
	req = withMuxName(req, "some-cred")
	rec := httptest.NewRecorder()
	svc.handleCredentialTest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("multi-team admin test with no teamId: got %d, want 400 — body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "select a team") {
		t.Fatalf("400 must prompt team selection, got %s", rec.Body.String())
	}
}

func TestCredentialTestNonAdminDanglingStays404(t *testing.T) {
	squad := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	svc, _, _ := newCredentialTester(t, squad)

	body := `{"runtime":"claude-code","teamId":"aaaaaaaa-1111-1111-1111-111111111111"}`
	nonAdmin := discussion.AuthorContext{Principal: "user:bob", TeamID: uuid.New(), IsAdmin: false}
	req := authReq(http.MethodPost, "/api/credentials/x/test", body, nonAdmin)
	req = withMuxName(req, "x")
	rec := httptest.NewRecorder()
	svc.handleCredentialTest(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin dangling test caller: got %d, want 404 — body %s", rec.Code, rec.Body.String())
	}
}

// ── GET /api/credentials (list) ─────────────────────────────────────────────

func TestCredentialsListFleetAdminSingleTeam(t *testing.T) {
	squad := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	ag := agent("isitobservable", "hermes", "claude-code", "hermes-cred")
	r := newCredReader(t, squad, ag)

	ov, err := r.Credentials(context.Background(), uuid.New().String(), true, "")
	if err != nil {
		t.Fatalf("fleet admin single-team list: unexpected err %v", err)
	}
	if len(ov.Agents) != 1 || ov.Agents[0].Agent != "hermes" {
		t.Fatalf("list must project the single team's agents, got %+v", ov.Agents)
	}
}

func TestCredentialsListFleetAdminMultiTeamNeedsSelection(t *testing.T) {
	a := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	b := teamWithStatus("betasquad", "beta", "bbbbbbbb-2222-2222-2222-222222222222", "betasquad")
	r := newCredReader(t, a, b)

	if _, err := r.Credentials(context.Background(), uuid.New().String(), true, ""); !errors.Is(err, ErrSelectTeam) {
		t.Fatalf("multi-team admin list with no teamId: got %v, want ErrSelectTeam", err)
	}
}

func TestCredentialsListFleetAdminExplicitTeam(t *testing.T) {
	a := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	b := teamWithStatus("betasquad", "beta", "bbbbbbbb-2222-2222-2222-222222222222", "betasquad")
	bAgent := agent("betasquad", "athena", "claude-code", "athena-cred")
	r := newCredReader(t, a, b, bAgent)

	ov, err := r.Credentials(context.Background(), uuid.New().String(), true, "bbbbbbbb-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("explicit teamId list: unexpected err %v", err)
	}
	if len(ov.Agents) != 1 || ov.Agents[0].Agent != "athena" {
		t.Fatalf("list must project the targeted team's agents, got %+v", ov.Agents)
	}
}

func TestCredentialsListNonAdminDanglingStaysNotFound(t *testing.T) {
	squad := teamWithStatus("isitobservable", "alpha", "aaaaaaaa-1111-1111-1111-111111111111", "isitobservable")
	r := newCredReader(t, squad)

	if _, err := r.Credentials(context.Background(), uuid.New().String(), false, "aaaaaaaa-1111-1111-1111-111111111111"); !errors.Is(err, ErrTeamNotFound) {
		t.Fatalf("non-admin dangling list: got %v, want ErrTeamNotFound", err)
	}
}
