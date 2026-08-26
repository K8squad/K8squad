package runsource

import (
	"context"
	"testing"

	"github.com/K8squad/K8squad/internal/buildbrowser"
)

// TestNewPostgresRunSource_NilDB — the constructor fails closed on a nil db rather
// than handing back a source that panics on first Lookup.
func TestNewPostgresRunSource_NilDB(t *testing.T) {
	if _, err := NewPostgresRunSource(nil); err == nil {
		t.Fatalf("NewPostgresRunSource(nil) = nil error, want a guard error")
	}
}

// TestPostgresRunSource_NonUUIDShortCircuit — a non-uuid Run id can never key a
// coord row, so Lookup answers found=false BEFORE touching Postgres (whose
// $1::uuid cast would otherwise turn the caller's junk id into a 500 instead of
// the 404 the route owes). It must not dereference the db on this path, so the
// short-circuit holds even with a nil *sql.DB.
func TestPostgresRunSource_NonUUIDShortCircuit(t *testing.T) {
	// db is deliberately nil: the short-circuit must return before any query, so a
	// dereference here would panic the test rather than pass it.
	s := &PostgresRunSource{db: nil, lookup: "unused on this path"}
	for _, id := range []string{"", "dev-run", "not-a-uuid", "123", "KSQUAD_DEV_RUNS"} {
		meta, found, err := s.Lookup(context.Background(), id)
		if err != nil {
			t.Fatalf("Lookup(%q) err = %v, want nil", id, err)
		}
		if found {
			t.Fatalf("Lookup(%q) found = true, want false (non-uuid can never key a coord row)", id)
		}
		if meta != (buildbrowser.RunMeta{}) {
			t.Fatalf("Lookup(%q) meta = %+v, want zero RunMeta", id, meta)
		}
	}
}

// TestValidUUID — the pre-query gate accepts a canonical uuid and rejects everything
// else, so Lookup only reaches the ::uuid-cast query with an id that can match.
func TestValidUUID(t *testing.T) {
	valid := []string{
		"11111111-1111-4111-8111-111111111111",
		"00000000-0000-0000-0000-000000000000",
	}
	for _, s := range valid {
		if !validUUID(s) {
			t.Fatalf("validUUID(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "dev-run", "1111", "11111111-1111-4111-8111", "zzzzzzzz-1111-4111-8111-111111111111"}
	for _, s := range invalid {
		if validUUID(s) {
			t.Fatalf("validUUID(%q) = true, want false", s)
		}
	}
}

// TestPostgresRunSource_ImplementsRunSource pins the production binding to the
// buildbrowser.RunSource seam the apiserver injects — a signature drift on either
// side goes RED here rather than at wiring time in cmd/apiserver.
func TestPostgresRunSource_ImplementsRunSource(t *testing.T) {
	var _ buildbrowser.RunSource = (*PostgresRunSource)(nil)
}
