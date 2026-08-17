package memory

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Embedder is the §7.1 embedder seam: text → a fixed-dimension vector. It sits behind an interface so
// an air-gapped cluster can swap the default local embedder for a remote endpoint (Config.Embedder*)
// without touching the store or the read tools. The ONLY hard contract is dimension: every vector it
// returns MUST be EmbeddingDim long, so a mismatch is a legible write-time error, never a silent
// truncation (mirrors PgVectorStore.Write's dimension check).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// HashingEmbedder is the deterministic, dependency-free default local embedder (§7.1). It feature-hashes
// tokens into a fixed EmbeddingDim space and L2-normalizes, so cosine distance clusters texts by shared
// vocabulary — enough for real ANN recall and fully reproducible in tests without a model server. It is
// intentionally simple: wiring a semantic model behind Config.EmbedderEndpoint is a fast-follow behind
// this same seam; nothing above the seam changes when it lands.
type HashingEmbedder struct {
	dim int
}

// NewHashingEmbedder returns the default local embedder at the configured EmbeddingDim.
func NewHashingEmbedder() HashingEmbedder { return HashingEmbedder{dim: EmbeddingDim} }

var _ Embedder = HashingEmbedder{}

// Embed hashes each token to a bucket and a sign (signed feature hashing, which keeps buckets roughly
// zero-mean), then L2-normalizes. An empty/all-zero result is nudged to a unit basis vector so pgvector
// never sees a zero vector (whose cosine distance is undefined).
func (e HashingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	dim := e.dim
	if dim <= 0 {
		dim = EmbeddingDim
	}
	v := make([]float32, dim)
	for _, tok := range tokenize(text) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum32()
		idx := int(sum % uint32(dim))
		if sum&1 == 0 {
			v[idx] += 1
		} else {
			v[idx] -= 1
		}
	}
	var norm float64
	for _, f := range v {
		norm += float64(f) * float64(f)
	}
	if norm == 0 {
		v[0] = 1 // never hand pgvector a zero vector (undefined cosine distance)
		return v, nil
	}
	inv := float32(1 / math.Sqrt(norm))
	for i := range v {
		v[i] *= inv
	}
	return v, nil
}

// tokenize lowercases and splits on any non-alphanumeric rune — a deliberately trivial, stable
// tokenizer so the same text always hashes to the same vector.
func tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
