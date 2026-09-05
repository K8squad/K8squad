package apiserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/credential"
)

// ============================================================================
// E3-S1 managed-credential write (ISI-3679, AD-6, NFR-2) — the merge-gate
// rubric from the ISI-3672 security pre-review:
//   - create stores ONE label-scoped Secret in the caller's team namespace
//     under the credinject-derived key (write/read agreement, F1);
//   - the value is NEVER echoed — not on create, not on any error branch;
//   - team scope is existence-hiding (unresolvable Team ⇒ 404);
//   - the human-seat (OAuth) class degrades to the documented 501 (ISI-2899);
//   - name is DNS-1123, class is ValidateClass'd, unknown (runtime, class)
//     pairs fail closed;
//   - a name collision is 409 (the create-only grant can never overwrite).
// ============================================================================

const secretValueCanary = "sk-ant-CANARY-value-that-must-never-leak"

func secretWriteScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	if err := ksquadv1.AddToScheme(s); err != nil {
		t.Fatalf("ksquadv1 scheme: %v", err)
	}
	return s
}

// teamWithStatus is a Team whose reconciler has stamped its squad namespace —
// the shape the write path resolves against.
func teamWithStatus(ns, name, uid, squadNS string) *ksquadv1.Team {
	tm := team(ns, name, uid)
	tm.Status.Namespace = squadNS
	return tm
}

func newSecretWriter(t *testing.T, objs ...client.Object) (*SecretWriteService, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(secretWriteScheme(t)).WithObjects(objs...).Build()
	return NewSecretWriteService(c), c
}

func testSecretWriteServer(t *testing.T, teamID uuid.UUID, writer *SecretWriteService) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		SecretWriter:  writer,
	})
	return srv.Handler()
}

func postCredential(t *testing.T, h http.Handler, body string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/credentials", strings.NewReader(body))
	if withAuth {
		req = withSession(req, devToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCredentialCreateHappyPath — a paste-key lands as ONE opaque Secret in the
// team namespace: managed-credential + credential-class labels stamped from the
// consts, material under the credinject-derived key (apiKey for a
// service-account), response a secret:// reference that does NOT contain the
// value.
func TestCredentialCreateHappyPath(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	svc, c := newSecretWriter(t, teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha"))
	h := testSecretWriteServer(t, teamID, svc)

	rec := postCredential(t, h, `{"name":"alice-anthropic","runtime":"claude-code","class":"service-account","value":"`+secretValueCanary+`"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretValueCanary) {
		t.Fatalf("NFR-2 breach: response echoes the credential value: %s", rec.Body.String())
	}
	var out credentialCreateResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SecretRef != "secret://ksquad-team-alpha/alice-anthropic" {
		t.Fatalf("secretRef: got %q", out.SecretRef)
	}

	var got corev1.Secret
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "ksquad-team-alpha", Name: "alice-anthropic"}, &got); err != nil {
		t.Fatalf("secret not stored: %v", err)
	}
	if got.Labels[credential.LabelManagedCredential] != credential.LabelManagedCredentialValue {
		t.Fatalf("managed label missing/wrong: %v", got.Labels)
	}
	if got.Labels[credential.LabelCredentialClass] != "service-account" {
		t.Fatalf("class label: %v", got.Labels)
	}
	if string(got.Data["apiKey"]) != secretValueCanary {
		t.Fatalf("material stored under wrong key; data keys: %v", keysOf(got.Data))
	}
	if _, ok := got.Data["token"]; ok {
		t.Fatalf("F1 regression: service-account material must NOT be stored under the human-seat key %q", "token")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCredentialCreateDefaultClass — an omitted class resolves to the safe
// default (service-account) and lands under the same apiKey key.
func TestCredentialCreateDefaultClass(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-555555555556")
	svc, c := newSecretWriter(t, teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha"))
	h := testSecretWriteServer(t, teamID, svc)

	rec := postCredential(t, h, `{"name":"openai-key","runtime":"codex","value":"`+secretValueCanary+`"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var got corev1.Secret
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "ksquad-team-alpha", Name: "openai-key"}, &got); err != nil {
		t.Fatalf("secret not stored: %v", err)
	}
	if got.Labels[credential.LabelCredentialClass] != "service-account" {
		t.Fatalf("class label should carry the RESOLVED default: %v", got.Labels)
	}
	if string(got.Data["apiKey"]) != secretValueCanary {
		t.Fatalf("material stored under wrong key; data keys: %v", keysOf(got.Data))
	}
}

// TestCredentialCreateHumanSeat501 — the OAuth/GLM-OAuth variant is the 7.7
// Connect-Claude lifecycle, never a paste-key: documented 501 pointing at
// ISI-2899, and NO Secret is written.
func TestCredentialCreateHumanSeat501(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-555555555557")
	svc, c := newSecretWriter(t, teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha"))
	h := testSecretWriteServer(t, teamID, svc)

	rec := postCredential(t, h, `{"name":"alice-oauth","runtime":"claude-code","class":"human-seat","value":"`+secretValueCanary+`"}`, true)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("human-seat: got %d, want 501 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ISI-2899") {
		t.Fatalf("501 must carry the tracking pointer: %s", rec.Body.String())
	}
	var secrets corev1.SecretList
	if err := c.List(t.Context(), &secrets); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("human-seat 501 must not write a Secret; found %d", len(secrets.Items))
	}
}

// TestCredentialCreateValidation — every reject branch is a 422 whose body
// names the field, never the value.
func TestCredentialCreateValidation(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-555555555558")
	svc, _ := newSecretWriter(t, teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha"))
	h := testSecretWriteServer(t, teamID, svc)

	cases := map[string]string{
		"bad name":        `{"name":"Not_A_Valid_Name!","runtime":"claude-code","class":"service-account","value":"x"}`,
		"unknown class":   `{"name":"ok-name","runtime":"claude-code","class":"super-user","value":"x"}`,
		"unknown runtime": `{"name":"ok-name","runtime":"triton-9000","class":"service-account","value":"x"}`,
		"empty value":     `{"name":"ok-name","runtime":"claude-code","class":"service-account","value":""}`,
	}
	for name, body := range cases {
		rec := postCredential(t, h, body, true)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: got %d, want 422 (body %s)", name, rec.Code, rec.Body.String())
		}
	}

	// Oversized value: over the credential ceiling, still under the route's
	// body bound.
	big := `{"name":"ok-name","runtime":"claude-code","class":"service-account","value":"` + strings.Repeat("A", credentialMaxValueBytes+1) + `"}`
	rec := postCredential(t, h, big, true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized value: got %d, want 422", rec.Code)
	}
}

// TestCredentialCreateBodyBound — a body beyond the route's maxBytesBody
// ceiling is rejected before decode.
func TestCredentialCreateBodyBound(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-555555555559")
	svc, _ := newSecretWriter(t, teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha"))
	h := testSecretWriteServer(t, teamID, svc)

	big := `{"name":"ok-name","runtime":"claude-code","class":"service-account","value":"` + strings.Repeat("A", credentialMaxValueBytes+4<<10) + `"}`
	rec := postCredential(t, h, big, true)
	if rec.Code == http.StatusCreated {
		t.Fatalf("oversized body must not create a credential")
	}
}

// TestCredentialCreateTeamScope — an unresolvable Team scope is a 404 and
// writes nothing (existence-hiding, §12.1).
func TestCredentialCreateTeamScope(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-55555555555a")
	svc, c := newSecretWriter(t, teamWithStatus("teams", "alpha", "22222222-2222-3333-4444-555555555555", "ksquad-team-alpha"))
	h := testSecretWriteServer(t, teamID, svc)

	rec := postCredential(t, h, `{"name":"alice-anthropic","runtime":"claude-code","class":"service-account","value":"`+secretValueCanary+`"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign/no team: got %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	var secrets corev1.SecretList
	if err := c.List(t.Context(), &secrets); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("404 path must not write; found %d Secrets", len(secrets.Items))
	}
}

// TestCredentialCreateConflict — a name collision is 409: the create-only
// grant can never overwrite an existing Secret, and the error echoes no value.
func TestCredentialCreateConflict(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-55555555555b")
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ksquad-team-alpha",
			Name:      "alice-anthropic",
			Labels:    map[string]string{credential.LabelManagedCredential: credential.LabelManagedCredentialValue},
		},
		Data: map[string][]byte{"apiKey": []byte("pre-existing")},
	}
	svc, _ := newSecretWriter(t, teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha"), existing)
	h := testSecretWriteServer(t, teamID, svc)

	rec := postCredential(t, h, `{"name":"alice-anthropic","runtime":"claude-code","class":"service-account","value":"`+secretValueCanary+`"}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict: got %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretValueCanary) {
		t.Fatalf("NFR-2 breach: conflict response echoes the value")
	}
}

// TestCredentialCreateUnauthenticated — no session ⇒ 401 at the choke point.
func TestCredentialCreateUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-55555555555c")
	svc, _ := newSecretWriter(t, teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha"))
	h := testSecretWriteServer(t, teamID, svc)

	rec := postCredential(t, h, `{"name":"x","runtime":"claude-code","value":"`+secretValueCanary+`"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}

// TestCredentialCreateNilWriter501 — a cluster-less dev run keeps the
// documented 501 (honest contract, not a 404).
func TestCredentialCreateNilWriter501(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-55555555555d")
	h := testSecretWriteServer(t, teamID, nil)

	rec := postCredential(t, h, `{"name":"x","runtime":"claude-code","value":"`+secretValueCanary+`"}`, true)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil writer: got %d, want 501 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ISI-3679") {
		t.Fatalf("501 must carry the tracking pointer: %s", rec.Body.String())
	}
}

// TestCredentialCreateNoEchoSweep — the NFR-2 invariant across EVERY error
// branch: no response body ever contains the submitted value.
func TestCredentialCreateNoEchoSweep(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-55555555555e")
	svc, _ := newSecretWriter(t, teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha"))
	h := testSecretWriteServer(t, teamID, svc)

	bodies := []string{
		`{"name":"bad name!","runtime":"claude-code","value":"` + secretValueCanary + `"}`,
		`{"name":"ok","runtime":"nope","value":"` + secretValueCanary + `"}`,
		`{"name":"ok","runtime":"claude-code","class":"human-seat","value":"` + secretValueCanary + `"}`,
		`{"name":"ok","runtime":"claude-code","class":"bogus","value":"` + secretValueCanary + `"}`,
		`{"value":"` + secretValueCanary + `"`,
		`not json ` + secretValueCanary,
	}
	for i, body := range bodies {
		rec := postCredential(t, h, body, true)
		if strings.Contains(rec.Body.String(), secretValueCanary) {
			t.Fatalf("case %d: status %d echoes the value: %s", i, rec.Code, rec.Body.String())
		}
	}
}
