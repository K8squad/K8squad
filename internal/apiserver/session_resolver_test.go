package apiserver

import (
	"context"
	"testing"
)

// The DB-backed resolution paths (live / expired / revoked / miss) run against real Postgres in
// session_resolver_integration_test.go (build-tag gated). These unit cases pin the fail-closed guards
// that must hold WITHOUT a database — the ones a nil/empty input hits before any SQL — since a resolver
// that reaches for a nil *sql.DB would panic instead of denying.

func TestPostgresSessionResolverFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		resolver *PostgresSessionResolver
		token    string
	}{
		{"nil-receiver", nil, "anything"},
		{"nil-db", NewPostgresSessionResolver(nil), "anything"},
		{"empty-token", NewPostgresSessionResolver(nil), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth, err := c.resolver.Resolve(context.Background(), c.token)
			if err != ErrNoSession {
				t.Fatalf("Resolve(%q) err = %v, want ErrNoSession", c.token, err)
			}
			if auth.Principal != "" {
				t.Fatalf("Resolve(%q) leaked a principal %q on a fail-closed path", c.token, auth.Principal)
			}
		})
	}
}

// The production resolver must satisfy the SessionResolver seam (compile-time contract).
func TestPostgresSessionResolverImplementsSeam(t *testing.T) {
	var _ SessionResolver = (*PostgresSessionResolver)(nil)
	var _ SessionResolver = NewPostgresSessionResolver(nil)
}
