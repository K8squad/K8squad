package auth

import (
	"crypto/sha256"
	"testing"
)

// TestMintTokenHashMatchesLookup is the DB-free guard for ISI-3541: the token_hash
// mintToken persists MUST equal sha256 of the emitted token string, because every
// session lookup (PostgresSessionStore.Resolve/Rotate/Logout and the §13
// PostgresSessionResolver) computes sha256([]byte(token)) over the ksquad_session
// cookie value. If these ever diverge again, login "succeeds" but every gated route
// 401s — the exact regression this asserts against.
func TestMintTokenHashMatchesLookup(t *testing.T) {
	token, hash, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	if token == "" {
		t.Fatal("mintToken returned an empty token")
	}
	want := sha256.Sum256([]byte(token)) // exactly what every lookup hashes
	if len(hash) != len(want) {
		t.Fatalf("hash length %d, want %d", len(hash), len(want))
	}
	for i := range want {
		if hash[i] != want[i] {
			t.Fatalf("persisted token_hash != sha256(emitted token): a session minted here would never resolve (byte %d differs)", i)
		}
	}
}

// TestMintTokenUnique is a cheap sanity check that entropy is actually read per call.
func TestMintTokenUnique(t *testing.T) {
	a, _, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	b, _, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	if a == b {
		t.Fatal("mintToken returned identical tokens across calls")
	}
}
