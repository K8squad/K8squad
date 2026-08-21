// Package auth is the Epic 15 identity seam inside the apiserver (ADR-033 / Arch §12.3,
// stories 15.1 + 15.2, ISI-2920).
//
// It owns the WRITE path of the local-cred store ISI-2758 landed the read path for:
// argon2id password hashing, opaque server-side session mint/rotate/revoke over auth.session,
// the short-lived internal JWT mint, per-IP login rate limiting, user CRUD + bootstrap admin
// over auth.user, and the 15.9 OIDC group→access mapping seam. It runs IN the apiserver —
// there is no separate ksquad-auth binary/Deployment (ADR-033 / §17.3).
//
// Discipline carried over from 0006/ISI-2758:
//   - only sha256(token) ever touches the database — the plaintext bearer token is never persisted;
//   - fail-closed everywhere: any doubt (unknown user, spent password, revoked/expired session,
//     deactivated account) is an indistinguishable denial;
//   - no user-enumeration oracles: login/reset answer identically whether or not the account exists.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2 parameters (OWASP 2024 memory-hard recommendations for argon2id). They are fixed
// package constants, encoded into every PHC string, so Verify reads the parameters FROM the
// stored hash — existing credentials keep verifying across a future parameter bump.
const (
	argon2Time    = 3
	argon2Memory  = 64 * 1024 // 64 MiB
	argon2Threads = 4
	argon2KeyLen  = 32
	argon2SaltLen = 16
)

// hashGate bounds CONCURRENT argon2 derivations (PR #90 review finding 2): each
// derivation allocates argon2Memory (64 MiB), so an unauthenticated endpoint
// that runs argon2 per request (login, with the timing-equalization dummy) must
// not run unbounded numbers of them at once — the apiserver pod's memory limit
// would be exhausted by N ≈ limit/64MiB parallel requests (OOM kill). Callers
// queue on the gate; memory stays bounded, wait time grows instead. Default 2
// (128 MiB peak) fits the chart's 256Mi apiserver limit. SetHashConcurrency(0)
// disables gating (tests only).
type hashGate struct {
	mu    sync.Mutex
	cond  *sync.Cond
	cap   int
	inUse int
}

var gate = newHashGate(2)

func newHashGate(cap int) *hashGate {
	g := &hashGate{cap: cap}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// SetHashConcurrency sets the maximum number of simultaneous argon2 derivations.
// n <= 0 removes the bound. It must be called before any hashing starts (startup
// config / init of a test binary), not raced against in-flight logins.
func SetHashConcurrency(n int) {
	gate.mu.Lock()
	gate.cap = n
	gate.cond.Broadcast()
	gate.mu.Unlock()
}

// acquire blocks until a derivation slot is free (or immediately when the gate
// is disabled) and returns the release func.
func (g *hashGate) acquire() func() {
	g.mu.Lock()
	for g.cap > 0 && g.inUse >= g.cap {
		g.cond.Wait()
	}
	if g.cap > 0 {
		g.inUse++
	}
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		if g.cap > 0 {
			g.inUse--
		}
		g.cond.Signal()
		g.mu.Unlock()
	}
}

// ErrInvalidPassword is returned by VerifyPassword when the credential does not match. It is
// deliberately indistinguishable from "no such user" at the call site (login runs Verify against
// a dummy hash for unknown users so even the TIMING matches — see Service.Login).
var ErrInvalidPassword = errors.New("auth: invalid credentials")

// HashPassword derives the argon2id PHC string ("$argon2id$v=19$m=..,t=..,p=..$salt$hash")
// for a plaintext password. The salt is freshly random per call; the plaintext is never
// retained. The derivation rides the bounded-concurrency gate (see hashGate).
func HashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	release := gate.acquire()
	defer release()
	key := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	return phcEncode(salt, key), nil
}

// VerifyPassword checks a plaintext against a stored argon2id PHC string in constant time
// (subtle.ConstantTimeCompare over the derived key). A malformed stored hash fails closed with
// ErrInvalidPassword — a corrupt credential row must never authenticate.
func VerifyPassword(password, phc string) error {
	salt, wantKey, params, err := phcDecode(phc)
	if err != nil {
		return ErrInvalidPassword
	}
	release := gate.acquire()
	defer release()
	// Guard the int -> uint32 narrowing below: a decoded PHC key longer than
	// MaxUint32 bytes cannot occur for a real argon2id hash (32 bytes), and a
	// corrupt/absurd stored hash fails closed here instead of converting.
	keyLen := len(wantKey)
	if keyLen > math.MaxUint32 {
		return ErrInvalidPassword
	}
	gotKey := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(keyLen))
	if subtle.ConstantTimeCompare(gotKey, wantKey) != 1 {
		return ErrInvalidPassword
	}
	return nil
}

type argon2Params struct {
	time    uint32
	memory  uint32
	threads uint8
}

// phcEncode renders the PHC string format argon2 consumers share.
func phcEncode(salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// phcDecode parses a PHC string, rejecting anything that is not argon2id.
func phcDecode(phc string) ([]byte, []byte, argon2Params, error) {
	parts := strings.Split(phc, "$")
	// "" | "argon2id" | "v=19" | "m=..,t=..,p=.." | salt | key
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, argon2Params{}, fmt.Errorf("auth: not an argon2id PHC string")
	}
	var params argon2Params
	for _, kv := range strings.Split(parts[3], ",") {
		var v uint32
		if _, err := fmt.Sscanf(kv, "m=%d", &v); err == nil {
			params.memory = v
			continue
		}
		if _, err := fmt.Sscanf(kv, "t=%d", &v); err == nil {
			params.time = v
			continue
		}
		if _, err := fmt.Sscanf(kv, "p=%d", &v); err == nil {
			//nolint:gosec // p is a parallelism hint clamped to uint8 by the format itself
			params.threads = uint8(v)
		}
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, argon2Params{}, fmt.Errorf("auth: decode salt: %w", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, argon2Params{}, fmt.Errorf("auth: decode key: %w", err)
	}
	if params.memory == 0 || params.time == 0 || params.threads == 0 || len(salt) == 0 || len(key) == 0 {
		return nil, nil, argon2Params{}, fmt.Errorf("auth: incomplete argon2id parameters")
	}
	return salt, key, params, nil
}
