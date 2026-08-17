// FR-B3 enforcement — the coordination surface has NO agent-to-agent chat
// channel (Story 2.5 / ISI-2524; Arch §6.1, §8.4; PRD FR-B3).
//
// The v1 coordination model is HUB-MEDIATED BY DESIGN: agents never address
// each other. The ONLY sanctioned inter-agent surfaces are, per Arch §6.1:
//
//   - coord.comment  — append-only, provenanced notes on a work item
//   - work_item.state — the lifecycle transition itself
//
// A "handoff" is a comment plus a state change on a work item, and nothing
// else. Memory is NOT a handoff channel either — that half of the rule is
// enforced in Epic 6 (§6.5 provenance model); this file pins the coord-side
// half. db/migrations/0001_coord_schema.sql already documents the invariant
// ("I4/no-P2P: there is no `message` table"), but a header comment is not a
// guard. These contract tests make FR-B3 executable so the invariant survives
// future stories (apiserver routes, reconcilers, new migrations):
//
//  1. TestFRB3_CoordSurfaceAllowlist      — the pkg/coord exported surface is
//     pinned to the custody-only set; ANY new identifier fails until the
//     allowlist is consciously updated, and chat-shaped identifiers are
//     rejected outright (allowlist edits cannot smuggle a chat channel).
//  2. TestFRB3_NoChatTableInAnyMigration  — no migration may CREATE a
//     chat/DM/inbox/message-shaped table. "There is no message table" stops
//     being an accident of v1 and becomes a tested contract.
//  3. TestFRB3_SanctionedHandoffChannelPinned — the positive half: the
//     sanctioned channels (append-only coord.comment, work_item.state) are
//     structurally present, so FR-B3 pins WHAT handoff IS, not only what it
//     is not.
//  4. TestFRB3_SpineOpensNoDirectPeerSockets — the spine never opens or
//     dials a socket: no listener, no dialer, no embedded HTTP/gRPC server.
//     Mediation goes through the shared coordination store, never a direct
//     peer connection (§6 no-P2P / §8.4).
//
// The scans are deliberately STATIC (AST + SQL text): they run in the default
// `go test ./...` lane with no Postgres, so every PR — not just the chaos
// lane — re-proves FR-B3. When the apiserver coordination routes land
// (Epic 3), extend allowlistDirs / the route scan there in the same story.
package coord_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/K8squad/K8squad/pkg/coord"
)

// chatShapeRe matches identifiers / table names that would constitute (or
// read as) an agent-to-agent chat channel. Kept to unambiguous chat nouns —
// "notification" is intentionally NOT matched: notifying a HUMAN via the
// console (e.g. Story 2.12 approval gates) is a different, sanctioned
// channel and is not FR-B3's concern.
var chatShapeRe = regexp.MustCompile(`(?i)chat|message|conversation|inbox|\bdm\b`)

// allowedSurface is the pinned FR-B3 allowlist: every exported identifier of
// pkg/coord, each justified by the §6 mechanism it implements. Adding a
// legitimate custody operation means adding it HERE, in the same PR, with a
// §reference. Adding a chat channel means changing the architecture — not
// this list.
var allowedSurface = map[string]string{
	"Config":                    "table-name binding config (trusted, code-supplied)",
	"Coordinator":               "the coordinator type (stateless beyond db+cfg)",
	"New":                       "constructor",
	"NewForTest":                "chaos-harness constructor (int-keyed schema)",
	"Coordinator.DB":            "harness handle accessor",
	"Coordinator.ClaimNext":     "§6.2 SKIP-LOCKED pop + fence-bump acquire",
	"Coordinator.Acquire":       "§6.2 conditional fence-bump acquire of a specific item",
	"Coordinator.Renew":         "§6.2 lease heartbeat under holder+fence+live-lease guard",
	"Coordinator.Complete":      "§6.3 fenced state-mutating completion",
	"Coordinator.RedriveClaim":  "§6.4 idempotent reconcile re-entry",
	"Coordinator.DispatchOnce":  "§6.4 custody-gated dispatch idempotency marker",
	"Coordinator.ReclaimFenced": "§6.3 fence-BEFORE-release reclaim",

	// §6.2 production SKIP LOCKED claim surface (ISI-2523, story 2.2 —
	// merged to main via PR #27 while this branch was in review).
	"ProdConfig":                 "§6.2 prod claimer config (DSN/backoff), code-supplied",
	"DefaultProdConfig":          "§6.2 sane default prod claimer config",
	"ProdClaimer":                "§6.2 production single-claim SKIP LOCKED claimer type",
	"NewProdClaimer":             "§6.2 constructor",
	"ProdClaimer.ClaimNext":      "§6.2 single-claim under contention (SKIP LOCKED + fence)",
	"ProdClaimer.ClaimableCount": "§6.2 claimable backlog introspection for tests/ops",

	// §17.4 domain-event capture wiring (Story 12.1 / ISI-2260). Emit-only: the
	// option co-commits ONE append-only coord.outbox event in the claim txn. NOT
	// an agent-to-agent channel — events are one-way non-custodial projections
	// (§17.4 no-P2P guard, 12.4), nothing published re-enters coordination.
	"ProdClaimerOption": "§17.4 functional option for the prod claimer (event capture opt-in)",
	"WithOutboxCapture": "§17.4 co-commit a work_item/claimed outbox event in the claim txn (emit-only)",

	// §6.2 acquire-a-named-item surface (Story 2.2 / ISI-2523), the specific-item
	// sibling of ClaimNext — used by the dispatch loop to re-acquire a work item
	// of record. Fenced like every other acquire; carries no worker content.
	"ProdClaimer.AcquireSpecific": "§6.2 conditional fence-bump acquire of a specific prod work item",

	// §2.9/§6.1 dispatch-of-record (Story 2.9 / ISI-2526). The coordinator reads
	// a completed dependency's handoff VIA THE COORDINATION RECORD and defines the
	// next fenced work item. No parameter carries worker-authored content — the
	// surface is custody-only, never an agent-to-agent channel.
	"ProdDispatcher":                      "§2.9 dispatch-of-record coordinator bound to the prod schema",
	"NewProdDispatcher":                   "§2.9 constructor",
	"ProdDispatcher.DispatchNextOfRecord": "§2.9 decide+prioritize → create the next fenced work item",
	"ProdDispatcher.ReadHandoff":          "§6.1 read a completed dependency's handoff from the record",
	"DispatchDecision":                    "§2.9 coordinator's decide+prioritize input (code-supplied)",
	"DispatchResult":                      "§2.9 outcome of one dispatch-of-record cycle",
	"HandoffDoc":                          "§6.1 the handoff surfaced to the coordinator (read-of-record)",
	"HandoffView":                         "§6.1 read-only projection of a handoff over the record",
	"ArtifactContent":                     "§6.5 content of a coordination artifact surfaced to the coordinator",
	"AdoptRecommendation":                 "§2.9 coordinator adopts a handoff's recommended_next as new work",
	"RecordComment":                       "§6.1 append a provenanced coord.comment (sanctioned handoff half)",

	// §2.8/§6.5 structured handoff artifact writer (Story 2.8 / ISI-2525). On Run
	// complete/pause the agent publishes what the next actor needs as ONE
	// provenance-tagged artifact row + state change — the sanctioned handoff, not
	// a chat channel.
	"ProdHandoffWriter":              "§2.8 structured handoff artifact writer bound to the prod schema",
	"NewProdHandoffWriter":           "§2.8 constructor",
	"ProdHandoffWriter.WriteHandoff": "§2.8 write the handoff artifact + state change (comment+state, §6.1)",
	"HandoffWriteResult":             "§2.8 outcome of a handoff write (artifact id + state)",
	"HandoffKind":                    "§2.8 handoff variant (complete vs pause) discriminator",
	"DraftWorkItem":                  "§2.8 downstream work item drafted from the handoff (custody-only)",
	"ArtifactRef":                    "§6.5 content-addressed reference to a coordination artifact",
	"AuditHandoffContent":            "§6.5 audit projection of handoff content (read-only)",
	"AuditHandoffURI":                "§6.5 audit projection of a handoff artifact URI (read-only)",
	"ErrCompletingRunMismatch":       "§2.8 guard: only the completing Run may write its handoff",
	"ErrNotHandoffCustodian":         "§2.8 guard: only the item's custodian may write the handoff",
	"ErrSourceNotComplete":           "§2.8 guard: handoff requires the source Run to be complete",

	// §8 tier-2 scheduled resume for Paused(rate_limited) (Story 2.11 / ISI-2527).
	// A single durable wake fires at resume_at; no polling, no agent-to-agent path.
	"ResumeStore":           "§8 durable pause/resume store (single-wake, no polling)",
	"NewResumeStore":        "§8 constructor",
	"NewResumeForTest":      "§8 chaos-harness constructor (int-keyed schema)",
	"ResumeConfig":          "§8 resume store config (backoff/jitter), code-supplied",
	"DefaultResumeConfig":   "§8 sane default resume config",
	"ResumeStore.DB":        "§8 harness handle accessor",
	"ResumeStore.NextWake":  "§8 re-derive the next durable wake from resume_at",
	"ResumeStore.Pause":     "§8 persist resume_at once at pause time",
	"ResumeStore.ResumeDue": "§8 pop pauses whose resume_at has elapsed",
	"ResumeStore.Stats":     "§8 query-counter introspection proving the idle timer does zero reads",
	"PauseInfo":             "§8 a persisted pause (item + resume_at)",
	"DuePause":              "§8 a pause whose resume_at has elapsed and is due to wake",
	"Timer":                 "§8 single-durable-wake timer over the resume store",
	"NewTimer":              "§8 constructor",
	"Timer.Run":             "§8 sleep-until-resume_at loop (zero reads while idle)",
	"Timer.Notify":          "§8 out-of-band kick when an earlier-deadline pause lands",
	"EqualJitter":           "§8 equal-jitter backoff helper for resume_at derivation",

	// §6.4 Run reconcile machine's durable Store binding (Story 3.1 / ISI-2655,
	// physical integration of pkg/reconcile / ISI-2535). Custody-only: every method
	// is a fenced/step-CAS read or a co-committed advance over the coord.claim +
	// §6.5 audit + canonical §6.6 outbox record. No parameter carries worker-authored
	// content and nothing published re-enters coordination (§6.4/§17.4 no-P2P) —
	// from_step/to_step ride in a one-way outbox projection, not an agent-to-agent
	// channel.
	"ProdReconcileStore":            "§6.4 durable reconcile Store bound to the prod coord schema",
	"NewProdReconcileStore":         "§6.4 constructor (binds one Run's claim row)",
	"ProdReconcileStore.Step":       "§6.4 read the durable reconcile_step (source of truth, AC2)",
	"ProdReconcileStore.Fence":      "§6.3 read the monotonic fence token",
	"ProdReconcileStore.Advance":    "§6.4 conditional step-CAS advance co-committing audit+outbox (AC3/AC6)",
	"ProdReconcileStore.Reclaim":    "§6.3 monotonic fence-first reclaim (bump + stamp reclaim_fenced_at)",
	"ProdReconcileStore.SetStep":    "§8 unguarded re-point for the Failed→Claiming retry re-entry",
	"ProdReconcileStore.AuditRows":  "§6.5 count of reconcile-advance audit rows (co-commit assertion)",
	"ProdReconcileStore.OutboxRows": "§6.6 count of reconcile-advance outbox rows (co-commit assertion)",
	"ProdReconcileStore.Err":        "§6.4 sticky infrastructure-error accessor (requeue signal)",
}

// forbiddenNetCalls are selector calls the spine must never issue. The
// coordination store mediates ALL interaction (§6 no-P2P / §8.4); the spine
// package holds no listener, dialer, or embedded server.
var forbiddenNetCalls = map[string]map[string]bool{
	"net": {
		"Listen": true, "ListenPacket": true, "ListenTCP": true, "ListenUDP": true,
		"ListenUnix": true, "ListenUnixgram": true, "ListenIP": true,
		"Dial": true, "DialContext": true, "DialTCP": true, "DialUDP": true,
		"DialUnix": true, "DialIP": true,
	},
	"http": {
		"ListenAndServe": true, "ListenAndServeTLS": true, "Serve": true, "ServeTLS": true,
	},
	"grpc": {"NewServer": true},
}

// coordPkgDir is this package's directory (tests run with CWD = package dir).
var coordPkgDir = "."

// migrationsDir is the forward-only migration set, relative to this package.
var migrationsDir = filepath.Join("..", "..", "db", "migrations")

// nonTestGoFiles returns the non-test .go sources of dir (the shipped spine,
// not the guards themselves).
func nonTestGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read %s", dir)
	var files []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	require.NotEmpty(t, files, "no non-test sources found in %s — scanner misconfigured", dir)
	return files
}

// exportedSurface parses the package's non-test sources and returns every
// exported identifier, methods qualified as Type.Method.
func exportedSurface(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	surface := map[string]string{}
	for _, file := range nonTestGoFiles(t, dir) {
		f, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err, "parse %s", file)
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil || len(d.Recv.List) == 0 {
					surface[d.Name.Name] = "package-level func"
					continue
				}
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok && id.IsExported() {
					surface[id.Name+"."+d.Name.Name] = "method on " + id.Name
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							surface[s.Name.Name] = "type"
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								surface[n.Name] = "package-level var/const"
							}
						}
					}
				}
			}
		}
	}
	return surface
}

func TestFRB3_CoordSurfaceAllowlist(t *testing.T) {
	surface := exportedSurface(t, coordPkgDir)

	// (a) Hard rejection first: nothing chat-shaped may exist AT ALL, whatever
	// the allowlist says — updating the allowlist cannot smuggle a chat
	// channel past FR-B3.
	for id := range surface {
		require.NotRegexp(t, chatShapeRe, id,
			"FR-B3 violation: %q is chat-shaped. The coordination API has NO agent-to-agent chat channel: handoff = comment + state change on a work item (§6.1). Remove the identifier; do not allowlist it.", id)
	}

	// (b) Allowlist pin: the surface is exactly the custody-only set below.
	// A new identifier fails here so its author must justify it in this file.
	var unexpected []string
	for id := range surface {
		if _, ok := allowedSurface[id]; !ok {
			unexpected = append(unexpected, id)
		}
	}
	sort.Strings(unexpected)
	require.Empty(t, unexpected,
		"pkg/coord grew new exported identifiers %v.\nFR-B3 (Story 2.5): every coordination-surface identifier is pinned in allowedSurface with a §reference. If the addition is a legitimate custody operation, add it to allowedSurface in the same PR. If it is an agent-to-agent channel, it is prohibited — handoff = comment/state change on work_item only (§6.1, §8.4).", unexpected)

	var missing []string
	for id := range allowedSurface {
		if _, ok := surface[id]; !ok {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"allowedSurface references %v which no longer exist — prune the allowlist so the pin keeps meaning something.", missing)
}

// migrationSQL concatenates the forward-only migration set in filename order
// (the order the apiserver runner applies them).
func migrationSQL(t *testing.T) (string, []string) {
	t.Helper()
	entries, err := os.ReadDir(migrationsDir)
	require.NoError(t, err, "read %s", migrationsDir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	require.NotEmpty(t, names, "no migrations found in %s", migrationsDir)
	var b strings.Builder
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(migrationsDir, n))
		require.NoError(t, err, "read migration %s", n)
		b.WriteString("\n-- file: " + n + "\n")
		b.Write(raw)
	}
	return b.String(), names
}

var createTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([\w."]+)`)

func TestFRB3_NoChatTableInAnyMigration(t *testing.T) {
	sqlText, _ := migrationSQL(t)

	matches := createTableRe.FindAllStringSubmatch(sqlText, -1)
	require.NotEmpty(t, matches, "no CREATE TABLE found — scanner misconfigured")
	for _, m := range matches {
		table := strings.Trim(m[1], `"`)
		schema, bare := "", table
		if i := strings.LastIndex(bare, "."); i >= 0 {
			schema, bare = bare[:i], bare[i+1:]
		}
		// FR-B3 forbids an agent-to-agent COORDINATION channel, not every table
		// whose name reads as conversational. The `discussion` schema is the
		// sanctioned Per-Project Discussion Room (Arch §7.5, Story 10.1 / ISI-2709)
		// — "conversation, not custody": it is structurally coordination-free
		// (0004_discussion_schema.sql carries NO claim/lease/fence_token/state/
		// holder/assignee/status column and no custody-transfer expression, proven
		// by AC4), so custody CANNOT move through it. Handoff still happens only via
		// coord.comment + a work_item state change (§6.1, §8.4). We allowlist the
		// whole `discussion` schema (not just today's tables) so the guard keeps its
		// teeth for any un-namespaced or `coord.`-schema chat-shaped table while
		// permitting this architecturally-blessed human/agent room surface.
		if schema == "discussion" {
			continue
		}
		require.NotRegexp(t, chatShapeRe, bare,
			"FR-B3 violation: table %s is chat-shaped (migration set). There is NO agent-to-agent message store: handoff = comment + state change on coord.work_item (§6.1). If this table is a HUMAN-facing channel (console notifications etc.), rename it so it cannot read as agent chat (e.g. notification, announcement).", table)
	}
}

// tableBlock extracts the CREATE TABLE body for schema.table from the full
// migration text.
func tableBlock(t *testing.T, sqlText, table string) string {
	t.Helper()
	start := regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + regexp.QuoteMeta(table) + `\s*\(`)
	m := start.FindStringIndex(sqlText)
	require.NotNil(t, m, "CREATE TABLE %s not found in migrations", table)
	end := strings.Index(sqlText[m[0]:], "\n);")
	require.Greater(t, end, 0, "terminator for %s not found", table)
	return sqlText[m[0] : m[0]+end]
}

func requireColumn(t *testing.T, block, column, table string) {
	t.Helper()
	require.Regexp(t, regexp.MustCompile(`(?m)^\s*`+column+`\s+\w`), block,
		"%s is missing column %q — the sanctioned handoff channel is pinned by FR-B3; if the schema legitimately changed, update this pin in the same PR with a §reference", table, column)
}

func TestFRB3_SanctionedHandoffChannelPinned(t *testing.T) {
	sqlText, _ := migrationSQL(t)

	// The positive half of FR-B3: handoff = comment + state change on the
	// work item. Pin that those surfaces exist, that a comment carries
	// provenance, and that history is append-only — structurally, via the
	// triggers Story 2.1 shipped (§6.5).
	comment := tableBlock(t, sqlText, "coord.comment")
	requireColumn(t, comment, "work_item_id", "coord.comment")
	requireColumn(t, comment, "author_principal", "coord.comment")
	requireColumn(t, comment, "body", "coord.comment")

	workItem := tableBlock(t, sqlText, "coord.work_item")
	requireColumn(t, workItem, "state", "coord.work_item")

	require.Regexp(t, `(?m)^CREATE\s+TRIGGER\s+comment_append_only\b`, sqlText,
		"coord.comment must stay append-only (§6.5): the comment channel is durable handoff history, not a mutable chat log")
	require.Regexp(t, `(?m)^CREATE\s+TRIGGER\s+comment_no_truncate\b`, sqlText,
		"coord.comment must reject TRUNCATE too (ISI-2339 F2)")
	require.Regexp(t, `(?m)^CREATE\s+FUNCTION\s+coord\.reject_mutation\b`, sqlText,
		"the append-only enforcement function must exist")
}

func TestFRB3_SpineOpensNoDirectPeerSockets(t *testing.T) {
	fset := token.NewFileSet()
	for _, file := range nonTestGoFiles(t, coordPkgDir) {
		f, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err, "parse %s", file)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if names := forbiddenNetCalls[pkg.Name]; names != nil && names[sel.Sel.Name] {
				t.Errorf("FR-B3 / §6 no-P2P violation: %s calls %s.%s — the spine never opens or dials sockets; all mediation goes through the shared coordination store (§8.4)",
					filepath.Base(file), pkg.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

// Compile-time pin: the package under guard is imported by these tests, so a
// rename/move of pkg/coord breaks this suite loudly instead of silently
// guarding a directory that no longer exists.
var _ = coord.New
