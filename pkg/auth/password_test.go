package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashPassword_IsArgon2idPHC(t *testing.T) {
	h, err := HashPassword("correct-horse-battery-staple")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(h, "$argon2id$v=19$"), "must be an argon2id PHC string, got %q", h[:min(len(h), 24)])
	assert.NotContains(t, h, "correct-horse", "plaintext never embedded")
}

func TestVerifyPassword_RoundTrip(t *testing.T) {
	h, err := HashPassword("s3cret-password!")
	require.NoError(t, err)
	assert.NoError(t, VerifyPassword("s3cret-password!", h))
	assert.ErrorIs(t, VerifyPassword("wrong-password", h), ErrInvalidPassword)
}

func TestVerifyPassword_UniqueSalts(t *testing.T) {
	h1, _ := HashPassword("same-password")
	h2, _ := HashPassword("same-password")
	assert.NotEqual(t, h1, h2, "salts must be per-hash — no rainbow-table reuse")
}

func TestVerifyPassword_MalformedHashFailsClosed(t *testing.T) {
	for _, bad := range []string{"", "plaintext", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$a2V5", "$argon2id$v=19$m=0,t=0,p=0$$"} {
		assert.ErrorIs(t, VerifyPassword("x", bad), ErrInvalidPassword, "malformed hash %q must fail closed", bad)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
