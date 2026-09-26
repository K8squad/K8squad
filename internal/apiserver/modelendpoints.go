package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// ============================================================================
// Model-endpoint surface (ISI-5005, child of ISI-4989; the deferred ISI-4890
// AC6 fast-follow). Two capabilities the console needs so an operator can wire
// a BYO model provider WITHOUT hand-crafting a Secret first:
//
//  1. LIST MODELS. POST /api/modelendpoints/list-models {provider,url?,apiKey?}
//     → {models:[{id,label?}]}. The apiserver — NOT the browser — dials the
//     provider (an Ollama-native GET {url}/api/tags, or an OpenAI-compatible
//     GET {base}/models) and returns the available model ids. Dialing from the
//     apiserver keeps the (optional) API key off the browser wire and lets a
//     LAN Ollama endpoint (unreachable from the user's browser) be probed from
//     inside the cluster.
//
//  2. ENDPOINT SECRET WRITE. POST /api/modelendpoints {name?,provider,url?,
//     apiKey?} upserts ONE Secret (endpointURL/apiToken keys) in the operator
//     namespace, labelled ksquad.io/model-endpoint — the SAME shape
//     pkg/modelendpoint/resolve.go resolves an Agent's modelEndpointRef from,
//     and the same namespace the ModelConfig "default" singleton lives in. GET
//     /api/modelendpoints lists them WITHOUT any secret data.
//
// Security posture (mirrors internal/apiserver/otelconfig.go write half +
// internal/apiserver/credentials.go):
//   - Session-gated, admin-tier. Wiring a cluster-wide model provider is
//     platform config, not per-tenant — the SAME write scope as the org-default
//     ModelConfig (composecrd.go: "the org default model is admin-only").
//   - The API key enters through the bounded request body and leaves ONLY
//     inside the Secret's Data map. No response body, error branch, or log line
//     ever echoes it; GET returns hasToken, never the token.
//   - Secret writes go through the apiserver ServiceAccount (ISI-3546): the
//     compose SA keeps NO secret-write RBAC. The namespaced Role granting
//     secrets get/list/create/update in the operator namespace lives in
//     config/helm/templates/control-plane/rbac.yaml and is pinned by
//     TestApiserverModelEndpointRoleLeastPrivilege.
//
// SSRF note (BY DESIGN): list-models validates scheme (http/https) + host, the
// same shape resolve.go enforces, but it does NOT block private/LAN ranges. LAN
// Ollama (http://10.x / http://192.168.x / http://ollama.svc) is the primary
// url-mode provider, so private ranges MUST stay reachable. Redirects are
// refused (an endpoint cannot bounce the probe to a different host), the dial is
// bounded to 5s, and the response body is size-capped.

// Secret labels stamped on every endpoint Secret this surface writes. The value
// const is the label the GET list and any operator-side selector match on.
const (
	LabelModelEndpoint         = "ksquad.io/model-endpoint"
	LabelModelEndpointValue    = "true"
	LabelModelEndpointProvider = "ksquad.io/model-endpoint-provider"
)

const (
	// modelEndpointDialTimeout bounds the whole list-models probe (dial + read).
	modelEndpointDialTimeout = 5 * time.Second
	// modelEndpointMaxRespBytes caps the provider response we read — a model
	// list is a few KiB; anything past this is truncated so a hostile or broken
	// endpoint cannot exhaust apiserver memory.
	modelEndpointMaxRespBytes = 1 << 20 // 1 MiB
	// modelEndpointMaxBodyBytes bounds the request body (defense in depth, same
	// discipline as the compose/credential/otel write routes).
	modelEndpointMaxBodyBytes = 16 << 10
	// modelEndpointMaxKeyBytes floors the API key size — a bearer token is
	// bytes, not a pasted file.
	modelEndpointMaxKeyBytes = 8 << 10
)

// providerMode is how an endpoint is addressed: the caller supplies the base URL
// (LAN Ollama, generic OpenAI-compatible), or the provider has a fixed hosted
// base and the caller supplies only an API key.
type providerMode string

const (
	modeURL    providerMode = "url"
	modeAPIKey providerMode = "apiKey"
)

// providerWire is the model-listing dialect: Ollama-native tags vs the
// OpenAI-compatible /models endpoint.
type providerWire string

const (
	wireOllama providerWire = "ollama" // GET {base}/api/tags  → {models:[{name}]}
	wireOpenAI providerWire = "openai" // GET {base}/models     → {data:[{id}]}
)

// providerSpec is one row of the data-only provider registry.
type providerSpec struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Mode/Wire are empty for curated-only providers (they are never dialed).
	Mode providerMode `json:"mode,omitempty"`
	Wire providerWire `json:"wire,omitempty"`
	// BaseURL is the fixed hosted base for an apiKey-mode provider; empty for a
	// url-mode provider (the caller supplies the URL).
	BaseURL string `json:"-"`
	// CuratedOnly flags anthropic/openai: the console special-cases claude/codex
	// (curated model lists), so this surface neither lists nor stores them.
	CuratedOnly bool `json:"curatedOnly,omitempty"`
	// AllowKey marks a url-mode provider that may ALSO carry an optional key (the
	// generic OpenAI-compatible provider behind an auth proxy).
	AllowKey bool `json:"allowKey,omitempty"`
}

// defaultProviders is the built-in registry (ISI-5005 scope). Each request-time
// service copies it so a test can override a hosted base URL without mutating
// package state.
func defaultProviders() map[string]providerSpec {
	return map[string]providerSpec{
		"ollama": {ID: "ollama", Label: "Ollama (local)", Mode: modeURL, Wire: wireOllama},
		"zai": {ID: "zai", Label: "Z.ai", Mode: modeAPIKey, Wire: wireOpenAI,
			BaseURL: "https://api.z.ai/api/paas/v4"},
		"deepseek": {ID: "deepseek", Label: "DeepSeek", Mode: modeAPIKey, Wire: wireOpenAI,
			BaseURL: "https://api.deepseek.com"},
		"kimi-moonshot": {ID: "kimi-moonshot", Label: "Kimi (Moonshot)", Mode: modeAPIKey, Wire: wireOpenAI,
			BaseURL: "https://api.moonshot.cn/v1"},
		"openai-compatible": {ID: "openai-compatible", Label: "OpenAI-compatible (custom)", Mode: modeURL, Wire: wireOpenAI, AllowKey: true},
		// Curated-only: the console renders claude/codex from a curated list; this
		// surface never dials or stores them.
		"anthropic": {ID: "anthropic", Label: "Anthropic (Claude)", CuratedOnly: true},
		"openai":    {ID: "openai", Label: "OpenAI (Codex)", CuratedOnly: true},
	}
}

// ModelEndpointClient is the cluster seam the endpoint write/list path needs:
// Secret Get/Create/Update (upsert) plus List (the GET read model). Production
// wires a DIRECT (uncached) controller-runtime client — a just-created Secret
// must be durable at the API server, and a write path must never read its own
// staleness through a cache. Tests wire a fake client.
type ModelEndpointClient interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
	Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// ModelEndpointService owns the three handlers. namespace is the operator
// namespace the endpoint Secrets and the ModelConfig singleton share.
type ModelEndpointService struct {
	client    ModelEndpointClient
	namespace string
	http      *http.Client
	providers map[string]providerSpec
}

// NewModelEndpointService builds the surface. namespace empty ⇒ the resolver's
// DefaultSystemNamespace ("k8squad-system"), so the write target and the
// resolver's read target can never drift. The http.Client refuses redirects and
// is bounded to modelEndpointDialTimeout.
func NewModelEndpointService(c ModelEndpointClient, namespace string) *ModelEndpointService {
	ns := namespace
	if ns == "" {
		ns = modelendpoint.DefaultSystemNamespace
	}
	return &ModelEndpointService{
		client:    c,
		namespace: ns,
		http: &http.Client{
			Timeout: modelEndpointDialTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("model-endpoint probe does not follow redirects")
			},
		},
		providers: defaultProviders(),
	}
}

// ── provider registry read ──────────────────────────────────────────────────

// handleProviders serves the data-only registry to the console's
// ProviderModelPicker. Session-gated (any authenticated caller): it is static
// metadata (ids/labels/modes), never a credential or a per-tenant fact.
func (s *ModelEndpointService) handleProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := meRequireSession(w, r); !ok {
		return
	}
	rows := make([]providerSpec, 0, len(s.providers))
	for _, p := range s.providers {
		rows = append(rows, p)
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].ID < rows[b].ID })
	writeJSON(w, http.StatusOK, map[string]any{"providers": rows})
}

// ── list models ─────────────────────────────────────────────────────────────

type listModelsRequest struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
	APIKey   string `json:"apiKey"`
}

// modelInfo is one listed model. Label is optional (the wire may or may not
// carry a display name distinct from the id).
type modelInfo struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

type listModelsResult struct {
	Models []modelInfo `json:"models"`
}

// handleListModels is POST /api/modelendpoints/list-models. Admin-tier: it dials
// a caller-chosen endpoint from inside the cluster, the same trust boundary as
// the org-default model write.
func (s *ModelEndpointService) handleListModels(w http.ResponseWriter, r *http.Request) {
	author, ok := meRequireAdmin(w, r)
	if !ok {
		return
	}
	var req listModelsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return // decodeJSON answered 400
	}

	spec, errs := s.resolveProbe(req)
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "fields": errs})
		return
	}

	result, status, msg := s.probe(r.Context(), spec.spec, spec.base, spec.apiKey)
	if status != 0 {
		// Honest upstream failure — never a fabricated model list.
		slog.WarnContext(r.Context(), "model-endpoint list-models probe failed",
			"provider", req.Provider, "principal", author.Principal, "detail", msg)
		writeJSONError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// resolvedProbe carries the validated dial inputs.
type resolvedProbe struct {
	spec   providerSpec
	base   string
	apiKey string
}

// resolveProbe validates {provider,url,apiKey} into a dial target, returning
// field errors for a 422. It NEVER copies the key into an error message.
func (s *ModelEndpointService) resolveProbe(req listModelsRequest) (resolvedProbe, []fieldError) {
	spec, errs := s.resolveTarget(req.Provider, req.URL, req.APIKey)
	if len(errs) > 0 {
		return resolvedProbe{}, errs
	}
	return spec, nil
}

// resolveTarget is the shared validation for both list-models and the write: it
// looks up the provider, rejects an unknown or curated-only provider, and
// resolves the base URL + key per the provider mode. url-mode requires a valid
// http(s) URL; apiKey-mode requires a key and uses the fixed hosted base.
func (s *ModelEndpointService) resolveTarget(provider, rawURL, apiKey string) (resolvedProbe, []fieldError) {
	var errs []fieldError
	spec, known := s.providers[strings.TrimSpace(provider)]
	if !known {
		return resolvedProbe{}, []fieldError{{Field: "provider", Message: "unknown provider"}}
	}
	if spec.CuratedOnly {
		return resolvedProbe{}, []fieldError{{Field: "provider", Message: "provider is curated-only (the console renders its model list directly)"}}
	}

	out := resolvedProbe{spec: spec}
	switch spec.Mode {
	case modeURL:
		base, err := validateEndpointURL(rawURL)
		if err != nil {
			errs = append(errs, fieldError{Field: "url", Message: err.Error()})
		} else {
			out.base = base
		}
		// A url-mode provider may carry an optional key only when AllowKey.
		if key := strings.TrimSpace(apiKey); key != "" {
			if !spec.AllowKey {
				errs = append(errs, fieldError{Field: "apiKey", Message: "this provider does not take an API key"})
			} else if len(key) > modelEndpointMaxKeyBytes {
				errs = append(errs, fieldError{Field: "apiKey", Message: "API key exceeds the size limit"})
			} else {
				out.apiKey = key
			}
		}
	case modeAPIKey:
		out.base = spec.BaseURL
		key := strings.TrimSpace(apiKey)
		switch {
		case key == "":
			errs = append(errs, fieldError{Field: "apiKey", Message: "API key is required for this provider"})
		case len(key) > modelEndpointMaxKeyBytes:
			errs = append(errs, fieldError{Field: "apiKey", Message: "API key exceeds the size limit"})
		default:
			out.apiKey = key
		}
	}
	if len(errs) > 0 {
		return resolvedProbe{}, errs
	}
	return out, nil
}

// validateEndpointURL mirrors pkg/modelendpoint/resolve.go: a parseable http(s)
// URL with a non-empty host. Private/LAN ranges stay reachable BY DESIGN (LAN
// Ollama) — this is deliberately NOT an SSRF allowlist. The rejected string is
// never echoed verbatim.
func validateEndpointURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("must be a valid http(s) URL with a host")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// probe dials the provider and parses its model list. Returns (result, 0, "") on
// success, or (zero, 5xx, honest message) on any dial/read/parse failure — never
// a fabricated list. status is an HTTP status for the CALLER (502: the apiserver
// could not get an honest answer from the upstream).
func (s *ModelEndpointService) probe(ctx context.Context, spec providerSpec, base, apiKey string) (listModelsResult, int, string) {
	probeURL := base
	switch spec.Wire {
	case wireOllama:
		probeURL = base + "/api/tags"
	case wireOpenAI:
		probeURL = base + "/models"
	default:
		return listModelsResult{}, http.StatusInternalServerError, "provider has no model-listing wire"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return listModelsResult{}, http.StatusBadGateway, "could not build the provider request"
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return listModelsResult{}, http.StatusBadGateway, "could not reach the model provider (check the URL/network; redirects are not followed)"
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, modelEndpointMaxRespBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return listModelsResult{}, http.StatusBadGateway,
			fmt.Sprintf("the model provider returned %d (check the credential/endpoint)", resp.StatusCode)
	}

	models, err := parseModelList(spec.Wire, body)
	if err != nil {
		return listModelsResult{}, http.StatusBadGateway, "the model provider returned an unexpected response shape"
	}
	return listModelsResult{Models: models}, 0, ""
}

// parseModelList decodes the wire-specific model list. Ollama-native
// ({models:[{name}]}) vs OpenAI-compatible ({data:[{id}]}).
func parseModelList(wire providerWire, body []byte) ([]modelInfo, error) {
	out := []modelInfo{}
	switch wire {
	case wireOllama:
		var payload struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		for _, m := range payload.Models {
			id := m.Name
			if id == "" {
				id = m.Model
			}
			if id != "" {
				out = append(out, modelInfo{ID: id})
			}
		}
	case wireOpenAI:
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		for _, m := range payload.Data {
			if m.ID != "" {
				out = append(out, modelInfo{ID: m.ID})
			}
		}
	default:
		return nil, fmt.Errorf("unknown wire %q", wire)
	}
	return out, nil
}

// ── endpoint Secret write / list ─────────────────────────────────────────────

type modelEndpointCreateRequest struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	URL      string `json:"url"`
	APIKey   string `json:"apiKey"`
}

// modelEndpointCreateResult is the write response — a name, NEVER the key.
type modelEndpointCreateResult struct {
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Operation string `json:"operation"` // "created" | "updated"
}

// modelEndpointRow is one GET /api/modelendpoints row — no secret data.
type modelEndpointRow struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	URL      string `json:"url"`
	HasToken bool   `json:"hasToken"`
}

// handleCreate is POST /api/modelendpoints — upsert one endpoint Secret in the
// operator namespace. Admin-tier. The key is written into Secret data and never
// echoed.
func (s *ModelEndpointService) handleCreate(w http.ResponseWriter, r *http.Request) {
	author, ok := meRequireAdmin(w, r)
	if !ok {
		return
	}
	var req modelEndpointCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	target, errs := s.resolveTarget(req.Provider, req.URL, req.APIKey)
	// The name defaults to the provider id when omitted; validate it as a
	// DNS-1123 subdomain (Secret name rule).
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = target.spec.ID
	}
	if msg := dns1123Name(name); msg != "" {
		errs = append(errs, fieldError{Field: "name", Message: msg})
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "fields": errs})
		return
	}

	data := map[string][]byte{modelendpoint.KeyEndpointURL: []byte(target.base)}
	if target.apiKey != "" {
		data[modelendpoint.KeyAPIToken] = []byte(target.apiKey)
	}
	labels := map[string]string{
		LabelModelEndpoint:         LabelModelEndpointValue,
		LabelModelEndpointProvider: target.spec.ID,
	}

	operation, status, msg := s.upsertSecret(r.Context(), name, labels, data)
	if status != 0 {
		if status >= 500 {
			slog.ErrorContext(r.Context(), "model-endpoint Secret write failed",
				"namespace", s.namespace, "name", name, "provider", target.spec.ID, "principal", author.Principal, "detail", msg)
		}
		writeJSONError(w, status, msg)
		return
	}

	slog.InfoContext(r.Context(), "model endpoint upserted",
		"namespace", s.namespace, "name", name, "provider", target.spec.ID, "operation", operation, "principal", author.Principal)
	code := http.StatusOK
	if operation == "created" {
		code = http.StatusCreated
	}
	writeJSON(w, code, modelEndpointCreateResult{Name: name, Provider: target.spec.ID, Operation: operation})
}

// upsertSecret does the Get-then-Create-or-Update. Get → NotFound ⇒ Create;
// present ⇒ Update carrying the live resourceVersion (compare-and-swap). Returns
// ("created"|"updated", 0, "") on success or ("", 5xx, message) on failure.
func (s *ModelEndpointService) upsertSecret(ctx context.Context, name string, labels map[string]string, data map[string][]byte) (string, int, string) {
	existing := &corev1.Secret{}
	getErr := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(getErr):
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace, Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		if err := s.client.Create(ctx, secret); err != nil {
			status, msg := secretWriteStatus(err)
			return "", status, msg
		}
		return "created", 0, ""
	case getErr != nil:
		return "", http.StatusBadGateway, "endpoint store read-before-write unavailable"
	default:
		// Overwrite the managed keys + labels; keep the object identity and carry
		// the resourceVersion so the Update is a compare-and-swap (a concurrent
		// change surfaces as a 409, not a lost write).
		existing.Type = corev1.SecretTypeOpaque
		existing.Data = data
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		for k, v := range labels {
			existing.Labels[k] = v
		}
		if err := s.client.Update(ctx, existing); err != nil {
			status, msg := secretWriteStatus(err)
			return "", status, msg
		}
		return "updated", 0, ""
	}
}

// secretWriteStatus maps a Secret write error onto the caller-facing status. A
// Forbidden is an RBAC regression (the Role lost a verb) — loud in logs, opaque
// to the caller.
func secretWriteStatus(err error) (int, string) {
	switch {
	case apierrors.IsConflict(err):
		return http.StatusConflict, "the endpoint was modified concurrently; retry"
	case apierrors.IsForbidden(err):
		return http.StatusBadGateway, "endpoint store rejected the write"
	default:
		return http.StatusBadGateway, "endpoint store unavailable"
	}
}

// handleList is GET /api/modelendpoints — the label-scoped list, no secret data.
// Admin-tier (the endpoint config is platform config, same scope as the write).
func (s *ModelEndpointService) handleList(w http.ResponseWriter, r *http.Request) {
	if _, ok := meRequireAdmin(w, r); !ok {
		return
	}
	var secrets corev1.SecretList
	if err := s.client.List(r.Context(), &secrets,
		client.InNamespace(s.namespace),
		client.MatchingLabels{LabelModelEndpoint: LabelModelEndpointValue}); err != nil {
		slog.ErrorContext(r.Context(), "model-endpoint list failed", "namespace", s.namespace, "error", err)
		writeJSONError(w, http.StatusBadGateway, "endpoint store unavailable")
		return
	}
	rows := make([]modelEndpointRow, 0, len(secrets.Items))
	for i := range secrets.Items {
		sec := &secrets.Items[i]
		rows = append(rows, modelEndpointRow{
			Name:     sec.Name,
			Provider: sec.Labels[LabelModelEndpointProvider],
			URL:      string(sec.Data[modelendpoint.KeyEndpointURL]),
			HasToken: len(sec.Data[modelendpoint.KeyAPIToken]) > 0,
		})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Name < rows[b].Name })
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": rows})
}

// ── session / admin gates ─────────────────────────────────────────────────────

// requireSession returns the authenticated author or answers 401.
func meRequireSession(w http.ResponseWriter, r *http.Request) (discussion.AuthorContext, bool) {
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return discussion.AuthorContext{}, false
	}
	return author, true
}

// requireAdmin returns the authenticated admin author or answers 401/403.
func meRequireAdmin(w http.ResponseWriter, r *http.Request) (discussion.AuthorContext, bool) {
	author, ok := meRequireSession(w, r)
	if !ok {
		return discussion.AuthorContext{}, false
	}
	if !author.IsAdmin {
		writeJSONError(w, http.StatusForbidden, "model-endpoint administration is admin-only")
		return discussion.AuthorContext{}, false
	}
	return author, true
}
