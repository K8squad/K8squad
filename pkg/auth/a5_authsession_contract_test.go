// §6.7.1 A5 — local-cred auth-session test contract (source scaffolding).
//
// ADR-033 / §12.3 pinned the console authN mechanism to a LOCAL username/password
// store (Postgres `auth` schema, argon2id) as the v1 default, with OIDC/SSO an
// opt-in fast-follow behind the AuthProvider seam. That decision DROPPED the
// §6.7.1 `skip` and made A5 the local-cred PRIMARY case (see
// docs/bmad/05-testing-strategy.md §6.7.0/§6.7.1 r3, ISI-2311).
//
// This file is mechanism-concrete scaffolding: it pins the behavioural contract
// the A5 flow must satisfy so the assertions in a5_password_reset_test.go are
// fully written NOW. It carries no dependency on a real implementation — the
// suite compiles standalone and SKIPS-WITH-REASON until Epic 15's auth source
// (pkg/auth / auth service, §12.3) lands and binds a live backend via
// SetLiveAuthBackend (see the seam note at the bottom of this file). This is the
// §13 source-scaffolding discipline: scaffolded now, activates green when source
// lands, never silently dropped.
package auth

import (
	"testing"
	"time"
)

// resetResponse is the caller-visible result of a reset request. The no-user-
// enumeration property (§6.7.1 A5) is asserted over Status+Body: they must be
// byte-identical whether or not the account exists. Token is a TEST-ONLY channel
// the fixture uses to hand back the token that would otherwise reach the user
// out-of-band (email/console); it is NOT part of the HTTP response body and must
// never appear in Body.
type resetResponse struct {
	Status int
	Body   string
	// Token is populated by the fixture only when the account exists; empty
	// otherwise. Delivered out-of-band in production — present here purely so the
	// test can drive PerformReset. A non-empty Token for a non-existent account
	// would itself be an enumeration oracle and is asserted against.
	Token string
}

// authBackend is the seam the A5 suite drives. Epic 15 binds a live
// implementation (real apiserver auth middleware + throwaway argon2id-seeded
// `auth`-schema fixture, no external IdP) via SetLiveAuthBackend. Every method
// models one observable behaviour named in the §6.7.1 A5 pass condition.
type authBackend interface {
	// SeedUser provisions a local-cred user (argon2id-hashed) and returns its
	// stable principal id. Used to prove A1 identity continuity across a reset.
	SeedUser(t *testing.T, username, password string) (principalID string)

	// Login performs the A1 leg. On success it returns the opaque HttpOnly
	// session cookie value (§12.3) and status 200; on failure a non-200 and an
	// empty cookie. It must never echo anything password-shaped.
	Login(username, password string) (cookie string, status int)

	// WhoAmI resolves a session cookie to its principal. A live session returns
	// (principalID, 200); an invalidated/expired/unknown cookie returns ("", 401).
	// This is the probe for "old cookie -> 401" after a reset.
	WhoAmI(cookie string) (principalID string, status int)

	// RequestReset triggers the "reset/recover" flow for a username. The response
	// MUST be identical (Status+Body) for existing and non-existing accounts.
	RequestReset(username string) resetResponse

	// PerformReset consumes a reset token and sets a new argon2id password. A
	// valid, unexpired, unused token returns 200; a spent/expired/forged token
	// returns a non-200 (typically 400/401) and makes no change.
	PerformReset(token, newPassword string) (status int)

	// PasswordHash returns the stored credential material for a principal so the
	// test can assert it is argon2id-encoded (PHC "$argon2id$..."), never the
	// plaintext.
	PasswordHash(principalID string) string

	// Logs returns everything the backend wrote to any log/span sink during the
	// flow, so the suite can assert NFR-SEC3: nothing password-shaped leaked.
	Logs() []string

	// Advance moves the backend's clock forward, so the single-use + time-boxed
	// token property is provable deterministically (no wall-clock sleep — matches
	// the §4.1 determinism guard and the bg-job/heartbeat constraints).
	Advance(d time.Duration)
}

// backendFactory builds a fresh, isolated backend per test (throwaway schema).
type backendFactory func(t *testing.T) authBackend

// liveAuthBackend is the activation seam. It is nil in the skeleton phase, so
// resolveBackend skips-with-reason. Epic 15 calls SetLiveAuthBackend from an
// init() in its own (source-gated) test file to flip the suite live:
//
//	// pkg/auth/a5_livebackend_test.go  (lands with Epic 15 auth source)
//	func init() { SetLiveAuthBackend(func(t *testing.T) authBackend { return newLiveHarness(t) }) }
//
// Runtime-skip (not a build tag) is deliberate: the case stays VISIBLE as
// `--- SKIP` with a reason in `go test` output — scaffolded, never silently
// dropped (§13).
var liveAuthBackend backendFactory

// SetLiveAuthBackend wires the real auth backend for the A5 suite. Exported for
// the Epic-15 live-binding file to call; unused in the skeleton phase.
func SetLiveAuthBackend(f backendFactory) { liveAuthBackend = f }

const a5PendingReason = "§6.7.1 A5 local-cred auth-session: pending Epic 15 auth source " +
	"(pkg/auth / auth service, §12.3, ADR-033). Mechanism-concrete scaffolding — " +
	"activates green once SetLiveAuthBackend binds the live argon2id `auth`-schema fixture (ISI-2311)."

// resolveBackend returns a fresh live backend or skips the calling test with a
// first-class reason when the auth source has not landed yet.
func resolveBackend(t *testing.T) authBackend {
	t.Helper()
	if liveAuthBackend == nil {
		t.Skip(a5PendingReason)
	}
	return liveAuthBackend(t)
}
