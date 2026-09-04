package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// 8.6 credential/auth-state read model (ISI-2902) — ACs from story 8.6:
//   - per-agent credential surface (BYO Secret ref, runtime, model) Team-scoped;
//   - a Run Paused on an expired token surfaces a CLEAR paused-on-expiry signal
//     (row health expired + PausedRuns carrying the hold), supporting S10 / 7.4;
//   - unknown horizons stay unknown (never a fabricated expiry — FR-I3 discipline);
//   - routes ride the §13 choke point (401 unauthenticated / 404 no-team / 501 nil reader);
//   - POST /api/credentials/connect answers its documented 501 until ISI-2899 (7.7).
// ============================================================================

func agent(ns, name, runtime, credSecret string) *ksquadv1.Agent {
	return &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ksquadv1.AgentSpec{
			RuntimeRef:          ksquadv1.ObjectRef{Name: runtime},
			CredentialSecretRef: ksquadv1.SecretRef{Name: credSecret},
			Model:               "claude-sonnet-4-5",
		},
	}
}

func pausedRun(ns, name, agentName, reason string, since time.Time) *ksquadv1.Run {
	return &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       ksquadv1.RunSpec{Agents: []ksquadv1.ObjectRef{{Name: agentName}}},
		Status: ksquadv1.RunStatus{
			Phase: ksquadv1.RunPhasePaused,
			Conditions: []metav1.Condition{{
				Type:               "Paused",
				Status:             metav1.ConditionTrue,
				Reason:             reason,
				LastTransitionTime: metav1.NewTime(since),
			}},
		},
	}
}

func newCredReader(t *testing.T, objs ...client.Object) *ClientCredentialReader {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
	return NewClientCredentialReader(c)
}

// TestCredentialsProjection — the happy path: agents project to rows with per-user Secret refs,
// a Paused(credential) Run marks that agent expired·paused, other pause reasons do not, and
// everything the cache cannot know (expiry horizon) stays honestly unknown.
func TestCredentialsProjection(t *testing.T) {
	const teamUID = "aaaaaaaa-1111-1111-1111-111111111111"
	since := time.Date(2026, 8, 20, 12, 0, 41, 0, time.UTC)
	r := newCredReader(t,
		team("squad-a", "alpha", teamUID),
		agent("squad-a", "fixer-hermes", "hermes", "sam-hermes-oauth"),
		agent("squad-a", "reviewer-openclaw", "openclaw", "sam-openclaw-key"),
		pausedRun("squad-a", "run-139", "fixer-hermes", "credential_expired", since),
		pausedRun("squad-a", "run-140", "reviewer-openclaw", "rate_limited", since),
	)

	ov, err := r.Credentials(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if ov.Team != "squad-a" {
		t.Fatalf("team ns: %q", ov.Team)
	}
	if len(ov.Agents) != 2 {
		t.Fatalf("agents: got %d, want 2", len(ov.Agents))
	}
	// Sorted by name: fixer-hermes before reviewer-openclaw.
	fixer, reviewer := ov.Agents[0], ov.Agents[1]
	if fixer.Agent != "fixer-hermes" || reviewer.Agent != "reviewer-openclaw" {
		t.Fatalf("order: %s, %s", fixer.Agent, reviewer.Agent)
	}

	// The paused-on-expiry agent: health expired, the hold carried with provenance.
	if fixer.Health != CredHealthExpired {
		t.Fatalf("fixer health: %q, want expired", fixer.Health)
	}
	if len(fixer.PausedRuns) != 1 || fixer.PausedRuns[0].Name != "run-139" {
		t.Fatalf("fixer pausedRuns: %+v", fixer.PausedRuns)
	}
	if fixer.PausedRuns[0].Reason != "credential_expired" {
		t.Fatalf("hold reason: %q", fixer.PausedRuns[0].Reason)
	}
	if fixer.PausedRuns[0].Since == nil || !fixer.PausedRuns[0].Since.Equal(since) {
		t.Fatalf("hold since: %+v", fixer.PausedRuns[0].Since)
	}

	// rate_limited is NOT a credential hold (7.6: distinct reason family) and zero credential
	// knowledge is NOT evidence of health: the row stays unknown, never a fabricated green
	// badge (PR #87 review — the honesty rule the file header states).
	if reviewer.Health != CredHealthUnknown {
		t.Fatalf("reviewer health: %q, want unknown (no hold ≠ connected)", reviewer.Health)
	}
	if len(reviewer.PausedRuns) != 0 {
		t.Fatalf("reviewer pausedRuns: %+v", reviewer.PausedRuns)
	}

	for _, row := range ov.Agents {
		if row.ExpiresKnown || row.ExpiresAt != nil {
			t.Fatalf("row %s: expiry must stay unknown (no controller data yet): %+v", row.Agent, row.ExpiresAt)
		}
		if row.CredentialRef == "" {
			t.Fatalf("row %s: credential ref is the point (FR-G1)", row.Agent)
		}
	}
	if fixer.CredentialRef != "squad-a/sam-hermes-oauth" {
		t.Fatalf("fixer credentialRef: %q", fixer.CredentialRef)
	}
}

// TestCredentialsCredentialClassDualRead — the Credentials read-model prefers the authoritative
// spec.credentialClass (ISI-3681 E3-S3 AC6 / R-CR1 C2) and falls back to the legacy
// ksquad.io/credential-class annotation only when the spec field is empty. A spec value wins even
// when a stale annotation disagrees, so the screen reflects what the injector/webhook actually read.
func TestCredentialsCredentialClassDualRead(t *testing.T) {
	const teamUID = "aaaaaaaa-7777-7777-7777-777777777777"

	// spec-set: authoritative field present → used verbatim.
	specAgent := agent("squad-a", "a-spec", "hermes", "sec-1")
	specAgent.Spec.CredentialClass = "human-seat"

	// legacy-annotation-only: no spec field → annotation fallback keeps the row honest.
	legacyAgent := agent("squad-a", "b-legacy", "hermes", "sec-2")
	legacyAgent.Annotations = map[string]string{"ksquad.io/credential-class": "service-account"}

	// spec wins over a stale annotation that disagrees.
	bothAgent := agent("squad-a", "c-both", "hermes", "sec-3")
	bothAgent.Spec.CredentialClass = "service-account"
	bothAgent.Annotations = map[string]string{"ksquad.io/credential-class": "human-seat"}

	r := newCredReader(t, team("squad-a", "alpha", teamUID), specAgent, legacyAgent, bothAgent)
	ov, err := r.Credentials(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	byName := map[string]string{}
	for _, row := range ov.Agents {
		byName[row.Agent] = row.CredentialClass
	}
	if byName["a-spec"] != "human-seat" {
		t.Fatalf("spec.credentialClass must be read: got %q", byName["a-spec"])
	}
	if byName["b-legacy"] != "service-account" {
		t.Fatalf("legacy annotation must be the fallback: got %q", byName["b-legacy"])
	}
	if byName["c-both"] != "service-account" {
		t.Fatalf("spec must win over a stale annotation: got %q", byName["c-both"])
	}
}

// TestCredentialsZeroKnowledgeIsUnknown — an Agent with no Runs, no annotations, and no
// controller data must render health unknown, NOT a fabricated connected badge: absence of a
// paused Run is not evidence the credential works (PR #87 review).
func TestCredentialsZeroKnowledgeIsUnknown(t *testing.T) {
	const teamUID = "aaaaaaaa-4444-4444-4444-444444444444"
	r := newCredReader(t,
		team("squad-a", "alpha", teamUID),
		agent("squad-a", "idle-agent", "hermes", "sam-idle-oauth"),
	)
	ov, err := r.Credentials(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if len(ov.Agents) != 1 || ov.Agents[0].Health != CredHealthUnknown {
		t.Fatalf("zero-knowledge health: %+v — want unknown", ov.Agents)
	}
}

// TestCredentialsCrossNamespaceRefSkipped — a Run referencing an Agent by name with an explicit
// FOREIGN namespace must not attribute its hold to a same-named Agent in this namespace (wrong
// row, wrong banner). Empty-namespace refs (the Run's own namespace) still join.
func TestCredentialsCrossNamespaceRefSkipped(t *testing.T) {
	const teamUID = "aaaaaaaa-5555-5555-5555-555555555555"
	since := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	r := newCredReader(t,
		team("squad-a", "alpha", teamUID),
		agent("squad-a", "fixer-hermes", "hermes", "sam-hermes-oauth"),
		&ksquadv1.Run{
			ObjectMeta: metav1.ObjectMeta{Namespace: "squad-a", Name: "run-foreign"},
			Spec: ksquadv1.RunSpec{Agents: []ksquadv1.ObjectRef{
				{Name: "fixer-hermes", Namespace: "squad-b"}, // foreign — skip
			}},
			Status: ksquadv1.RunStatus{
				Phase: ksquadv1.RunPhasePaused,
				Conditions: []metav1.Condition{{
					Type: "Paused", Status: metav1.ConditionTrue,
					Reason: "credential_expired", LastTransitionTime: metav1.NewTime(since),
				}},
			},
		},
		pausedRun("squad-a", "run-local", "fixer-hermes", "credential_expired", since), // own ns — join
	)
	ov, err := r.Credentials(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	row := ov.Agents[0]
	if len(row.PausedRuns) != 1 || row.PausedRuns[0].Name != "run-local" {
		t.Fatalf("foreign-ns ref must be skipped, own-ns kept: %+v", row.PausedRuns)
	}
}

// TestCredentialsTeamNamespaceMemoized — the UID→namespace list runs once per distinct UID:
// repeat reads never re-scan every Team (PR #87 review perf note).
func TestCredentialsTeamNamespaceMemoized(t *testing.T) {
	const teamUID = "aaaaaaaa-6666-6666-6666-666666666666"
	lists := 0
	base := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(
		team("squad-a", "alpha", teamUID),
		agent("squad-a", "fixer-hermes", "hermes", "sam-hermes-oauth"),
	).Build()
	counting := &listCounter{Client: base, lists: &lists}

	reader := NewClientCredentialReader(counting)
	for i := 0; i < 3; i++ {
		if _, err := reader.Credentials(context.Background(), teamUID); err != nil {
			t.Fatalf("Credentials #%d: %v", i, err)
		}
	}
	if lists != 1 {
		t.Fatalf("team list ran %d times for one UID — memoization must collapse it to 1", lists)
	}
}

// listCounter counts TeamList calls flowing through an inner client (memoization assertion —
// Agent/Run lists are per-request by design, only the UID scan must collapse).
type listCounter struct {
	client.Client
	lists *int
}

func (c *listCounter) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, isTeam := list.(*ksquadv1.TeamList); isTeam {
		*c.lists++
	}
	return c.Client.List(ctx, list, opts...)
}

// TestCredentialsHandlerUnavailable — a reader error that is NOT ErrTeamNotFound answers 502
// (read model unavailable), the branch a downed backing produces.
func TestCredentialsHandlerUnavailable(t *testing.T) {
	teamID := uuid.MustParse("bbbbbbbb-7777-7777-7777-777777777777")
	h := testCredServer(t, teamID, errReader{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/credentials", nil), devToken))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("credentials(reader error): got %d, want 502", rec.Code)
	}
}

// errReader always fails with a generic error (backing down).
type errReader struct{}

func (errReader) Credentials(context.Context, string) (CredentialsOverview, error) {
	return CredentialsOverview{}, errors.New("backing store down")
}

// TestCredentialsTeamScopeIsolation — Team B's agents never leak into Team A's credential surface.
func TestCredentialsTeamScopeIsolation(t *testing.T) {
	const uidA = "aaaaaaaa-2222-2222-2222-222222222222"
	const uidB = "bbbbbbbb-2222-2222-2222-222222222222"
	r := newCredReader(t,
		team("squad-a", "alpha", uidA),
		team("squad-b", "beta", uidB),
		agent("squad-a", "a-agent", "hermes", "sam-a-oauth"),
		agent("squad-b", "b-agent", "openclaw", "eve-b-oauth"),
	)
	ov, err := r.Credentials(context.Background(), uidA)
	if err != nil {
		t.Fatalf("Credentials A: %v", err)
	}
	if len(ov.Agents) != 1 || ov.Agents[0].Agent != "a-agent" {
		t.Fatalf("team A leaked cross-tenant credential rows: %+v", ov.Agents)
	}
}

// TestCredentialsTeamNotFound — unknown/empty scope ⇒ ErrTeamNotFound (→ 404), never a blank row set.
func TestCredentialsTeamNotFound(t *testing.T) {
	r := newCredReader(t, team("squad-a", "alpha", "cccccccc-3333-3333-3333-333333333333"))
	for _, uid := range []string{"", "99999999-9999-9999-9999-999999999999"} {
		if _, err := r.Credentials(context.Background(), uid); !errors.Is(err, ErrTeamNotFound) {
			t.Fatalf("uid %q: got %v, want ErrTeamNotFound", uid, err)
		}
	}
}

// TestCredentialHoldReasonFamily — the closed reason vocabulary the projection treats as a
// credential hold (case/whitespace tolerant), and the negatives that must NOT hold.
// The "…™" case pins the G115-safe normalization: non-ASCII runes are dropped, never
// narrowed rune→byte, so noise around the vocabulary still matches the vocabulary.
func TestCredentialHoldReasonFamily(t *testing.T) {
	for _, reason := range []string{"auth_failure", "CredentialExpired", " cred_expired ", "credential_rotated", "cred_expired™"} {
		if !isCredentialHold(reason) {
			t.Fatalf("reason %q must classify as a credential hold", reason)
		}
	}
	for _, reason := range []string{"rate_limited", "sandbox_missing", "", "pending", "Инvalid"} {
		if isCredentialHold(reason) {
			t.Fatalf("reason %q must NOT classify as a credential hold", reason)
		}
	}
}

// --- handler / server wiring ---------------------------------------------------------------------

func testCredServer(t *testing.T, teamID uuid.UUID, reader CredentialOverviewReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Credentials:   reader,
	})
	return srv.Handler()
}

// TestCredentialsHandlerOK — a session whose Team scope resolves serves 200 + the projection.
func TestCredentialsHandlerOK(t *testing.T) {
	teamID := uuid.MustParse("dddddddd-4444-4444-4444-444444444444")
	reader := newCredReader(t,
		team("squad-a", "alpha", teamID.String()),
		agent("squad-a", "fixer-hermes", "hermes", "sam-hermes-oauth"),
	)
	h := testCredServer(t, teamID, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/credentials", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("credentials: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var ov CredentialsOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ov.Team != "squad-a" || len(ov.Agents) != 1 {
		t.Fatalf("body: %+v", ov)
	}
}

// TestCredentialsHandlerUnauthenticated — no session ⇒ 401 at the choke point.
func TestCredentialsHandlerUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("eeeeeeee-5555-5555-5555-555555555555")
	h := testCredServer(t, teamID, newCredReader(t, team("squad-a", "alpha", teamID.String())))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/credentials", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("credentials(no session): got %d, want 401", rec.Code)
	}
}

// TestCredentialsHandlerTeamNotFound — authenticated caller, unresolvable Team scope ⇒ 404.
func TestCredentialsHandlerTeamNotFound(t *testing.T) {
	teamID := uuid.MustParse("ffffffff-6666-6666-6666-666666666666")
	h := testCredServer(t, teamID, newCredReader(t, team("squad-a", "alpha", "12345678-6666-6666-6666-666666666666")))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/credentials", nil), devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("credentials(no team): got %d, want 404", rec.Code)
	}
}

// TestCredentialsNilReaderStill501 — no read model wired ⇒ documented 501 (cluster-less dev run).
func TestCredentialsNilReaderStill501(t *testing.T) {
	teamID := uuid.MustParse("1a2b3c4d-7777-7777-7777-777777777777")
	h := testCredServer(t, teamID, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/credentials", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("credentials(nil reader): got %d, want 501", rec.Code)
	}
}

// TestConnectClaudeDocumented501 — POST /api/credentials/connect answers the documented 501 with
// its tracking issue until the 7.7 OAuth flow (ISI-2899) lands; never a fabricated login.
func TestConnectClaudeDocumented501(t *testing.T) {
	teamID := uuid.MustParse("2b3c4d5e-8888-8888-8888-888888888888")
	h := testCredServer(t, teamID, newCredReader(t, team("squad-a", "alpha", teamID.String())))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodPost, "/api/credentials/connect", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("connect: got %d, want 501", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["tracking"] != "ISI-2899: credential controller + Connect Claude OAuth flow" {
		t.Fatalf("connect tracking: %q", body["tracking"])
	}
}

// TestCredentialsReadModelIsReadOnly — the reader performs no writes against the cluster: the
// projection is a pure read over the cache (defence-in-depth check via a recording interceptor).
func TestCredentialsReadModelIsReadOnly(t *testing.T) {
	const teamUID = "3c4d5e6f-9999-9999-9999-999999999999"
	writes := 0
	base := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(
		team("squad-a", "alpha", teamUID),
		agent("squad-a", "fixer-hermes", "hermes", "sam-hermes-oauth"),
	).Build()
	recording := &countingWriter{Client: base, writes: &writes}

	if _, err := NewClientCredentialReader(recording).Credentials(context.Background(), teamUID); err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if writes != 0 {
		t.Fatalf("read model wrote %d objects — the credential read model must be read-only", writes)
	}
}

// countingWriter counts mutating calls flowing through an inner client (read-only assertion).
type countingWriter struct {
	client.Client
	writes *int
}

func (c *countingWriter) Create(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
	*c.writes++
	return nil
}

func (c *countingWriter) Update(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
	*c.writes++
	return nil
}

func (c *countingWriter) Delete(_ context.Context, _ client.Object, _ ...client.DeleteOption) error {
	*c.writes++
	return nil
}

func (c *countingWriter) DeleteAllOf(_ context.Context, _ client.Object, _ ...client.DeleteAllOfOption) error {
	*c.writes++
	return nil
}

// Regression (PR #87 re-review): cmd/apiserver/main.go shares ONE ClientCredentialReader
// across HTTP request goroutines, so the teamNS memoization must be race-safe — an
// unsynchronized map crashes the process on a concurrent cold-start burst
// (fatal error: concurrent map writes). Goes green under -race only when guarded.
func TestCredentialsConcurrentTeamNSMemoization(t *testing.T) {
	const teamUID = "aaaaaaaa-1111-1111-1111-111111111111"
	r := newCredReader(t,
		team("squad-a", "alpha", teamUID),
		agent("squad-a", "fixer-hermes", "hermes", "sam-hermes-oauth"),
		pausedRun("squad-a", "run-139", "fixer-hermes", "credential_expired", time.Now().UTC()),
	)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Credentials(context.Background(), teamUID); err != nil {
				t.Errorf("Credentials: %v", err)
			}
		}()
	}
	wg.Wait()
}
