package taskio

import (
	"errors"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/auth"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestMintVerifyRoundTrip(t *testing.T) {
	m, err := NewMinter(testKey(), time.Hour)
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	tok, err := m.Mint("run-A", "wi-1", "backend-agent")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.RunID != "run-A" || got.WorkItemID != "wi-1" || got.Principal != "backend-agent" {
		t.Fatalf("binding mismatch: %+v", got)
	}
}

func TestMintRequiresBinding(t *testing.T) {
	m, _ := NewMinter(testKey(), time.Hour)
	if _, err := m.Mint("", "wi-1", "a"); err == nil {
		t.Fatal("expected error minting without runID")
	}
	if _, err := m.Mint("run-A", "", "a"); err == nil {
		t.Fatal("expected error minting without workItemID")
	}
}

// AC5: a console session JWT (auth issuer) must NOT verify as a run token even
// under the same signing key — audience separation by issuer string.
func TestSessionJWTIsNotARunToken(t *testing.T) {
	key := testKey()
	sessIss, _ := auth.NewJWTIssuer(key, time.Hour) // iss = ksquad-apiserver
	sessTok, err := sessIss.Mint(auth.Claims{Subject: "user", UserID: "u1"})
	if err != nil {
		t.Fatalf("session mint: %v", err)
	}
	m, _ := NewMinter(key, time.Hour) // iss = ksquad-taskio
	if _, err := m.Verify(sessTok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("session JWT verified as run token (want ErrInvalidToken), got %v", err)
	}
}

// AC5: a run token must not verify against the console session issuer either.
func TestRunTokenIsNotASessionJWT(t *testing.T) {
	key := testKey()
	m, _ := NewMinter(key, time.Hour)
	runTok, _ := m.Mint("run-A", "wi-1", "agent")
	sessIss, _ := auth.NewJWTIssuer(key, time.Hour)
	if _, err := sessIss.Verify(runTok); err == nil {
		t.Fatal("run token verified as a console session JWT")
	}
}

func TestVerifyExpired(t *testing.T) {
	m, _ := NewMinter(testKey(), time.Nanosecond)
	tok, _ := m.Mint("run-A", "wi-1", "a")
	time.Sleep(2 * time.Millisecond)
	if _, err := m.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for expired, got %v", err)
	}
}

func TestVerifyTamperedAndWrongKey(t *testing.T) {
	m, _ := NewMinter(testKey(), time.Hour)
	tok, _ := m.Mint("run-A", "wi-1", "a")

	other := make([]byte, 32)
	for i := range other {
		other[i] = 0xAA
	}
	m2, _ := NewMinter(other, time.Hour)
	if _, err := m2.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for wrong key, got %v", err)
	}
	if _, err := m.Verify(tok + "x"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for tampered, got %v", err)
	}
}

func TestAuthorizeOwnRunOnly(t *testing.T) {
	tok := RunToken{RunID: "run-A", WorkItemID: "wi-1", Principal: "a"}
	if err := tok.Authorize("run-A", "wi-1"); err != nil {
		t.Fatalf("own-run authorize should pass: %v", err)
	}
	if err := tok.Authorize("", ""); err != nil {
		t.Fatalf("empty want should pass: %v", err)
	}
	if err := tok.Authorize("run-B", ""); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("cross-run should be ErrScopeMismatch, got %v", err)
	}
	if err := tok.Authorize("", "wi-2"); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("cross-item should be ErrScopeMismatch, got %v", err)
	}
}
