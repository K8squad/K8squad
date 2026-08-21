package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// The short-lived internal JWT (15.1): minted at login/refresh beside the opaque
// edge session, for BFF→apiserver and Run-identity propagation. HS256 with an
// IN-PROCESS key (ADR-033): no external JWKS round-trip, no key distribution —
// the key never leaves the apiserver that minted it. Default TTL 1h.
// ============================================================================

// Claims is the internal JWT payload. Subject is the STABLE principal (matches
// AuthorContext.Principal / author_principal stamps); role is the bounded two-value
// global role (admin|user) the BFF uses for adaptive-nav hints (8.16) — it is NEVER
// an authorization decision by itself (the server re-resolves the session per call).
type Claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	UserID    string `json:"uid"`
	TeamID    string `json:"tid,omitempty"`
	Role      string `json:"role,omitempty"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	SessionID string `json:"sid,omitempty"`
}

// JWTIssuer issues HS256 mint/Verify pair.
type JWTIssuer struct {
	key []byte
	iss string
	ttl time.Duration
	now func() time.Time
}

// ErrInvalidToken is the single, opaque failure for a token that does not verify
// (bad signature, expired, malformed). No reason is surfaced to the caller.
var ErrInvalidToken = errors.New("auth: invalid token")

// NewJWTIssuer builds the issuer. key must be at least 32 bytes (HS256 security
// floor); ttl <= 0 defaults to 1h. A zero key is rejected — callers generate one
// at startup (cmd/apiserver) and log the auto-generation warning.
func NewJWTIssuer(key []byte, ttl time.Duration) (*JWTIssuer, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("auth: jwt signing key must be >= 32 bytes, got %d", len(key))
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &JWTIssuer{key: key, iss: "ksquad-apiserver", ttl: ttl, now: time.Now}, nil
}

// TTL reports the configured token lifetime (for the login response's expiresIn).
func (j *JWTIssuer) TTL() time.Duration { return j.ttl }

// Mint signs the claims into a compact HS256 JWT.
func (j *JWTIssuer) Mint(c Claims) (string, error) {
	now := j.now()
	c.Issuer = j.iss
	c.IssuedAt = now.Unix()
	c.ExpiresAt = now.Add(j.ttl).Unix()

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("auth: marshal claims: %w", err)
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, j.key)
	mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify checks signature + expiry + issuer and returns the claims. Any failure
// (tampered payload, wrong key, expired, foreign issuer) is the one ErrInvalidToken.
func (j *JWTIssuer) Verify(token string) (Claims, error) {
	var c Claims
	last := strings.LastIndexByte(token, '.')
	if last <= 0 {
		return c, ErrInvalidToken
	}
	signing, sigB64 := token[:last], token[last+1:]
	first := strings.IndexByte(signing, '.')
	if first <= 0 {
		return c, ErrInvalidToken
	}

	// Reject alg confusion up front: the header must pin HS256.
	hdr, err := base64.RawURLEncoding.DecodeString(signing[:first])
	if err != nil || string(hdr) != `{"alg":"HS256","typ":"JWT"}` {
		return c, ErrInvalidToken
	}

	mac := hmac.New(sha256.New, j.key)
	mac.Write([]byte(signing))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !hmac.Equal(want, got) {
		return c, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(signing[first+1:])
	if err != nil {
		return c, ErrInvalidToken
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, ErrInvalidToken
	}
	if c.Issuer != j.iss || j.now().Unix() >= c.ExpiresAt {
		return c, ErrInvalidToken
	}
	return c, nil
}

// GenerateSigningKey mints a fresh 32-byte HS256 key (base64-encoded for env/config
// transport). Used by cmd/apiserver when no durable key is configured — auto-generated
// keys mean sessions survive only until the pod restarts (Helm 9.5 supplies the durable one).
func GenerateSigningKey() string {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(fmt.Sprintf("auth: read signing key entropy: %v", err))
	}
	return base64.RawStdEncoding.EncodeToString(k)
}
