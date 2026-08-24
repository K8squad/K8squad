package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ============================================================================
// The real (semantic) embedder behind the §7.1 seam (Story 6.2 / ISI-2895)
// ============================================================================
//
// HashingEmbedder is the dependency-free default; it clusters by shared vocabulary but has no semantic
// understanding ("k8s" and "kubernetes" are orthogonal to it). Config.EmbedderEndpoint has always been
// the seam's knob for swapping in a real model — this wires it. HTTPEmbedder POSTs to an OpenAI-shaped
// `/v1/embeddings` endpoint (the de-facto standard exposed by llama.cpp, text-embeddings-inference,
// vLLM, Ollama's compat shim, and OpenAI itself) and returns the returned vector. NOTHING above the
// seam changes: the store, read tools, and write tool consume the Embedder interface, not this type.
//
// The seam's one hard contract is dimension: a returned vector whose length != EmbeddingDim is a
// legible error at embed time, never a silent truncation that pgvector would later reject with a far
// less actionable message. Wiring a model whose native dimension differs from EmbeddingDim (768) is a
// deployment misconfiguration surfaced here, immediately.

// HTTPEmbedder is the live semantic embedder. It is safe for concurrent use (the http.Client is).
type HTTPEmbedder struct {
	endpoint string
	model    string
	dim      int
	client   *http.Client
}

var _ Embedder = (*HTTPEmbedder)(nil)

// NewHTTPEmbedder builds a live embedder client for the configured endpoint/model. endpoint is the full
// embeddings URL (e.g. http://embedder.ksquad.svc:8080/v1/embeddings); model is the model id passed in
// the request body. A zero timeout defaults to 30s so a wedged model server can never block a write or
// a read indefinitely.
func NewHTTPEmbedder(endpoint, model string, timeout time.Duration) *HTTPEmbedder {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	m := model
	if m == "" {
		m = "local-default"
	}
	return &HTTPEmbedder{
		endpoint: endpoint,
		model:    m,
		dim:      EmbeddingDim,
		client:   &http.Client{Timeout: timeout},
	}
}

// embeddingsRequest / embeddingsResponse are the OpenAI-compatible embeddings shape. `input` is a
// single string (v1 embeds one text at a time — the read tool embeds one query, the write tool one
// body); the response's first data element carries the vector.
type embeddingsRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed POSTs the text to the embeddings endpoint and returns the model's vector, failing closed on any
// transport error, non-2xx status, empty result, or dimension mismatch. It NEVER falls back to the
// hashing embedder mid-flight: a configured live embedder that is failing is an error the caller sees,
// not a silent switch to a different, incompatible vector space (which would corrupt recall).
func (e *HTTPEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embeddingsRequest{Model: e.model, Input: text})
	if err != nil {
		return nil, fmt.Errorf("embedder: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedder: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedder: call %s: %w", e.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("embedder: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedder: %s returned %d: %s", e.endpoint, resp.StatusCode, truncate(raw, 256))
	}

	var out embeddingsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embedder: decode response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("embedder: model error: %s", out.Error.Message)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedder: empty embedding from %s", e.endpoint)
	}
	vec := out.Data[0].Embedding
	if len(vec) != e.dim {
		return nil, fmt.Errorf("embedder: model returned dim %d != configured dim %d (endpoint/model mismatch)", len(vec), e.dim)
	}
	return vec, nil
}

// NewEmbedder is the §7.1 seam factory: return the live HTTP embedder when an endpoint is configured,
// otherwise the deterministic local default. This is the ONE place the choice is made — main and any
// test harness get the right embedder from the Config alone, with no scattered branching.
func NewEmbedder(cfg Config) Embedder {
	if cfg.EmbedderEndpoint != "" {
		return NewHTTPEmbedder(cfg.EmbedderEndpoint, cfg.EmbedderModel, 0)
	}
	return NewHashingEmbedder()
}

// truncate bounds an error body so a chatty model server can't blow up a log line.
func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
