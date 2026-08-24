package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestIssuer(t *testing.T) *JWTIssuer {
	t.Helper()
	iss, err := NewJWTIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)
	return iss
}

func TestJWT_RoundTrip(t *testing.T) {
	iss := newTestIssuer(t)
	tok, err := iss.Mint(Claims{Subject: "user:amelia", UserID: "11111111-1111-1111-1111-111111111111", Role: RoleAdmin})
	require.NoError(t, err)

	got, err := iss.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, "user:amelia", got.Subject)
	assert.Equal(t, RoleAdmin, got.Role)
	assert.Equal(t, "ksquad-apiserver", got.Issuer)
	assert.Equal(t, int64(3600), got.ExpiresAt-got.IssuedAt, "1h default TTL")
}

func TestJWT_TamperedPayloadRejected(t *testing.T) {
	iss := newTestIssuer(t)
	tok, err := iss.Mint(Claims{Subject: "user:amelia", Role: RoleUser})
	require.NoError(t, err)

	// Flip the role inside the payload, keep the signature. The payload is
	// base64 — decode, rewrite the role claim, re-encode (RawURLEncoding, no
	// padding, matching Mint), then re-attach the ORIGINAL signature.
	parts := strings.Split(tok, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	tampered := strings.Replace(string(raw), `"role":"user"`, `"role":"admin"`, 1)
	require.NotEqual(t, string(raw), tampered, "payload must actually change")
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(tampered)) + "." + parts[2]

	_, err = iss.Verify(forged)
	assert.ErrorIs(t, err, ErrInvalidToken, "privilege-escalation forgery must fail")
}

func TestJWT_ExpiredRejected(t *testing.T) {
	iss := newTestIssuer(t)
	base := time.Now()
	iss.now = func() time.Time { return base }
	tok, err := iss.Mint(Claims{Subject: "user:amelia"})
	require.NoError(t, err)

	iss.now = func() time.Time { return base.Add(2 * time.Hour) } // cross the TTL
	_, err = iss.Verify(tok)
	assert.ErrorIs(t, err, ErrInvalidToken, "expired token must fail")
}

func TestJWT_WrongKeyRejected(t *testing.T) {
	issA := newTestIssuer(t)
	issB, err := NewJWTIssuer([]byte("ffffffffffffffffffffffffffffffff"), time.Hour)
	require.NoError(t, err)

	tok, err := issA.Mint(Claims{Subject: "user:amelia"})
	require.NoError(t, err)
	_, err = issB.Verify(tok)
	assert.ErrorIs(t, err, ErrInvalidToken, "a token from another key must not verify")
}

func TestJWT_ShortKeyRejected(t *testing.T) {
	_, err := NewJWTIssuer([]byte("too-short"), time.Hour)
	assert.Error(t, err, "HS256 keys below 32 bytes must be refused")
}

func TestGenerateSigningKey_DistinctAndLong(t *testing.T) {
	a, b := GenerateSigningKey(), GenerateSigningKey()
	assert.NotEqual(t, a, b)
	assert.Len(t, a, 43, "32 bytes base64 RawStd = 43 chars")
}
