package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPEmbedder_ReturnsModelVector verifies the happy path: an OpenAI-shaped embeddings response is
// decoded and its (correctly-dimensioned) vector returned unchanged.
func TestHTTPEmbedder_ReturnsModelVector(t *testing.T) {
	want := make([]float32, EmbeddingDim)
	for i := range want {
		want[i] = 0.001 * float32(i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Input == "" {
			t.Errorf("empty input forwarded")
		}
		_ = json.NewEncoder(w).Encode(embeddingsResponse{Data: []struct {
			Embedding []float32 `json:"embedding"`
		}{{Embedding: want}}})
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "test-model", 0)
	got, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(got) != EmbeddingDim || got[10] != want[10] {
		t.Fatalf("vector mismatch: got[10]=%v want %v", got[10], want[10])
	}
}

// TestHTTPEmbedder_RejectsWrongDim is the seam's one hard contract: a model whose native dimension
// differs from EmbeddingDim is a legible embed-time error, never a silent truncation.
func TestHTTPEmbedder_RejectsWrongDim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embeddingsResponse{Data: []struct {
			Embedding []float32 `json:"embedding"`
		}{{Embedding: make([]float32, 384)}}}) // wrong dim
	}))
	defer srv.Close()
	if _, err := NewHTTPEmbedder(srv.URL, "m", 0).Embed(context.Background(), "x"); err == nil {
		t.Fatalf("expected dimension-mismatch error")
	}
}

// TestHTTPEmbedder_FailsClosedOnHTTPError asserts a non-2xx is surfaced, never swallowed into a
// hashing fallback that would corrupt the shared vector space.
func TestHTTPEmbedder_FailsClosedOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := NewHTTPEmbedder(srv.URL, "m", 0).Embed(context.Background(), "x"); err == nil {
		t.Fatalf("expected error on 503")
	}
}

// TestNewEmbedder_SelectsBySeam verifies the factory: an endpoint yields the live client, its absence
// the deterministic default.
func TestNewEmbedder_SelectsBySeam(t *testing.T) {
	if _, ok := NewEmbedder(Config{EmbedderEndpoint: "http://x/v1/embeddings"}).(*HTTPEmbedder); !ok {
		t.Fatalf("configured endpoint should yield *HTTPEmbedder")
	}
	if _, ok := NewEmbedder(Config{}).(HashingEmbedder); !ok {
		t.Fatalf("no endpoint should yield HashingEmbedder")
	}
}
