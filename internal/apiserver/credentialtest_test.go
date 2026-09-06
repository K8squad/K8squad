package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/credential"
	"github.com/K8squad/K8squad/pkg/credinject"
)

// ============================================================================
// E3-S2 test-connection (ISI-3680, AD-7, NFR-2) — the AC set:
//   - AC1: POST /api/credentials/{name}/test probes the STORED Secret
//     server-side (client never re-sends the value) and answers {ok, detail};
//   - AC2: the last result is cached as Team annotations (per-credential +
//     the per-agent flags the onboarding read model consumes); the probe
//     never returns the secret (NFR-2);
//   - AC3: BFF route (see console/app/api/credentials/[name]/test/route.ts).
// ============================================================================

// fakeProber answers canned statuses so tests never dial real providers. It
// records the last request so tests can pin the target dialect (URL, header
// name, Bearer shape) — never the material, which is asserted only by
// absence (NFR-2).
type fakeProber struct {
	status int
	err    error
	last   probeRequest
	calls  int
}

func (f *fakeProber) probe(_ context.Context, req probeRequest) (int, error) {
	f.calls++
	f.last = req
	return f.status, f.err
}

func newCredentialTester(t *testing.T, objs ...client.Object) (*CredentialTestService, client.Client, *fakeProber) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(secretWriteScheme(t)).WithObjects(objs...).Build()
	svc := NewCredentialTestService(c)
	// The BYO egress guard's DNS seam answers one fixed TEST-NET-3
	// address for every hostname (ISI-3891): tests never dial a real
	// resolver, and fixtures allow 203.0.113.0/24 when a BYO probe is
	// expected to be dialed.
	svc.probeEgress.Resolver = staticTestResolver{}
	fp := &fakeProber{status: http.StatusOK}
	svc.prober = fp
	svc.timeout = 0
	return svc, c, fp
}

// staticTestResolver maps every hostname to 203.0.113.10 (TEST-NET-3),
// except RFC-2606 .invalid names, which it refuses — the unresolvable-host
// fail-closed path needs a hostname that genuinely does not resolve.
type staticTestResolver struct{}

func (staticTestResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if strings.HasSuffix(host, ".invalid") {
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
}

// byoAllowPolicy is the fixture EgressPolicy that opens BYO probing in
// tests: TEST-NET-3, the range staticTestResolver answers with.
func byoAllowPolicy(ns string) *ksquadv1.EgressPolicy {
	p := &ksquadv1.EgressPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "byo-test-allow"}}
	p.Spec.Allow = []ksquadv1.EgressRule{{To: ksquadv1.EgressDestination{CIDR: "203.0.113.0/24"}}}
	return p
}

func testCredentialTestServer(t *testing.T, teamID uuid.UUID, tester *CredentialTestService) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator:    NewCookieAuthenticator(resolver),
		Discussion:       discussion.NewHandler(nil),
		CredentialTester: tester,
	})
	return srv.Handler()
}

func postCredentialTest(t *testing.T, h http.Handler, name, body string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/credentials/"+name+"/test", strings.NewReader(body))
	if withAuth {
		req = withSession(req, devToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// managedCredential builds a stored managed-credential Secret the probe reads.
func managedCredential(ns, name, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels: map[string]string{
				credential.LabelManagedCredential: credential.LabelManagedCredentialValue,
				credential.LabelCredentialClass:   "service-account",
			},
		},
		Data: map[string][]byte{key: []byte(value)},
	}
}

func agentWithCredential(ns, name, secretName string) *ksquadv1.Agent {
	return &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ksquadv1.AgentSpec{
			RuntimeRef:          ksquadv1.ObjectRef{Name: "claude-code"},
			CredentialSecretRef: ksquadv1.SecretRef{Name: secretName},
		},
	}
}

// TestCredentialTestHappyPathGreen — a stored service-account credential
// probed through the claude-code mapping answers {ok:true}, hits the
// Anthropic dialect (x-api-key, NOT Bearer), and caches passed flags on the
// Team CR for the credential AND every referencing agent. The material never
// appears in the response.
func TestCredentialTestHappyPathGreen(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-555555555555")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	cred := managedCredential("ksquad-team-alpha", "alice-anthropic", "apiKey", secretValueCanary)
	shared := agentWithCredential("ksquad-team-alpha", "boss", "alice-anthropic")
	solo := agentWithCredential("ksquad-team-alpha", "impl", "other-cred")
	svc, _, fp := newCredentialTester(t, tm, cred, shared, solo)
	h := testCredentialTestServer(t, teamID, svc)

	rec := postCredentialTest(t, h, "alice-anthropic", `{"runtime":"claude-code"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretValueCanary) {
		t.Fatalf("NFR-2 breach: response echoes the credential value: %s", rec.Body.String())
	}
	var out credentialTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || !strings.Contains(out.Detail, "Connected") {
		t.Fatalf("result: got %+v, want ok=true Connected", out)
	}

	// Dialect: Anthropic x-api-key (raw material, no Bearer prefix) + the
	// version header, against the models URL.
	if fp.calls != 1 {
		t.Fatalf("probe calls: %d", fp.calls)
	}
	if fp.last.target.url != anthropicModelsURL {
		t.Fatalf("probe url: %q", fp.last.target.url)
	}
	if fp.last.target.header != "x-api-key" {
		t.Fatalf("probe header: %q", fp.last.target.header)
	}
	if fp.last.target.bearer {
		t.Fatalf("anthropic dialect must not ride Bearer")
	}
	if fp.last.material != secretValueCanary {
		t.Fatalf("probe must carry the STORED material")
	}

	// AC2 cache: per-credential flag + per-agent mirror for referencing
	// agents only.
	var updated ksquadv1.Team
	if err := svc.client.Get(t.Context(), client.ObjectKey{Namespace: "teams", Name: "alpha"}, &updated); err != nil {
		t.Fatalf("team: %v", err)
	}
	if recorded, passed := CredentialTestFlag(&updated, "alice-anthropic"); !recorded || !passed {
		t.Fatalf("credential flag: recorded=%v passed=%v", recorded, passed)
	}
	if recorded, passed := TestConnectionFlag(&updated, "boss"); !recorded || !passed {
		t.Fatalf("agent mirror for referencing agent: recorded=%v passed=%v", recorded, passed)
	}
	if recorded, _ := TestConnectionFlag(&updated, "impl"); recorded {
		t.Fatalf("agent NOT referencing the credential must not be flagged")
	}
}

// TestCredentialTestRedAndUnreachable — a 401 is an honest red (failed
// cached on credential + agent); a transport failure is Unreachable, still
// red; 429 is honest "not confirmed".
func TestCredentialTestRedAndUnreachable(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-555555555556")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	cred := managedCredential("ksquad-team-alpha", "k", "apiKey", secretValueCanary)
	shared := agentWithCredential("ksquad-team-alpha", "boss", "k")

	for _, tc := range []struct {
		name   string
		status int
		err    error
		detail string
	}{
		{name: "rejected", status: http.StatusUnauthorized, detail: "Rejected"},
		{name: "forbidden", status: http.StatusForbidden, detail: "Rejected"},
		{name: "unreachable", status: 0, err: errors.New("dial tcp: no route"), detail: "Unreachable"},
		{name: "ratelimited", status: http.StatusTooManyRequests, detail: "Rate-limited"},
		{name: "server error", status: http.StatusInternalServerError, detail: "Endpoint error"},
	} {
		svc, _, _ := newCredentialTester(t, tm, cred, shared)
		svc.prober = &fakeProber{status: tc.status, err: tc.err}
		h := testCredentialTestServer(t, teamID, svc)

		rec := postCredentialTest(t, h, "k", `{"runtime":"openclaw"}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), secretValueCanary) {
			t.Fatalf("%s: NFR-2 breach: %s", tc.name, rec.Body.String())
		}
		var out credentialTestResult
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if out.OK {
			t.Fatalf("%s: must be a red", tc.name)
		}
		if !strings.Contains(out.Detail, tc.detail) {
			t.Fatalf("%s: detail %q must contain %q", tc.name, out.Detail, tc.detail)
		}

		var updated ksquadv1.Team
		if err := svc.client.Get(t.Context(), client.ObjectKey{Namespace: "teams", Name: "alpha"}, &updated); err != nil {
			t.Fatalf("%s: team: %v", tc.name, err)
		}
		if recorded, passed := CredentialTestFlag(&updated, "k"); !recorded || passed {
			t.Fatalf("%s: credential flag must be recorded failed: %v/%v", tc.name, recorded, passed)
		}
		if recorded, passed := TestConnectionFlag(&updated, "boss"); !recorded || passed {
			t.Fatalf("%s: agent mirror must be recorded failed: %v/%v", tc.name, recorded, passed)
		}
	}
}

// TestCredentialTestOpenAIFamily — the codex mapping probes the OpenAI
// dialect (Authorization: Bearer).
func TestCredentialTestOpenAIFamily(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-555555555557")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	cred := managedCredential("ksquad-team-alpha", "k", "apiKey", secretValueCanary)
	svc, _, fp := newCredentialTester(t, tm, cred)
	h := testCredentialTestServer(t, teamID, svc)

	rec := postCredentialTest(t, h, "k", `{"runtime":"codex"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: got %d (body %s)", rec.Code, rec.Body.String())
	}
	if fp.last.target.url != openAIModelsURL || !fp.last.target.bearer || fp.last.target.header != "Authorization" {
		t.Fatalf("openai dialect: %+v", fp.last.target)
	}
}

// TestCredentialTestBYOEndpoint — a modelEndpointRef names a 7.5 endpoint
// Secret; its base URL replaces the public provider and the credential
// rides Bearer — provided the squad's EgressPolicy declares the endpoint
// (ISI-3891). A ref without endpointURL is a 422 naming the field.
func TestCredentialTestBYOEndpoint(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-555555555558")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	cred := managedCredential("ksquad-team-alpha", "k", "apiKey", secretValueCanary)
	endpoint := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ksquad-team-alpha", Name: "glm-endpoint"},
		Data:       map[string][]byte{"endpointURL": []byte("https://open.bigmodel.cn/api/paas/v4/")},
	}
	svc, _, fp := newCredentialTester(t, tm, cred, endpoint, byoAllowPolicy("ksquad-team-alpha"))
	h := testCredentialTestServer(t, teamID, svc)

	rec := postCredentialTest(t, h, "k", `{"runtime":"opencode","modelEndpointRef":"glm-endpoint"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("byo test: got %d (body %s)", rec.Code, rec.Body.String())
	}
	if fp.last.target.url != "https://open.bigmodel.cn/api/paas/v4/models" {
		t.Fatalf("byo url: %q", fp.last.target.url)
	}
	if !fp.last.target.bearer || fp.last.target.header != "Authorization" {
		t.Fatalf("byo dialect: %+v", fp.last.target)
	}

	// Missing endpointURL: field-level 422, no probe dialed.
	bare := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ksquad-team-alpha", Name: "bare-endpoint"}}
	svc2, _, fp2 := newCredentialTester(t, tm, cred, bare)
	h2 := testCredentialTestServer(t, teamID, svc2)
	rec2 := postCredentialTest(t, h2, "k", `{"runtime":"opencode","modelEndpointRef":"bare-endpoint"}`, true)
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bare endpoint: got %d, want 422 (body %s)", rec2.Code, rec2.Body.String())
	}
	if fp2.calls != 0 {
		t.Fatalf("bare endpoint must not dial")
	}
}

// TestCredentialTestBYOEndpointEgressBlocked (ISI-3891) — the confused-deputy
// containment: a BYO endpoint outside the squad's declared egress allowlist
// is answered with the curated Blocked red and NO dial, including the
// control-plane internals the apiserver itself can reach (Postgres-style
// private ports, the link-local metadata service, loopback). A recorded red
// honestly un-completes the milestone, and the response never echoes the URL.
func TestCredentialTestBYOEndpointEgressBlocked(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-555555555560")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	cred := managedCredential("ksquad-team-alpha", "k", "apiKey", secretValueCanary)
	shared := agentWithCredential("ksquad-team-alpha", "boss", "k")

	cases := []struct {
		name     string
		endpoint string
		policy   *ksquadv1.EgressPolicy // nil = squad declares nothing
	}{
		{"undeclared squad, LAN target", "http://10.0.0.185:11434/v1", nil},
		{"outside the declared range", "http://192.168.1.1:8080/v1", byoAllowPolicy("ksquad-team-alpha")},
		{"metadata service despite allow-all", "http://169.254.169.254/latest/meta-data/", allowAllPolicy("ksquad-team-alpha")},
		{"loopback despite allow-all", "http://127.0.0.1:5432/", allowAllPolicy("ksquad-team-alpha")},
		{"unresolvable host", "http://no-such-host.invalid:11434/", byoAllowPolicy("ksquad-team-alpha")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ksquad-team-alpha", Name: "byo"},
				Data:       map[string][]byte{"endpointURL": []byte(tc.endpoint)},
			}
			objs := []client.Object{tm, cred, shared, endpoint}
			if tc.policy != nil {
				objs = append(objs, tc.policy)
			}
			svc, c, fp := newCredentialTester(t, objs...)
			h := testCredentialTestServer(t, teamID, svc)

			rec := postCredentialTest(t, h, "k", `{"runtime":"opencode","modelEndpointRef":"byo"}`, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("blocked byo: got %d (body %s)", rec.Code, rec.Body.String())
			}
			var result credentialTestResult
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if result.OK || result.Detail != credentialTestBlockedDetail {
				t.Fatalf("blocked byo detail: %+v", result)
			}
			if strings.Contains(rec.Body.String(), secretValueCanary) {
				t.Fatalf("NFR-2 breach: %s", rec.Body.String())
			}
			if fp.calls != 0 {
				t.Fatalf("blocked byo must not dial, dialed %d", fp.calls)
			}
			// The blocked red is CACHED like any red (AC2): the
			// per-credential and per-agent flags flip to failed.
			var team ksquadv1.Team
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: "teams", Name: "alpha"}, &team); err != nil {
				t.Fatalf("read team: %v", err)
			}
			if recorded, passed := CredentialTestFlag(&team, "k"); !recorded || passed {
				t.Fatalf("per-credential flag must record the red: recorded=%t passed=%t", recorded, passed)
			}
			if recorded, passed := TestConnectionFlag(&team, "boss"); !recorded || passed {
				t.Fatalf("per-agent mirror must record the red: recorded=%t passed=%t", recorded, passed)
			}
		})
	}
}

// allowAllPolicy declares 0.0.0.0/0 — the guard's hard-blocked classes
// (loopback, link-local) must still refuse to dial.
func allowAllPolicy(ns string) *ksquadv1.EgressPolicy {
	p := &ksquadv1.EgressPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "allow-all"}}
	p.Spec.Allow = []ksquadv1.EgressRule{{To: ksquadv1.EgressDestination{CIDR: "0.0.0.0/0"}}}
	return p
}

// TestCredentialTestMissingAndForeign — a missing name in the caller's
// namespace and a same-name Secret in ANOTHER team's namespace are both a
// plain 404 (existence-hiding): the probe only ever looks inside the
// caller's own namespace.
func TestCredentialTestMissingAndForeign(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-555555555559")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	foreign := managedCredential("ksquad-team-beta", "k", "apiKey", secretValueCanary)
	svc, _, fp := newCredentialTester(t, tm, foreign)
	h := testCredentialTestServer(t, teamID, svc)

	rec := postCredentialTest(t, h, "k", `{"runtime":"claude-code"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign/missing: got %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if fp.calls != 0 {
		t.Fatalf("404 path must not dial")
	}
}

// TestCredentialTestUnlabelledSecret404 — a Secret in the caller's own
// namespace WITHOUT the managed-credential label is not a probeable
// credential: 404, never a 403 that confirms it exists.
func TestCredentialTestUnlabelledSecret404(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-55555555555a")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	stranger := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ksquad-team-alpha",
			Name:      "kube-root-ca.crt",
		},
		Data: map[string][]byte{"ca.crt": []byte("not-a-credential")},
	}
	svc, _, fp := newCredentialTester(t, tm, stranger)
	h := testCredentialTestServer(t, teamID, svc)

	rec := postCredentialTest(t, h, "kube-root-ca.crt", `{"runtime":"claude-code"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unlabelled: got %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if fp.calls != 0 {
		t.Fatalf("unlabelled must not dial")
	}
}

// TestCredentialTestHumanSeat501 — the OAuth class is the 7.7 Connect-Claude
// lifecycle: documented 501 pointing at ISI-2899, no probe dialed.
func TestCredentialTestHumanSeat501(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-55555555555b")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	seat := managedCredential("ksquad-team-alpha", "seat", "token", secretValueCanary)
	seat.Labels[credential.LabelCredentialClass] = "human-seat"
	svc, _, fp := newCredentialTester(t, tm, seat)
	h := testCredentialTestServer(t, teamID, svc)

	rec := postCredentialTest(t, h, "seat", `{"runtime":"claude-code"}`, true)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("human-seat: got %d, want 501 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ISI-2899") {
		t.Fatalf("501 must carry the tracking pointer: %s", rec.Body.String())
	}
	if fp.calls != 0 {
		t.Fatalf("human-seat must not dial")
	}
}

// TestCredentialTestValidation — an unmapped runtime hint is a 422 naming
// the field; a Secret with no material under the credinject key is a 422
// shape mismatch; both without dialing.
func TestCredentialTestValidation(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-55555555555c")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	cred := managedCredential("ksquad-team-alpha", "k", "apiKey", secretValueCanary)
	empty := managedCredential("ksquad-team-alpha", "empty", "apiKey", "   ")
	svc, _, fp := newCredentialTester(t, tm, cred, empty)
	h := testCredentialTestServer(t, teamID, svc)

	rec := postCredentialTest(t, h, "k", `{"runtime":"triton-9000"}`, true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown runtime: got %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "runtime") {
		t.Fatalf("422 must name the runtime field: %s", rec.Body.String())
	}

	rec = postCredentialTest(t, h, "empty", `{"runtime":"claude-code"}`, true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank material: got %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	if fp.calls != 0 {
		t.Fatalf("validation branches must not dial")
	}
}

// TestCredentialTestTeamScopeAndAuth — unresolvable team scope is 404
// (existence-hiding); no session is 401 at the choke point.
func TestCredentialTestTeamScopeAndAuth(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-55555555555d")
	otherTeam := teamWithStatus("teams", "alpha", "22222222-2222-3333-4444-555555555555", "ksquad-team-alpha")
	cred := managedCredential("ksquad-team-alpha", "k", "apiKey", secretValueCanary)
	svc, _, _ := newCredentialTester(t, otherTeam, cred)
	h := testCredentialTestServer(t, teamID, svc)

	rec := postCredentialTest(t, h, "k", `{"runtime":"claude-code"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no team scope: got %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}

	rec = postCredentialTest(t, h, "k", `{"runtime":"claude-code"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}

// TestCredentialTestNilTester501 — a cluster-less dev run keeps the
// documented 501 (honest contract, not a 404).
func TestCredentialTestNilTester501(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-55555555555e")
	h := testCredentialTestServer(t, teamID, nil)

	rec := postCredentialTest(t, h, "k", `{"runtime":"claude-code"}`, true)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil tester: got %d, want 501 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ISI-3680") {
		t.Fatalf("501 must carry the tracking pointer: %s", rec.Body.String())
	}
}

// TestCredentialTestNoEchoSweep — NFR-2 across EVERY branch: no response
// body ever contains the stored material.
func TestCredentialTestNoEchoSweep(t *testing.T) {
	teamID := uuid.MustParse("21111111-2222-3333-4444-55555555555f")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	cred := managedCredential("ksquad-team-alpha", "k", "apiKey", secretValueCanary)
	shared := agentWithCredential("ksquad-team-alpha", "boss", "k")

	bodies := []string{
		`{"runtime":"claude-code"}`,
		`{"runtime":"triton-9000"}`,
		`{"runtime":"claude-code","modelEndpointRef":"ghost"}`,
		`{"runtime":"claude-code"`,
	}
	statuses := []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusNotFound}

	for i, body := range bodies {
		svc, _, _ := newCredentialTester(t, tm, cred, shared)
		svc.prober = &fakeProber{status: statuses[i%len(statuses)]}
		h := testCredentialTestServer(t, teamID, svc)
		for _, nm := range []string{"k", "missing", "kube-root-ca.crt"} {
			rec := postCredentialTest(t, h, nm, body, i != 3)
			if strings.Contains(rec.Body.String(), secretValueCanary) {
				t.Fatalf("body %q name %q (status %d): NFR-2 breach: %s", body, nm, rec.Code, rec.Body.String())
			}
		}
	}
	// Transport-error detail must not smuggle the material either.
	svc, _, _ := newCredentialTester(t, tm, cred, shared)
	svc.prober = &fakeProber{err: errors.New(secretValueCanary)}
	h := testCredentialTestServer(t, teamID, svc)
	rec := postCredentialTest(t, h, "k", `{"runtime":"claude-code"}`, true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), secretValueCanary) {
		t.Fatalf("transport detail must not leak the prober's error verbatim: %s", rec.Body.String())
	}
}

// TestProbeTargetsPinnedToInjectionTable — the probe dialect map must cover
// EVERY service-account env-var family the credinject table can yield for
// the known runtimes, so adding a runtime row cannot ship a probe that 502s
// (or worse, pings the wrong provider) — write-side drift guard, the E3-S2
// mirror of the S1 F1 pin. Human-seat rows are deliberately NOT pinned: the
// handler 501s the OAuth class (ISI-2899) before any probe dialect lookup.
func TestProbeTargetsPinnedToInjectionTable(t *testing.T) {
	pairs := []struct{ runtime string }{
		{ksquadv1.RuntimeTypeClaudeCode},
		{ksquadv1.RuntimeTypeOpenClaw},
		{ksquadv1.RuntimeTypeHermes},
		{ksquadv1.RuntimeTypeOpenCode},
		{ksquadv1.RuntimeTypeCodex},
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		injection, err := credinject.Inject(p.runtime, credinject.ClassServiceAccount, ksquadv1.SecretRef{Name: "pin"})
		if err != nil {
			t.Errorf("runtime %s: service-account injection failed: %v", p.runtime, err)
			continue
		}
		env := injection.Env[0].Name
		seen[env] = true
		if _, ok := probeTargets[env]; !ok {
			t.Errorf("injected env %q (runtime=%s) has no probe dialect", env, p.runtime)
		}
	}
	if !seen[envAnthropicAPIKey] || !seen[envOpenAIAPIKey] {
		t.Fatalf("pin degenerate: expected both provider families to be exercised, saw %v", seen)
	}
}

// TestCredentialTestFlagHelpers — the AD-2 annotation vocabulary round-trips
// and stays distinct from the per-agent prefix.
func TestCredentialTestFlagHelpers(t *testing.T) {
	tm := team("teams", "alpha", "u")
	if recorded, _ := CredentialTestFlag(tm, "k"); recorded {
		t.Fatalf("no flag recorded on a fresh Team")
	}
	SetCredentialTestFlag(tm, "k", true)
	SetTestConnectionFlag(tm, "k", false) // same NAME, agent-scoped: must not shadow
	if recorded, passed := CredentialTestFlag(tm, "k"); !recorded || !passed {
		t.Fatalf("credential flag: %v/%v", recorded, passed)
	}
	if recorded, passed := TestConnectionFlag(tm, "k"); !recorded || passed {
		t.Fatalf("agent flag: %v/%v", recorded, passed)
	}
	if v, ok := tm.Annotations[onboardingCredentialTestPrefix+"k"]; !ok || v != "passed" {
		t.Fatalf("annotation vocabulary: %v", tm.Annotations)
	}
}
