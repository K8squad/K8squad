package memory

import (
	"testing"
)

// ISI-7286 (L5 green-up): the pgvector wire encoding (encodeVector) was the
// remaining zero-coverage function in internal/memory. Pin its contract so
// the ratchet floor stays earned by tests, not by lowering it.
//
// The config surface (DefaultConfig / LoadConfig / Config.Validate) is
// already pinned by config_test.go — do not re-declare those test names
// here; duplicate Test funcs in one package break the whole test build
// (that is exactly what turned PR #1010's lint/L1 matrix red).

func TestEncodeVector(t *testing.T) {
	cases := []struct {
		name string
		v    []float32
		want string
	}{
		{"empty", nil, "[]"},
		{"single", []float32{0}, "[0]"},
		{"negative and fraction", []float32{1.5, -2.25}, "[1.5,-2.25]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeVector(tc.v); got != tc.want {
				t.Fatalf("encodeVector(%v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}
