package apiserver

// Package-level docs live with each service; this file is the E3-S2
// test-connection surface (ISI-3680, AD-7): POST /api/credentials/{name}/test
// answers a REAL green/red on a stored credential.
//
// ============================================================================
// Test-connection for model credentials (E3-S2 / ISI-3680, AD-7, NFR-2) —
// POST /api/credentials/{name}/test performs a STATELESS server-side probe
// against the provider (or a BYO OpenAI-compatible endpoint) using the
// STORED Secret, then answers {ok, detail}.
// ============================================================================
//
// The shape of the decision (AD-7):
//
//   - The client NEVER re-sends the credential value. The path names the
//     credential; the body carries only routing hints ({runtime}, optional
//     modelEndpointRef for BYO endpoints) — nothing sensitive.
//   - The probe is a cheap authenticated ping (GET /v1/models on the
//     provider's public API, or GET {base}/models on a BYO endpoint), run
//     with a short timeout. No job, no queue, no state beyond the cached
//     last result.
//   - The result is ADVISORY and cached as Team annotations (AD-2): one
//     per-credential flag the Credentials/Launchpad surfaces can read on
//     resume, mirrored onto the per-AGENT flags the onboarding read model
//     (modelsMilestoneComplete) already consumes — so a recorded failure
//     honestly un-completes milestone ③ with zero reader changes.
//
// NFR-2 discipline, structural here as in secretwrite.go:
//
//   - The material leaves the Secret exactly once, inside the probe's
//     Authorization/x-api-key header, and is never rendered into a response
//     body, an error message, a log line, or a URL. `detail` strings are a
//     CURATED vocabulary plus an HTTP status code — provider response
//     bodies are never forwarded (some providers echo key fragments in
//     error payloads; we do not gamble).
//   - Only Secrets labelled ksquad.io/managed-credential=true in the
//     caller's OWN team namespace are ever fetched — anything else is a 404
//     (existence-hiding), never a 403 that confirms the object exists.
//   - The human-seat (OAuth) class answers the same documented 501 as the
//     create path (ISI-2899): an interactive OAuth token is not probeable
//     by paste-key semantics.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gorilla/mux"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/credential"
	"github.com/K8squad/K8squad/pkg/credinject"
)

// credentialProbeTimeout bounds one provider ping. A test-connection is a
// click-path action: a provider that cannot answer a models list inside a
// few seconds is a red, not a spinner.
const credentialProbeTimeout = 5 * time.Second

// credentialTestMaxBodyBytes bounds the request body. It carries routing
// hints only ({runtime}, optional modelEndpointRef}) — never a value — so a
// small ceiling is generous.
const credentialTestMaxBodyBytes = 1 << 10

// Provider ping endpoints for the credential families the injection table
// maps. Keyed by the ENV VAR NAME credinject.Inject yields, so the probe
// never maintains a parallel (runtime, class) → provider table: the runtime
// hint resolves through the same reviewed injection contract the kubelet
// side uses, and this map only knows how to say "authenticated list models"
// in each provider's dialect.
const (
	envAnthropicAPIKey = "ANTHROPIC_API_KEY"
	envOpenAIAPIKey    = "OPENAI_API_KEY"

	anthropicModelsURL = "https://api.anthropic.com/v1/models"
	anthropicVersion   = "2023-06-01"
	openAIModelsURL    = "https://api.openai.com/v1/models"
)

// probeTarget is one provider ping dialect: the models-list URL, the header
// that carries the credential, whether it rides as a Bearer token, and any
// extra headers the provider requires.
type probeTarget struct {
	url       string
	header    string
	bearer    bool
	extraName string
	extraVal  string
}

// probeTargets maps the injected env-var family to its ping dialect. Both
// rows answer a cheap GET that REQUIRES valid credentials, so a 200 is
// honest green and a 401/403 is honest red.
var probeTargets = map[string]probeTarget{
	envAnthropicAPIKey: {
		url:       anthropicModelsURL,
		header:    "x-api-key",
		extraName: "anthropic-version",
		extraVal:  anthropicVersion,
	},
	envOpenAIAPIKey: {
		url:    openAIModelsURL,
		header: "Authorization",
		bearer: true,
	},
}

// CredentialTestClient is the cluster seam the probe path needs: Secret Get
// (the stored material + the optional BYO endpoint Secret), Team List/Get
// (UID → Team resolution, annotation cache) + Team Update (the cache write)
// and Agent List (mirroring the result onto referencing agents). Production
// wires a direct client (annotation writes must not race a stale cache);
// tests wire a fake.
type CredentialTestClient interface {
	client.Reader
	Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error
}

// CredentialTestService is the E3-S2 test-connection model: one stateless
// probe per request, value never echoed, last result cached on the Team CR.
type CredentialTestService struct {
	client  CredentialTestClient
	prober  credentialProber
	timeout time.Duration
}

// NewCredentialTestService builds the test-connection model. The client's
// scheme MUST have corev1 and ksquadv1 registered (NewCredentialTester
// guarantees both).
func NewCredentialTestService(c CredentialTestClient) *CredentialTestService {
	return &CredentialTestService{
		client:  c,
		prober:  &httpCredentialProber{client: &http.Client{Timeout: credentialProbeTimeout}},
		timeout: credentialProbeTimeout,
	}
}

// credentialTestRequest is the POST /api/credentials/{name}/test body. It
// carries ROUTING HINTS only — the credential value is never re-sent (the
// whole point of the endpoint). Runtime selects the injection mapping (and
// thereby the provider family); modelEndpointRef optionally names a BYO
// endpoint Secret (the 7.5 shape) whose base URL is probed instead of the
// public provider.
type credentialTestRequest struct {
	Runtime          string `json:"runtime"`
	ModelEndpointRef string `json:"modelEndpointRef"`
}

// credentialTestResult is the response body (AC1): a boolean and a curated,
// human-readable detail. No secret material, ever (NFR-2).
type credentialTestResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// handleCredentialTest is the handler behind POST /api/credentials/{name}/test.
func (s *CredentialTestService) handleCredentialTest(w http.ResponseWriter, r *http.Request) {
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	var req credentialTestRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	name := muxVarsName(r, "name")

	team, err := s.resolveTeam(r.Context(), author.TeamID.String())
	if errors.Is(err, ErrTeamNamespaceUnresolved) {
		writeJSONError(w, http.StatusNotFound, "no team namespace for this caller")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "team scope resolution unavailable")
		return
	}
	ns := team.Status.Namespace

	// Read the STORED Secret (AC1: the client never re-sends the value).
	var secret corev1.Secret
	if err := s.client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &secret); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			// Existence-hiding: a foreign, mistyped or non-credential
			// Secret is indistinguishable from "no such credential".
			writeJSONError(w, http.StatusNotFound, "no such credential")
		case apierrors.IsForbidden(err):
			log.Printf("apiserver: credential test GET FORBIDDEN for %s/%s principal=%s: check team-namespace credential-tester Role (E3-S2)", ns, name, author.Principal)
			writeJSONError(w, http.StatusBadGateway, "credential store rejected the read")
		default:
			log.Printf("apiserver: credential test read failed for %s/%s: %v", ns, name, err)
			writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		}
		return
	}

	// Label-scoped containment (AD-6 mirror): only managed credentials are
	// probeable, and only in the caller's own namespace (enforced by the
	// object key above). Anything else is a 404, never a 403.
	if secret.Labels[credential.LabelManagedCredential] != credential.LabelManagedCredentialValue {
		writeJSONError(w, http.StatusNotFound, "no such credential")
		return
	}

	class := credinject.Resolve(credinject.CredentialClass(secret.Labels[credential.LabelCredentialClass]))

	// Honest degrade (the S1/AC4 posture): a human-seat credential is an
	// interactive OAuth lifecycle, not a probeable paste-key. Same
	// documented 501 as the create path.
	if class == credinject.ClassHumanSeat {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":    "not implemented",
			"detail":   "human-seat credentials follow the Connect Claude OAuth lifecycle and cannot be probe-tested",
			"tracking": "ISI-2899: credential controller + Connect Claude OAuth flow (POST /api/credentials/connect)",
		})
		return
	}

	// The runtime hint resolves through the SAME injection contract the
	// write path and the kubelet side use — write key and probe dialect
	// cannot drift from what a Run would actually inject.
	key, mapped := credinject.DefaultSecretKey(req.Runtime, class)
	if !mapped {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": []fieldError{{Field: "runtime", Message: "no credential mapping for this runtime and class"}},
		})
		return
	}
	injection, err := credinject.Inject(req.Runtime, class, ksquadv1.SecretRef{Name: name})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "credential mapping unavailable")
		return
	}
	target, known := probeTargets[injection.Env[0].Name]
	if !known {
		log.Printf("apiserver: no probe mapping for injected env %q (runtime=%s class=%s)", injection.Env[0].Name, req.Runtime, class)
		writeJSONError(w, http.StatusBadGateway, "no probe mapping for this credential family")
		return
	}

	material := strings.TrimSpace(string(secret.Data[key]))
	if material == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "credential shape mismatch",
			"fields": []fieldError{{Field: "runtime", Message: "stored credential has no material under the key this runtime reads"}},
		})
		return
	}

	// Optional BYO endpoint: the ref names a 7.5 endpoint Secret whose base
	// URL replaces the public provider. The credential under test is still
	// the named managed credential — the endpoint Secret contributes the
	// URL only, never another token.
	if req.ModelEndpointRef != "" {
		endpointURL, err := s.byoEndpointURL(r.Context(), ns, req.ModelEndpointRef)
		if errors.Is(err, errNoEndpointURL) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  "validation failed",
				"fields": []fieldError{{Field: "modelEndpointRef", Message: "endpoint Secret has no endpointURL (arch §11 / story 7.5 shape)"}},
			})
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "endpoint read unavailable")
			return
		}
		target = probeTarget{url: endpointURL, header: "Authorization", bearer: true}
	}

	probe := probeRequest{target: target, material: material}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	status, perr := s.prober.probe(ctx, probe)
	if perr != nil {
		// Server-side only: net errors carry hostnames, never headers —
		// the response detail stays curated (foldProbeResult, NFR-2).
		log.Printf("apiserver: credential test probe transport failure ns=%s name=%s url=%s: %v", ns, name, target.url, perr)
	}
	result := foldProbeResult(target, status, perr)

	// Cache the last result as Team annotations (AC2 / AD-2): the
	// per-credential flag plus the per-AGENT flags the onboarding read
	// model already reads. Advisory: a cache failure logs and the probe
	// result is still answered honestly.
	referencing := s.agentsReferencing(r.Context(), ns, name)
	s.recordResult(r.Context(), team, name, referencing, result.OK)

	log.Printf("apiserver: credential test ns=%s name=%s runtime=%s class=%s ok=%t by=%s", ns, name, req.Runtime, class, result.OK, author.Principal)
	writeJSON(w, http.StatusOK, result)
}

// byoEndpointURL reads a 7.5 BYO-endpoint Secret and returns its validated
// models-list URL (base + "/models"). Only the URL is used; an apiToken in
// the endpoint Secret is deliberately ignored — the test exercises the
// NAMED managed credential.
func (s *CredentialTestService) byoEndpointURL(ctx context.Context, ns, ref string) (string, error) {
	var secret corev1.Secret
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref}, &secret); err != nil {
		return "", err
	}
	raw := strings.TrimSpace(string(secret.Data["endpointURL"]))
	if raw == "" {
		raw = strings.TrimSpace(string(secret.Data["url"]))
	}
	if raw == "" {
		return "", errNoEndpointURL
	}
	raw = strings.TrimRight(raw, "/")
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", errNoEndpointURL
	}
	return raw + "/models", nil
}

// errNoEndpointURL names the 7.5 shape failure: an endpoint Secret without
// a usable endpointURL is a 422 against the caller, not a 502 against the
// cluster.
var errNoEndpointURL = errors.New("credentialtest: endpoint Secret has no endpointURL")

// foldProbeResult maps the probe's HTTP outcome onto the curated {ok,
// detail} vocabulary (NFR-2: detail strings are fixed text plus a status
// code — never a provider body, never a raw transport error, both of which
// can carry material fragments; the transport error is logged server-side
// instead, where net errors carry only hostnames).
func foldProbeResult(target probeTarget, status int, err error) credentialTestResult {
	switch {
	case err != nil:
		return credentialTestResult{OK: false, Detail: "Unreachable — the endpoint could not be reached (network error)"}
	case status == http.StatusOK:
		return credentialTestResult{OK: true, Detail: "Connected — the endpoint accepted the credential"}
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return credentialTestResult{OK: false, Detail: fmt.Sprintf("Rejected — the endpoint declined the credential (HTTP %d)", status)}
	case status == http.StatusTooManyRequests:
		return credentialTestResult{OK: false, Detail: "Rate-limited — the endpoint is reachable but throttled (HTTP 429); credential not confirmed"}
	default:
		return credentialTestResult{OK: false, Detail: fmt.Sprintf("Endpoint error (HTTP %d)", status)}
	}
}

// recordResult persists the last result as Team annotations (AC2): the
// per-credential flag (CredentialTestFlag vocabulary) plus, for every Agent
// in the namespace whose spec.credentialSecretRef names this credential,
// the per-agent flag the onboarding read model's modelsMilestoneComplete
// already consumes — a recorded red honestly un-completes milestone ③.
// One Team Update carries both mirrors; a failure logs loudly and does not
// mask the probe result (the cache is advisory, AD-7).
func (s *CredentialTestService) recordResult(ctx context.Context, team *ksquadv1.Team, credName string, referencing []string, passed bool) {
	SetCredentialTestFlag(team, credName, passed)
	for _, agent := range referencing {
		SetTestConnectionFlag(team, agent, passed)
	}
	if err := s.client.Update(ctx, team); err != nil {
		log.Printf("apiserver: credential test annotation write FAILED for team %s credential %s: %v", team.Name, credName, err)
	}
}

// agentsReferencing lists the names of Agents in ns whose
// spec.credentialSecretRef resolves to credName, sorted — the mirror set
// for recordResult.
func (s *CredentialTestService) agentsReferencing(ctx context.Context, ns, credName string) []string {
	var agents ksquadv1.AgentList
	if err := s.client.List(ctx, &agents, client.InNamespace(ns)); err != nil {
		log.Printf("apiserver: credential test agent mirror list failed for ns %s: %v", ns, err)
		return nil
	}
	var out []string
	for i := range agents.Items {
		if agents.Items[i].Spec.CredentialSecretRef.Name == credName {
			out = append(out, agents.Items[i].Name)
		}
	}
	sort.Strings(out)
	return out
}

// resolveTeam resolves the caller's Team UID to the Team object itself (the
// probe needs both the reconciled namespace and the CR to annotate). Same
// discipline as secretwrite.teamNamespace: unknown UID or un-reconciled
// namespace is ErrTeamNamespaceUnresolved (404), never a shared fallback.
func (s *CredentialTestService) resolveTeam(ctx context.Context, teamUID string) (*ksquadv1.Team, error) {
	if teamUID == "" {
		return nil, ErrTeamNamespaceUnresolved
	}
	var teams ksquadv1.TeamList
	if err := s.client.List(ctx, &teams); err != nil {
		return nil, err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID && teams.Items[i].Status.Namespace != "" {
			return &teams.Items[i], nil
		}
	}
	return nil, ErrTeamNamespaceUnresolved
}

// ── probe seam ─────────────────────────────────────────────────────────────

// probeRequest is one resolved, authenticated ping. The material lives here
// and ONLY here: it crosses to the HTTP client inside the auth header and
// is never rendered into logs, errors, or responses.
type probeRequest struct {
	target   probeTarget
	material string
}

// credentialProber performs one ping and reports the HTTP status (err is a
// transport failure — the status is meaningless then). The seam exists so
// tests never dial the real providers.
type credentialProber interface {
	probe(ctx context.Context, req probeRequest) (int, error)
}

// httpCredentialProber is the production prober.
type httpCredentialProber struct {
	client *http.Client
}

func (p *httpCredentialProber) probe(ctx context.Context, req probeRequest) (int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.target.url, nil)
	if err != nil {
		return 0, err
	}
	value := req.material
	if req.target.bearer {
		value = "Bearer " + value
	}
	httpReq.Header.Set(req.target.header, value)
	if req.target.extraName != "" {
		httpReq.Header.Set(req.target.extraName, req.target.extraVal)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// muxVarsName reads a gorilla/mux path variable, matching the house pattern
// (artifacts.go / dashboard.go).
func muxVarsName(r *http.Request, key string) string {
	return mux.Vars(r)[key]
}
