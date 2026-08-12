// §6.7.1 A5 · password reset — local-cred variant (primary, ADR-033).
//
// Ref: docs/bmad/05-testing-strategy.md §6.7.0/§6.7.1 (r3), ADR-033 / §12.3 in
// 03-architecture.md. Framework: Go testing + testify against the apiserver auth
// middleware (Playwright login leg lives in console/e2e/auth/a5-password-reset.spec.ts).
//
// Pass condition (§6.7.1 A5): reset token single-use + time-boxed; on reset ALL
// existing sessions invalidated (old HttpOnly cookie -> 401); reset responses
// identical whether or not the account exists (no user-enumeration); new password
// argon2id-hashed with nothing password-shaped in any log/span (NFR-SEC3); next
// login (A1) succeeds with the same principal identity.
package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	a5User    = "amelia"
	a5OldPass = "old-Correct-Horse-Battery-1!"
	a5NewPass = "new-Tr0ub4dor-&3-reset!"
	// resetTTL is the fixture's configured time-box. The assertion is on the
	// boundary behaviour (valid before, rejected after), not the exact value.
	resetTTL = 15 * time.Minute
)

// TestA5_LocalCredPasswordReset is the primary A5 case. Each sub-test isolates
// one clause of the pass condition and drives it through the seam contract.
func TestA5_LocalCredPasswordReset(t *testing.T) {
	t.Run("reset token is single-use", func(t *testing.T) {
		b := resolveBackend(t)
		b.SeedUser(t, a5User, a5OldPass)

		token := b.RequestReset(a5User).Token
		require.NotEmpty(t, token, "existing account must yield a usable reset token")

		require.Equal(t, 200, b.PerformReset(token, a5NewPass), "first use of a fresh token must succeed")
		assert.NotEqual(t, 200, b.PerformReset(token, "another-Pass-9!"),
			"a spent token must be rejected — single-use (no replay)")
	})

	t.Run("reset token is time-boxed", func(t *testing.T) {
		b := resolveBackend(t)
		b.SeedUser(t, a5User, a5OldPass)

		token := b.RequestReset(a5User).Token
		require.NotEmpty(t, token)

		b.Advance(resetTTL + time.Minute) // cross the time-box deterministically
		assert.NotEqual(t, 200, b.PerformReset(token, a5NewPass),
			"an expired token must be rejected — time-boxed")
	})

	t.Run("all sessions invalidated on reset (old cookie -> 401)", func(t *testing.T) {
		b := resolveBackend(t)
		pid := b.SeedUser(t, a5User, a5OldPass)

		// Two concurrent live sessions for the same principal.
		c1, s1 := b.Login(a5User, a5OldPass)
		require.Equal(t, 200, s1)
		c2, s2 := b.Login(a5User, a5OldPass)
		require.Equal(t, 200, s2)
		require.NotEqual(t, c1, c2, "distinct logins must mint distinct session cookies")

		if got, st := b.WhoAmI(c1); assert.Equal(t, 200, st) {
			assert.Equal(t, pid, got)
		}

		token := b.RequestReset(a5User).Token
		require.NotEmpty(t, token)
		require.Equal(t, 200, b.PerformReset(token, a5NewPass))

		// EVERY pre-reset session must now be dead.
		_, st1 := b.WhoAmI(c1)
		_, st2 := b.WhoAmI(c2)
		assert.Equal(t, 401, st1, "session #1 must be invalidated by the reset")
		assert.Equal(t, 401, st2, "session #2 must be invalidated by the reset")
	})

	t.Run("no user-enumeration: response identical for existing vs missing account", func(t *testing.T) {
		b := resolveBackend(t)
		b.SeedUser(t, a5User, a5OldPass)

		existing := b.RequestReset(a5User)
		missing := b.RequestReset("nobody-here-9c2f")

		assert.Equal(t, existing.Status, missing.Status,
			"status must not distinguish an existing account from a missing one")
		assert.Equal(t, existing.Body, missing.Body,
			"body must be byte-identical — no enumeration oracle")
		assert.Empty(t, missing.Token,
			"a missing account must not yield a reset token (timing/side-channel oracle)")
	})

	t.Run("new password is argon2id-hashed, plaintext never stored", func(t *testing.T) {
		b := resolveBackend(t)
		pid := b.SeedUser(t, a5User, a5OldPass)

		token := b.RequestReset(a5User).Token
		require.NotEmpty(t, token)
		require.Equal(t, 200, b.PerformReset(token, a5NewPass))

		hash := b.PasswordHash(pid)
		assert.True(t, strings.HasPrefix(hash, "$argon2id$"),
			"stored credential must be argon2id (PHC) encoded, got %q", firstRune(hash, 12))
		assert.NotContains(t, hash, a5NewPass, "plaintext must never be stored")
	})

	t.Run("nothing password-shaped hits any log/span (NFR-SEC3)", func(t *testing.T) {
		b := resolveBackend(t)
		b.SeedUser(t, a5User, a5OldPass)

		token := b.RequestReset(a5User).Token
		require.NotEmpty(t, token)
		require.Equal(t, 200, b.PerformReset(token, a5NewPass))
		_, _ = b.Login(a5User, a5NewPass)

		secrets := []string{a5OldPass, a5NewPass}
		for _, line := range b.Logs() {
			for _, secret := range secrets {
				assert.NotContains(t, line, secret,
					"NFR-SEC3: no password-shaped value may appear in a log/span; leaked line: %q", line)
			}
		}
	})

	t.Run("next login (A1) succeeds with the same principal identity", func(t *testing.T) {
		b := resolveBackend(t)
		pid := b.SeedUser(t, a5User, a5OldPass)

		token := b.RequestReset(a5User).Token
		require.NotEmpty(t, token)
		require.Equal(t, 200, b.PerformReset(token, a5NewPass))

		// Old credential no longer authenticates...
		_, oldStatus := b.Login(a5User, a5OldPass)
		assert.Equal(t, 401, oldStatus, "the old password must not authenticate after reset")

		// ...new credential logs in as the SAME principal (identity continuity).
		cookie, status := b.Login(a5User, a5NewPass)
		require.Equal(t, 200, status, "A1 login with the new password must succeed")
		got, who := b.WhoAmI(cookie)
		require.Equal(t, 200, who)
		assert.Equal(t, pid, got, "reset must preserve principal identity — same subject after reset")
	})
}

// TestA5OIDC_PasswordReset stays skipped-with-reason: the IdP variant is the
// opt-in seam case and does not activate until the AuthProvider OIDC seam is
// wired (§6.7.1 A5-oidc, ADR-033). Present as scaffolding so it is never
// silently dropped.
func TestA5OIDC_PasswordReset(t *testing.T) {
	t.Skip("§6.7.1 A5-oidc (IdP variant): skipped-with-reason until the AuthProvider OIDC seam is wired " +
		"(ADR-033). When enabled: KSquad redirects to the IdP, never stores/handles the password, nothing " +
		"password-shaped hits any log, and the next login (A1) succeeds with the same principal identity.")
}

// firstRune returns up to n leading bytes of s for a readable failure message
// without dumping a full hash.
func firstRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
