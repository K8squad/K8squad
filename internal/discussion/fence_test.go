// Package discussion — THE DISCUSSION-ROOM FENCE, as a single runnable guarantee.
//
// ┌───────────────────────────────────────────────────────────────────────────────────────────┐
// │ ADR-0019 §"The fence must be *tested*, not asserted" + Enforcement matrix                    │
// │ PRD R13 (custody-erosion risk, rated High) · NFR-SEC6 / F6 · PRD §13 CEO-gate ratification   │
// │ Epic 10 Story 10.4 (epics.md:290) · ISI-4009 (consolidates ISI-3998 J-B)                     │
// └───────────────────────────────────────────────────────────────────────────────────────────┘
//
// THE FENCE: the discussion room is a COLLABORATION SURFACE, not a COORDINATION RECORD.
// "Conversation, not custody." Custody of a work item moves ONLY in the fenced `coord` claim
// tables (§6.2/§6.3) — never through a discussion write.
//
// Before this suite the fence was backed by *scattered* facts that no single file *was*: the
// no-custody schema (db/migrations/0004_discussion_schema.sql), the append-only triggers, the AC3
// integration test TestServerStampsAuthorIgnoringForgedBody, the AC5 TestCrossTeamReadsAreEmpty,
// and the console test/discussion/no-coordination.test.ts. A future refactor could erode the fence
// without a dedicated red test. This file is the F6-style blast-radius suite the CEO-gate (§13) can
// point to: ONE named runnable authority. Grep `TestDiscussionFence` → here.
//
// THE THREE INVARIANTS THIS SUITE PROVES (ADR-0019 Enforcement matrix + R13):
//  1. Coordination-free by construction — the discussion schema exposes NO claim/lease/fence/
//     state/holder/assignee/status/custody/owner column and no custody-transfer path. Proven by an
//     ALLOW-LIST over the exact columns, so any FUTURE custody column fails the test by default
//     (machine form of ADR AC4). See TestDiscussionFenceSchemaHasNoCustodyColumn (this file,
//     tag-free — runs in the unit lane, no Postgres) and its live-schema companion
//     TestDiscussionFenceSchemaIntrospection (fence_integration_test.go).
//  2. A message write moves no coordination state — opening/posting/replying/retracting leaves
//     every coord custody row byte-identical. See TestDiscussionFenceWriteMovesNoCoordState.
//  3. Provenance is always server-stamped — forged body author_* never lands; an unattributed or
//     unscoped row is un-representable (NOT NULL); retract is the only mutation. See
//     TestDiscussionFenceProvenanceServerStamped + TestDiscussionFenceUnattributedRowUnrepresentable.
//     Plus cross-tenant reads empty, 404-not-403: TestDiscussionFenceCrossTenantReadsEmpty.
//
// LAYOUT (AC7 wants one discoverable authority; the build-tag split is mechanical):
//   - fence_test.go              (THIS FILE, tag-free)  — the pure-structural AC1 allow-list, so the
//     load-bearing "no custody column" fact is a
//     test the default unit lane always runs.
//   - fence_integration_test.go  (//go:build discussion_integration) — the DB-backed AC1 (live
//     introspection), AC2, AC3, AC4, AC5.
//     Both files contribute TestDiscussionFence* functions; this file's doc comment is the authority.
//
// AC6 — CONSOLE LOCKSTEP. The client face of this fence is asserted by
// console/test/discussion/no-coordination.test.ts, which statically proves the discussion component
// surface exposes NO claim/checkout/assign/transition/handoff affordance. That test and this suite
// are the two halves of the same guarantee and must never drift: if you change one, revisit the
// other. (The console test guards the UI cannot *offer* custody; this Go suite guards the schema and
// write path cannot *record* it.)
//
// GUARDRAIL DISCIPLINE: this is boring to keep green and loud to break. NO production code change is
// permitted to make an assertion pass — if one cannot pass, the fence is already broken and that is a
// bug to FILE (R13), not to paper over here.
package discussion

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// allowedThreadColumns / allowedMessageColumns are the EXACT column sets the fence permits. This is
// an allow-list, not a deny-list: any column added to discussion.thread/message that is not listed
// here fails the test by default — the machine form of ADR-0019 AC4 ("the schema cannot become a
// coordination record"). Update these ONLY together with a deliberate schema change that a reviewer
// has confirmed does not introduce a custody/coordination column.
var (
	allowedThreadColumns = []string{
		"id", "project_id", "team_id", "title", "created_by", "created_at",
	}
	allowedMessageColumns = []string{
		"id", "thread_id", "parent_id", "author_principal", "author_agent_id",
		"author_run_id", "body", "created_at", "invalidated_at",
	}
	// forbiddenCustodyTokens are substrings that would signal a custody/coordination affordance
	// creeping into the discussion schema. Case-insensitive substring guard (belt-and-suspenders
	// alongside the allow-list): even a renamed variant like `work_state` or `claim_token` trips it.
	forbiddenCustodyTokens = []string{
		"claim", "lease", "fence", "fence_token", "state", "holder",
		"assignee", "status", "custody", "owner",
	}
)

// TestDiscussionFenceSchemaHasNoCustodyColumn is AC1 in its pure-structural form: it reads the
// SHIPPED migration (not a live DB) so the load-bearing "no custody column" invariant runs in the
// default unit lane with no Postgres. It parses the CREATE TABLE blocks for discussion.thread and
// discussion.message and asserts (a) the column set is EXACTLY the allow-list and (b) no column name
// contains a forbidden custody token. A future `ALTER/CREATE` adding e.g. `status text` or
// `holder_principal text` turns this RED — which is the whole point (R13, ADR AC4).
func TestDiscussionFenceSchemaHasNoCustodyColumn(t *testing.T) {
	sqlText := readDiscussionMigration(t)

	cases := []struct {
		table   string
		allowed []string
	}{
		{"discussion.thread", allowedThreadColumns},
		{"discussion.message", allowedMessageColumns},
	}
	for _, c := range cases {
		got := parseCreateTableColumns(t, sqlText, c.table)
		assertColumnAllowList(t, c.table, got, c.allowed)
		for _, col := range got {
			assertNoForbiddenToken(t, c.table, col)
		}
	}
}

// readDiscussionMigration loads db/migrations/0004_discussion_schema.sql relative to the package dir
// (mirrors the candidate list used by the integration lane's applyMigration).
func readDiscussionMigration(t *testing.T) string {
	t.Helper()
	candidates := []string{
		filepath.Join("..", "..", "db", "migrations", "0004_discussion_schema.sql"),
		filepath.Join("db", "migrations", "0004_discussion_schema.sql"),
	}
	if d := os.Getenv("DISCUSSION_MIGRATIONS_DIR"); d != "" {
		candidates = append([]string{filepath.Join(d, "0004_discussion_schema.sql")}, candidates...)
	}
	for _, c := range candidates {
		if b, err := os.ReadFile(c); err == nil {
			return string(b)
		}
	}
	t.Fatalf("could not locate 0004_discussion_schema.sql (tried %v); set DISCUSSION_MIGRATIONS_DIR", candidates)
	return ""
}

// parseCreateTableColumns extracts the declared column names from the `CREATE TABLE <table> ( ... );`
// block. It reads the lines between the opening paren and the terminating `);`, taking the first
// token of each column line and skipping table constraints (CONSTRAINT/CHECK/PRIMARY/FOREIGN/UNIQUE)
// and comment/blank lines. Deliberately simple so it stays a trustworthy guardrail, not a SQL parser.
func parseCreateTableColumns(t *testing.T, sqlText, table string) []string {
	t.Helper()
	marker := "CREATE TABLE " + table + " ("
	start := strings.Index(sqlText, marker)
	if start < 0 {
		t.Fatalf("could not find %q in the shipped migration — did the table get renamed?", marker)
	}
	body := sqlText[start+len(marker):]
	// The block ends at the first line that begins the closing `);`.
	end := strings.Index(body, "\n);")
	if end < 0 {
		t.Fatalf("could not find the closing `);` for %s", table)
	}
	block := body[:end]

	skip := map[string]bool{
		"CONSTRAINT": true, "CHECK": true, "PRIMARY": true, "FOREIGN": true, "UNIQUE": true,
	}
	var cols []string
	for _, raw := range strings.Split(block, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		tok := fields[0]
		if skip[strings.ToUpper(tok)] {
			continue
		}
		cols = append(cols, strings.ToLower(tok))
	}
	if len(cols) == 0 {
		t.Fatalf("parsed zero columns for %s — parser or schema shape changed", table)
	}
	return cols
}

// assertColumnAllowList fails unless got == allowed as sets (order-independent), listing the exact
// unexpected/missing columns so a fence breach reads clearly.
func assertColumnAllowList(t *testing.T, table string, got, allowed []string) {
	t.Helper()
	allow := make(map[string]bool, len(allowed))
	for _, c := range allowed {
		allow[c] = true
	}
	seen := make(map[string]bool, len(got))
	var unexpected []string
	for _, c := range got {
		seen[c] = true
		if !allow[c] {
			unexpected = append(unexpected, c)
		}
	}
	var missing []string
	for _, c := range allowed {
		if !seen[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(missing)
	if len(unexpected) > 0 {
		t.Errorf("AC1 FENCE BREACH: %s has UNEXPECTED column(s) %v — not on the fence allow-list %v. "+
			"If this is a deliberate, custody-free column, add it to the allow-list *and* have a "+
			"reviewer confirm it moves no coordination state (ADR-0019 AC4, R13).", table, unexpected, allowed)
	}
	if len(missing) > 0 {
		t.Errorf("AC1: %s is MISSING expected column(s) %v — the shipped schema drifted from the allow-list %v.",
			table, missing, allowed)
	}
}

// assertNoForbiddenToken fails if a column name contains any custody-signalling substring.
func assertNoForbiddenToken(t *testing.T, table, col string) {
	t.Helper()
	lc := strings.ToLower(col)
	for _, tok := range forbiddenCustodyTokens {
		if strings.Contains(lc, tok) {
			t.Errorf("AC1 FENCE BREACH: %s column %q contains forbidden custody token %q — the "+
				"discussion schema must not carry coordination state (ADR-0019 §fence, R13).", table, col, tok)
		}
	}
}
