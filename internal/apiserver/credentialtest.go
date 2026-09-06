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
//   - BYO targets are egress-contained (ISI-3891, the ISI-3887 N1
//     advisory): the apiserver dials a caller-chosen URL only when the
//     destination sits inside the squad's declared EgressPolicy allowlist
//     (pkg/probeegress) — the probe can never reach anything a Run
//     could not, so it cannot serve as a confused deputy against
//     control-plane internals, and a green always means a Run could
//     actually use the endpoint.
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
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gorilla/mux"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/credential"
	"github.com/K8squad/K8squad/pkg/credinject"
	"github.com/K8squad/K8squad/pkg/probeegress"
)

// credentialProbeTimeout bounds one provider ping. A test-connection is a
// click-path action: a provider that cannot answer a models list inside a
// few seconds is a red, not a spinner.
const credentialProbeTimeout = 5 * time.Second

// credentialTestMaxBodyBytes bounds the request body. It carries routing
// hints only ({runtime}, optional modelEndpointRef}) — never a value — so a
// small ceiling is generous.
const credentialTestMaxBodyBytes = 1 << 10

// credentialTestBlockedDetail is the ONE curated string for every BYO egress
// guard denial (ISI-3891). Same vocabulary discipline as foldProbeResult:
// the reason the guard refused (no policy, not covered, unresolvable,
// proxy-only, hard-blocked address class) is server-side log material — the
// caller gets the fix, not an oracle on the apiserver's network vantage.
const credentialTestBlockedDetail = "Blocked — this endpoint is not in your squad's egress policy, so a Run could not reach it either; declare the endpoint in an EgressPolicy for your squad and test again"

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
// extra headers the provider requires. For BYO targets, pinned carries the
// guard-validated address set (ISI-3899 F-A): the dial connects ONLY to
// these addresses, never to a fresh resolution of the URL's host.
type probeTarget struct {
	url       string
	header    string
	bearer    bool
	extraName string
	extraVal  string
	// pinned is non-empty exactly when the egress guard validated this
	// target (BYO path). nil/empty = public provider path, dialed by the
	// default client (the target is not caller-chosen).
	pinned []netip.Addr
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
	// probeEgress constrains BYO probe targets to the squad's declared
	// egress allowlist (ISI-3891). Nil disables the guard — only tests
	// may do that, and only to pin the unguarded legacy shape.
	probeEgress *probeegress.Guard
}

// NewCredentialTestService builds the test-connection model. The client's
// scheme MUST have corev1 and ksquadv1 registered (NewCredentialTester
// guarantees both). The BYO egress guard rides the same client (it reads
// EgressPolicy/Project CRs from the caller's namespace).
func NewCredentialTestService(c CredentialTestClient) *CredentialTestService {
	return &CredentialTestService{
		client:      c,
		prober:      &httpCredentialProber{client: &http.Client{Timeout: credentialProbeTimeout}},
		timeout:     credentialProbeTimeout,
		probeEgress: &probeegress.Guard{Reader: c},
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
	// Funnel span + metric (ISI-3669/ISI-3680, obs plan §3/§4): the M3
	// credential test-connection step. team.id + the bounded (class, runtime)
	// pair ride the span; the credential VALUE stays telemetry-dark (NFR-2,
	// §5.3 — TestNFR2CredentialTestSecretNeverInTelemetry sweeps span AND log).
	ctx, span := funnelSpan(r.Context(), "ksquad.credential.test")
	defer span.End()
	fn := funnelInst()

	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		funnelOutcome(ctx, span, fn.credentialTest, outcomeUnauthenticated)
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	funnelAttrs(span, attribute.String("team.id", author.TeamID.String()))

	var req credentialTestRequest
	if err := decodeJSON(w, r, &req); err != nil {
		funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestInvalid)
		return
	}

	name := muxVarsName(r, "name")

	team, err := s.resolveTeam(r.Context(), author.TeamID.String())
	if errors.Is(err, ErrTeamNamespaceUnresolved) {
		funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestNoNamespace)
		writeJSONError(w, http.StatusNotFound, "no team namespace for this caller")
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "credential test: team scope resolution unavailable", "error", err)
		funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestError)
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
			funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestNotFound)
			writeJSONError(w, http.StatusNotFound, "no such credential")
		case apierrors.IsForbidden(err):
			slog.ErrorContext(ctx, "credential test GET FORBIDDEN — check team-namespace credential-tester Role (E3-S2)",
				"namespace", ns, "name", name, "principal", author.Principal)
			funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestError)
			writeJSONError(w, http.StatusBadGateway, "credential store rejected the read")
		default:
			slog.ErrorContext(ctx, "credential test read failed", "namespace", ns, "name", name, "error", err)
			funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestError)
			writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		}
		return
	}

	// Label-scoped containment (AD-6 mirror): only managed credentials are
	// probeable, and only in the caller's own namespace (enforced by the
	// object key above). Anything else is a 404, never a 403.
	if secret.Labels[credential.LabelManagedCredential] != credential.LabelManagedCredentialValue {
		funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestNotFound)
		writeJSONError(w, http.StatusNotFound, "no such credential")
		return
	}

	class := credinject.Resolve(credinject.CredentialClass(secret.Labels[credential.LabelCredentialClass]))
	funnelAttrs(span, attribute.String("credential.class", string(class)))

	// Honest degrade (the S1/AC4 posture): a human-seat credential is an
	// interactive OAuth lifecycle, not a probeable paste-key. Same
	// documented 501 as the create path.
	if class == credinject.ClassHumanSeat {
		funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestUnsupported)
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
		funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": []fieldError{{Field: "runtime", Message: "no credential mapping for this runtime and class"}},
		})
		return
	}
	// (runtime, class) is now a validated injection-table pair — both bounded
	// enums, safe as a span attribute (mirrors the create path).
	funnelAttrs(span, attribute.String("credential.runtime", req.Runtime))
	injection, err := credinject.Inject(req.Runtime, class, ksquadv1.SecretRef{Name: name})
	if err != nil {
		funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestError)
		writeJSONError(w, http.StatusBadGateway, "credential mapping unavailable")
		return
	}
	target, known := probeTargets[injection.Env[0].Name]
	if !known {
		slog.ErrorContext(ctx, "credential test: no probe mapping for injected env",
			"env", injection.Env[0].Name, "runtime", req.Runtime, "class", string(class))
		funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestError)
		writeJSONError(w, http.StatusBadGateway, "no probe mapping for this credential family")
		return
	}

	material := strings.TrimSpace(string(secret.Data[key]))
	if material == "" {
		funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestInvalid)
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
			funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestInvalid)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  "validation failed",
				"fields": []fieldError{{Field: "modelEndpointRef", Message: "endpoint Secret has no endpointURL (arch §11 / story 7.5 shape)"}},
			})
			return
		}
		if err != nil {
			funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestError)
			writeJSON(w, http.StatusBadGateway, "endpoint read unavailable")
			return
		}
		// Egress containment (ISI-3891, the ISI-3887 N1 advisory): the
		// apiserver has high-trust egress, so a caller-chosen URL must
		// not turn the probe into a confused deputy against
		// control-plane internals. The target must sit inside the
		// squad's DECLARED egress allowlist (EgressPolicy CRs + Project
		// refs) — exactly what a Run could reach. A denial is an honest
		// red (a green on an endpoint the squad cannot reach would be a
		// false green for every Run), answered with one curated string
		// that never echoes the URL; the precise reason is logged
		// server-side.
		//
		// The guard also returns the address set it validated
		// (ISI-3899 F-A): the probe dial is PINNED to it. The tenant
		// controls BYO DNS, so a second, independent resolution inside
		// the dialer (low-TTL rebind or split-answer) could otherwise
		// validate one address and connect to another — resurrecting
		// the reachability oracle this guard exists to remove. Pinning
		// makes validate-time and dial-time resolution one resolution.
		var pinned []netip.Addr
		if s.probeEgress != nil {
			addrs, denial, gerr := s.probeEgress.Allow(r.Context(), ns, endpointURL)
			if gerr != nil {
				slog.ErrorContext(ctx, "credential test egress read failed",
					"namespace", ns, "name", name, "endpointRef", req.ModelEndpointRef, "error", gerr)
				funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestError)
				writeJSON(w, http.StatusBadGateway, "squad egress policy unavailable")
				return
			}
			if denial != nil {
				// The raw endpointURL is telemetry-dark (§5.3): a BYO URL can
				// carry a token in userinfo, so the log names the ref and the
				// guard's reason, never the URL.
				slog.WarnContext(ctx, "credential test BYO probe blocked by squad egress policy",
					"namespace", ns, "name", name, "endpointRef", req.ModelEndpointRef, "reason", denial)
				s.recordResult(r.Context(), team, name, s.agentsReferencing(r.Context(), ns, name), false)
				funnelOutcome(ctx, span, fn.credentialTest, outcomeCredTestBlocked)
				writeJSON(w, http.StatusOK, credentialTestResult{OK: false, Detail: credentialTestBlockedDetail})
				return
			}
			pinned = addrs
		}
		target = probeTarget{url: endpointURL, header: "Authorization", bearer: true, pinned: pinned}
	}

	probe := probeRequest{target: target, material: material}
	probeCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	status, perr := s.prober.probe(probeCtx, probe)
	if perr != nil {
		// Server-side only, and telemetry-dark on the target URL (§5.3): net
		// errors carry hostnames, never headers; a BYO URL may carry userinfo,
		// so it is not logged. The response detail stays curated
		// (foldProbeResult, NFR-2).
		slog.WarnContext(ctx, "credential test probe transport failure", "namespace", ns, "name", name, "error", perr)
	}
	result := foldProbeResult(target, status, perr)

	// Cache the last result as Team annotations (AC2 / AD-2): the
	// per-credential flag plus the per-AGENT flags the onboarding read
	// model already reads. Advisory: a cache failure logs and the probe
	// result is still answered honestly.
	referencing := s.agentsReferencing(r.Context(), ns, name)
	s.recordResult(r.Context(), team, name, referencing, result.OK)

	outcome := outcomeCredTestFailed
	if result.OK {
		outcome = outcomeCredTestPassed
	}
	slog.InfoContext(ctx, "credential test",
		"namespace", ns, "name", name, "runtime", req.Runtime, "class", string(class), "ok", result.OK, "principal", author.Principal)
	funnelOutcome(ctx, span, fn.credentialTest, outcome)
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
	// Guard-validated BYO targets dial through a PINNED transport
	// (ISI-3899 F-A): the DialContext connects only to the addresses the
	// egress guard validated and never re-resolves the URL's host, so a
	// DNS rebind or split-answer between guard and dial has nothing to
	// hit. Public provider targets (pinned empty) keep the plain client.
	client := p.client
	if len(req.target.pinned) > 0 {
		client = &http.Client{
			Timeout:   p.client.Timeout,
			Transport: pinnedProbeTransport(req.target.pinned),
		}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// pinnedProbeTransport is the dial plan for a guard-validated BYO probe
// (ISI-3899 F-A). The DialContext takes ONLY the port from the address
// the transport asks for and dials the guard-validated IPs by literal —
// the hostname the transport hands over is never resolved, never
// consulted. TLS keeps the URL host as ServerName and the request keeps
// its Host header, so certificate and virtual-host behavior are exactly
// what a Run dialing the same URL would see. Proxy is nil on purpose:
// the guard already refuses proxy-shaped egress, and an env-var proxy
// would redirect the dial away from the validated addresses.
func pinnedProbeTransport(addrs []netip.Addr) *http.Transport {
	return &http.Transport{
		DisableKeepAlives: true, // one-shot probe; leave no idle sockets
		Proxy:             nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("pinned probe dial: malformed address %q: %w", addr, err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil || port < 1 || port > 65535 {
				return nil, fmt.Errorf("pinned probe dial: bad port in %q", addr)
			}
			var dialer net.Dialer
			var firstErr error
			for _, a := range addrs {
				// netip.AddrPort.String() renders the bracketed v6
				// form; dialing a literal skips DNS entirely.
				conn, derr := dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(a, uint16(port)).String())
				if derr == nil {
					return conn, nil
				}
				if firstErr == nil {
					firstErr = derr
				}
			}
			if firstErr == nil {
				firstErr = errors.New("pinned probe dial: no validated addresses to dial")
			}
			return nil, firstErr
		},
	}
}

// muxVarsName reads a gorilla/mux path variable, matching the house pattern
// (artifacts.go / dashboard.go).
func muxVarsName(r *http.Request, key string) string {
	return mux.Vars(r)[key]
}
