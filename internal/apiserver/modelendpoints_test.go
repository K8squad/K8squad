package apiserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// ============================================================================
// ISI-5005 model-endpoint surface — merge-gate rubric (issue AC):
//   - list-models dials the right wire per provider (ollama tags vs OpenAI
//     /models) and returns the ids;
//   - 422 on unknown/curated provider, missing/garbage url, missing apiKey;
//   - non-admin is 403 on every write/list/list-models verb;
//   - the API key is NEVER echoed — not on write, not on list;
//   - a LAN (loopback) URL is allowed BY DESIGN;
//   - an upstream timeout/failure is an honest 5xx, never a fabricated list.
// ============================================================================

const meOperatorNS = "k8squad-system"

func newModelEndpointService(t *testing.T, objs ...client.Object) (*ModelEndpointService, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(secretWriteScheme(t)).WithObjects(objs...).Build()
	return NewModelEndpointService(c, meOperatorNS), c
}

func testModelEndpointServer(t *testing.T, svc *ModelEndpointService) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		meAdminToken: {Principal: "user:admin", IsAdmin: true},
		devToken:     {Principal: "user:alice"}, // authenticated non-admin
	}}
	srv := NewServer(Options{
		Authenticator:  NewCookieAuthenticator(resolver),
		Discussion:     discussion.NewHandler(nil),
		ModelEndpoints: svc,
	})
	return srv.Handler()
}

const meAdminToken = "me-admin-token"

func mePost(t *testing.T, h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		withSession(req, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func meGet(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		withSession(req, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ollamaFake serves the Ollama-native GET /api/tags wire.
func ollamaFake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("ollama probe hit %q, want /api/tags", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3:latest","model":"llama3:latest"},{"name":"qwen2.5:7b"}]}`))
	}))
}

// openAIFake serves the OpenAI-compatible GET /models wire and records the
// Authorization header it received (to prove the apiserver dials with the key).
func openAIFake(t *testing.T, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("openai probe hit %q, want /models", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"}]}`))
	}))
}

// TestListModelsOllamaWire — a url-mode provider (LAN Ollama) is dialed on its
// native /api/tags wire; the loopback URL is allowed BY DESIGN.
func TestListModelsOllamaWire(t *testing.T) {
	up := ollamaFake(t)
	defer up.Close()
	svc, _ := newModelEndpointService(t)
	h := testModelEndpointServer(t, svc)

	rec := mePost(t, h, "/api/modelendpoints/list-models", meAdminToken,
		`{"provider":"ollama","url":"`+up.URL+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("list-models: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out listModelsResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Models) != 2 || out.Models[0].ID != "llama3:latest" || out.Models[1].ID != "qwen2.5:7b" {
		t.Fatalf("models = %+v", out.Models)
	}
}

// TestListModelsOpenAIWire — an apiKey-mode provider is dialed on the
// OpenAI-compatible /models wire, with the key as a bearer token, never echoed.
func TestListModelsOpenAIWire(t *testing.T) {
	var gotAuth string
	up := openAIFake(t, &gotAuth)
	defer up.Close()
	svc, _ := newModelEndpointService(t)
	// Point the hosted provider's fixed base at the fake.
	svc.providers["deepseek"] = providerSpec{ID: "deepseek", Mode: modeAPIKey, Wire: wireOpenAI, BaseURL: up.URL}
	h := testModelEndpointServer(t, svc)

	const key = "sk-deepseek-SECRET"
	rec := mePost(t, h, "/api/modelendpoints/list-models", meAdminToken,
		`{"provider":"deepseek","apiKey":"`+key+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("list-models: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer "+key {
		t.Fatalf("upstream Authorization = %q, want bearer with the key", gotAuth)
	}
	if strings.Contains(rec.Body.String(), key) {
		t.Fatalf("key echoed in list-models response: %s", rec.Body.String())
	}
	var out listModelsResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Models) != 2 || out.Models[0].ID != "deepseek-chat" {
		t.Fatalf("models = %+v", out.Models)
	}
}

// TestListModelsValidation — the 422 field-validation surface.
func TestListModelsValidation(t *testing.T) {
	svc, _ := newModelEndpointService(t)
	h := testModelEndpointServer(t, svc)
	cases := []struct {
		name string
		body string
	}{
		{"unknown provider", `{"provider":"nope"}`},
		{"curated-only provider", `{"provider":"anthropic"}`},
		{"url-mode missing url", `{"provider":"ollama"}`},
		{"garbage url", `{"provider":"ollama","url":"::::not a url"}`},
		{"non-http scheme", `{"provider":"ollama","url":"ftp://host/x"}`},
		{"apiKey-mode missing key", `{"provider":"deepseek"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := mePost(t, h, "/api/modelendpoints/list-models", meAdminToken, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("got %d want 422 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestListModelsUpstreamTimeout — a slow/unreachable upstream is an honest 5xx,
// never a fabricated model list.
func TestListModelsUpstreamTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer slow.Close()
	svc, _ := newModelEndpointService(t)
	svc.http.Timeout = 40 * time.Millisecond // shorten so the test is fast
	h := testModelEndpointServer(t, svc)

	rec := mePost(t, h, "/api/modelendpoints/list-models", meAdminToken,
		`{"provider":"ollama","url":"`+slow.URL+`"}`)
	if rec.Code < 500 || rec.Code >= 600 {
		t.Fatalf("timeout: got %d want a 5xx (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "could not reach") {
		t.Fatalf("timeout error body not honest: %s", rec.Body.String())
	}
}

// TestListModelsUpstreamNon2xx — an upstream 4xx/5xx (bad key/endpoint) maps to a
// 502 naming the upstream status.
func TestListModelsUpstreamNon2xx(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer up.Close()
	svc, _ := newModelEndpointService(t)
	h := testModelEndpointServer(t, svc)

	rec := mePost(t, h, "/api/modelendpoints/list-models", meAdminToken,
		`{"provider":"ollama","url":"`+up.URL+`"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("non-2xx: got %d want 502 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestModelEndpointNonAdmin403 — every verb is admin-tier; an authenticated
// non-admin gets 403 and an unauthenticated caller gets 401.
func TestModelEndpointNonAdmin403(t *testing.T) {
	svc, _ := newModelEndpointService(t)
	h := testModelEndpointServer(t, svc)

	writes := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/modelendpoints/list-models", `{"provider":"ollama","url":"http://x"}`},
		{http.MethodPost, "/api/modelendpoints", `{"provider":"ollama","url":"http://x"}`},
		{http.MethodGet, "/api/modelendpoints", ""},
	}
	for _, wcase := range writes {
		var rec *httptest.ResponseRecorder
		if wcase.method == http.MethodGet {
			rec = meGet(t, h, wcase.path, devToken)
		} else {
			rec = mePost(t, h, wcase.path, devToken, wcase.body)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s non-admin: got %d want 403 (body %s)", wcase.method, wcase.path, rec.Code, rec.Body.String())
		}
		// Unauthenticated → 401.
		if wcase.method == http.MethodGet {
			rec = meGet(t, h, wcase.path, "")
		} else {
			rec = mePost(t, h, wcase.path, "", wcase.body)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s unauth: got %d want 401", wcase.method, wcase.path, rec.Code)
		}
	}
}

// TestModelEndpointCreateAndList — a write lands ONE labelled Secret in the
// operator namespace (endpointURL/apiToken keys), the response and the GET list
// carry a name/hasToken but NEVER the key, and a re-POST upserts in place.
func TestModelEndpointCreateAndList(t *testing.T) {
	svc, c := newModelEndpointService(t)
	h := testModelEndpointServer(t, svc)

	const key = "sk-zai-SECRET-VALUE"
	svc.providers["zai"] = providerSpec{ID: "zai", Mode: modeAPIKey, Wire: wireOpenAI, BaseURL: "https://api.z.ai/api/paas/v4"}

	rec := mePost(t, h, "/api/modelendpoints", meAdminToken,
		`{"name":"team-zai","provider":"zai","apiKey":"`+key+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), key) {
		t.Fatalf("key echoed in create response: %s", rec.Body.String())
	}
	var out modelEndpointCreateResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Name != "team-zai" || out.Provider != "zai" || out.Operation != "created" {
		t.Fatalf("result = %+v", out)
	}

	// Secret landed with the resolve.go-shaped keys + labels in the operator ns.
	var got corev1.Secret
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: meOperatorNS, Name: "team-zai"}, &got); err != nil {
		t.Fatalf("secret not stored: %v", err)
	}
	if string(got.Data[modelendpoint.KeyEndpointURL]) != "https://api.z.ai/api/paas/v4" {
		t.Fatalf("endpointURL = %q", got.Data[modelendpoint.KeyEndpointURL])
	}
	if string(got.Data[modelendpoint.KeyAPIToken]) != key {
		t.Fatalf("apiToken not stored under the resolve.go key")
	}
	if got.Labels[LabelModelEndpoint] != LabelModelEndpointValue || got.Labels[LabelModelEndpointProvider] != "zai" {
		t.Fatalf("labels = %v", got.Labels)
	}

	// A url-mode endpoint WITHOUT a token → hasToken false.
	rec = mePost(t, h, "/api/modelendpoints", meAdminToken,
		`{"name":"lan-ollama","provider":"ollama","url":"http://10.0.0.5:11434"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create ollama: got %d want 201 (body %s)", rec.Code, rec.Body.String())
	}

	// GET list: names/providers/urls/hasToken, no secret data anywhere.
	lrec := meGet(t, h, "/api/modelendpoints", meAdminToken)
	if lrec.Code != http.StatusOK {
		t.Fatalf("list: got %d want 200 (body %s)", lrec.Code, lrec.Body.String())
	}
	if strings.Contains(lrec.Body.String(), key) {
		t.Fatalf("key echoed in list response: %s", lrec.Body.String())
	}
	var list struct {
		Endpoints []modelEndpointRow `json:"endpoints"`
	}
	if err := json.Unmarshal(lrec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Endpoints) != 2 {
		t.Fatalf("endpoints = %+v", list.Endpoints)
	}
	byName := map[string]modelEndpointRow{}
	for _, e := range list.Endpoints {
		byName[e.Name] = e
	}
	if e := byName["team-zai"]; !e.HasToken || e.Provider != "zai" {
		t.Fatalf("team-zai row = %+v", e)
	}
	if e := byName["lan-ollama"]; e.HasToken || e.URL != "http://10.0.0.5:11434" || e.Provider != "ollama" {
		t.Fatalf("lan-ollama row = %+v", e)
	}

	// Re-POST the same name upserts in place (200 updated), not a 409.
	rec = mePost(t, h, "/api/modelendpoints", meAdminToken,
		`{"name":"team-zai","provider":"zai","apiKey":"sk-zai-ROTATED"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: meOperatorNS, Name: "team-zai"}, &got); err != nil {
		t.Fatalf("secret gone after upsert: %v", err)
	}
	if string(got.Data[modelendpoint.KeyAPIToken]) != "sk-zai-ROTATED" {
		t.Fatalf("upsert did not rotate the token")
	}
}

// TestModelEndpointCreateValidation — the write shares the list-models validation
// (unknown/curated provider, url-mode needs a valid url, apiKey-mode needs a key)
// plus a DNS-1123 name check.
func TestModelEndpointCreateValidation(t *testing.T) {
	svc, _ := newModelEndpointService(t)
	h := testModelEndpointServer(t, svc)
	cases := []string{
		`{"provider":"nope","url":"http://x"}`,
		`{"provider":"anthropic"}`,
		`{"provider":"ollama"}`,                                    // missing url
		`{"provider":"ollama","url":"http://x","name":"Bad_Name"}`, // invalid name
		`{"provider":"deepseek"}`,                                  // missing key
	}
	for i, body := range cases {
		rec := mePost(t, h, "/api/modelendpoints", meAdminToken, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("case %d: got %d want 422 (body %s)", i, rec.Code, rec.Body.String())
		}
	}
}

// TestModelEndpointNameDefaultsToProvider — omitting name defaults it to the
// provider id.
func TestModelEndpointNameDefaultsToProvider(t *testing.T) {
	svc, c := newModelEndpointService(t)
	h := testModelEndpointServer(t, svc)

	rec := mePost(t, h, "/api/modelendpoints", meAdminToken,
		`{"provider":"ollama","url":"http://10.0.0.9:11434"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: meOperatorNS, Name: "ollama"}, &corev1.Secret{}); err != nil {
		t.Fatalf("secret not stored under default name %q: %v", "ollama", err)
	}
}

// TestModelEndpointProvidersRegistry — the data-only registry is session-gated
// (any authenticated caller) and flags the curated-only providers.
func TestModelEndpointProvidersRegistry(t *testing.T) {
	svc, _ := newModelEndpointService(t)
	h := testModelEndpointServer(t, svc)

	// Non-admin CAN read the registry (static metadata).
	rec := meGet(t, h, "/api/modelendpoints/providers", devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("providers: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Providers []providerSpec `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]providerSpec{}
	for _, p := range out.Providers {
		byID[p.ID] = p
	}
	if !byID["anthropic"].CuratedOnly || !byID["openai"].CuratedOnly {
		t.Fatalf("anthropic/openai must be curated-only: %+v", out.Providers)
	}
	if byID["ollama"].Mode != modeURL || byID["deepseek"].Mode != modeAPIKey {
		t.Fatalf("modes wrong: %+v", out.Providers)
	}
	// Unauthenticated → 401.
	if rec := meGet(t, h, "/api/modelendpoints/providers", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("providers unauth: got %d want 401", rec.Code)
	}
}

// TestModelEndpointNilServiceNotImplemented — a nil service keeps the documented
// 501 (cluster-less dev run), like the compose/otel surfaces.
func TestModelEndpointNilServiceNotImplemented(t *testing.T) {
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		meAdminToken: {Principal: "user:admin", IsAdmin: true},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
	})
	h := srv.Handler()
	rec := mePost(t, h, "/api/modelendpoints", meAdminToken, `{"provider":"ollama","url":"http://x"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil service: got %d want 501 (body %s)", rec.Code, rec.Body.String())
	}
}
