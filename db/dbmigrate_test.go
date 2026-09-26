package dbmigrate

import (
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// versionRE matches the forward-only naming convention (db/migrations/README.md): NNNN_short_name.sql.
var versionRE = regexp.MustCompile(`^\d{4}_[a-z0-9_]+\.sql$`)

// TestSelectMigrationsExcludesTestCompanions is the guard that keeps the runner from ever executing a
// *_test.sql self-check as if it were a migration (README: the runner ignores them).
func TestSelectMigrationsExcludesTestCompanions(t *testing.T) {
	names, err := selectMigrations()
	if err != nil {
		t.Fatalf("selectMigrations: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("selectMigrations returned no migrations — embed is empty")
	}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.sql") {
			t.Errorf("%s is a *_test.sql self-check and must not be selected as a migration", n)
		}
		if !versionRE.MatchString(path.Base(n)) {
			t.Errorf("%s does not match the NNNN_name.sql convention", path.Base(n))
		}
	}

	// At least one *_test.sql exists on disk, so the filter is doing real work — a regression that
	// stopped filtering would still pass a "no companions" check on a set that happened to have none.
	all, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	companions := 0
	for _, a := range all {
		if strings.HasSuffix(a, "_test.sql") {
			companions++
		}
	}
	if companions == 0 {
		t.Fatal("expected embedded *_test.sql companions to prove the filter runs; found none")
	}
	if len(all)-companions != len(names) {
		t.Errorf("selectMigrations kept %d; expected %d non-test files", len(names), len(all)-companions)
	}
}

// TestSelectMigrationsSorted proves the runner applies in deterministic lexical order — the contract
// that makes "applied once, in order" (README) hold across the same-number files (0008_*, 0009_*,
// 0010_*, 0019_*).
func TestSelectMigrationsSorted(t *testing.T) {
	names, err := selectMigrations()
	if err != nil {
		t.Fatalf("selectMigrations: %v", err)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("migrations are not in lexical order: %v", names)
	}
}

// TestHeadIsHighestVersion pins the assumption Apply logs against: the last selected file is the
// numerically-latest migration (HEAD). Guards a future file mis-named so it sorts out of place.
func TestHeadIsHighestVersion(t *testing.T) {
	names, err := selectMigrations()
	if err != nil {
		t.Fatalf("selectMigrations: %v", err)
	}
	head := path.Base(names[len(names)-1])
	for _, n := range names {
		if path.Base(n) > head {
			t.Errorf("%s sorts after HEAD %s — HEAD detection is wrong", path.Base(n), head)
		}
	}
}
