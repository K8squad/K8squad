package memory

import (
	"strings"
	"testing"
)

// good returns a live column→type map that matches every pinned expectation.
func good() map[string]string {
	m := make(map[string]string, len(expectedColumnTypes))
	for col, typ := range expectedColumnTypes {
		m[col] = typ
	}
	return m
}

func TestAssertColumnTypes_Match(t *testing.T) {
	if err := assertColumnTypes(good()); err != nil {
		t.Fatalf("matching schema must pass, got: %v", err)
	}
}

// The ISI-5109 regression: project_id retyped to text under a reader whose code still casts ::uuid.
func TestAssertColumnTypes_SkewFailsClosed(t *testing.T) {
	live := good()
	live["project_id"] = "uuid" // stale schema the pre-0004 reader would still expect
	err := assertColumnTypes(live)
	if err == nil {
		t.Fatal("project_id type skew must fail closed (ISI-5109), got nil")
	}
	if !strings.Contains(err.Error(), "project_id") {
		t.Fatalf("error must name the offending column, got: %v", err)
	}
}

func TestAssertColumnTypes_MissingColumnFailsClosed(t *testing.T) {
	live := good()
	delete(live, "squad_id")
	if err := assertColumnTypes(live); err == nil {
		t.Fatal("a missing pinned column must fail closed, got nil")
	}
}

func TestEmbeddedMigrationHead(t *testing.T) {
	head, err := embeddedMigrationHead()
	if err != nil {
		t.Fatalf("embeddedMigrationHead: %v", err)
	}
	if !strings.HasPrefix(head, "migrations/") || !strings.HasSuffix(head, ".sql") {
		t.Fatalf("unexpected head %q", head)
	}
	// HEAD must be the lexical max of the embedded set — the newest schema this binary carries.
	if head < "migrations/0001_memory.sql" {
		t.Fatalf("head %q sorts before the base migration", head)
	}
}
