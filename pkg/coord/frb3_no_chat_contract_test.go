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
	"Coordinator.WithFencer":    "§6.3 wire the resource-layer fence that gates reclaim release (fail-closed)",
	"ResourceFencer":            "§6.3 resource-layer kill/cordon confirm seam — custody gate, not an A2A channel",

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
	// Timer.Run/Timer.Notify now live on the package-private generic wakeLoop
	// (the harness Timer and the prod ProdTimer share one loop, ISI-2883) —
	// promoted methods, invisible to this scanner by construction.
	"EqualJitter": "§8 equal-jitter backoff helper for resume_at derivation",

	// §2.10/§6.2-6.3 rate-limit re-route to a different-credential Agent (Story
	// 2.10 / ISI-2882). Custody-only, same discipline as reclaim (2.4) and
	// handoff (2.8): fenced RELEASE → coordinator RE-DISPATCH → §6.2 claim —
	// never a P2P lease handoff. The hold names the throttled credential's
	// OPAQUE identity (7.6) so the credentialed claim can refuse the same
	// credential while the window is live; no seat token is ever re-pointed
	// (§11.2/ADR-041) and no parameter carries worker-authored content.
	"ReroutePolicy":                           "§2.10/3.7 escalation policy (repeat attempts / long Retry-After), code-supplied",
	"ReroutePolicy.Validate":                  "§2.10/3.7 fail-open guard: rejects AfterAttempts<2 / non-positive window (ISI-3083 F4)",
	"DefaultReroutePolicy":                    "§2.10 sane default escalation policy",
	"MaxHoldWindow":                           "§2.10/3.7 upper bound on a hold's resume_at, mirrors resume.BackoffCap (ISI-3083 F5)",
	"ShouldReroute":                           "§2.10/3.7 pure verdict: does this escalated pause re-route?",
	"PickAlternateCredential":                 "§2.10/7.6 pure roster pick: an Agent credential that differs from the throttled one",
	"ProdRerouteStore":                        "§2.10 fenced-release + re-dispatch + hold store bound to the prod schema",
	"NewProdRerouteStore":                     "§2.10 constructor",
	"ProdRerouteStore.ReleaseForReroute":      "§2.10/§6.3 fenced release → todo re-dispatch → throttled-credential hold (one txn)",
	"ProdRerouteOption":                       "§2.10 functional option for the reroute store (event capture opt-in)",
	"WithRerouteOutboxCapture":                "§2.10/§17.4 co-commit a work_item/reroute_released outbox event in the release txn (emit-only)",
	"RerouteReleaseResult":                    "§2.10 outcome of a release (installed fence + idempotency marker)",
	"ErrNotRerouteCustodian":                  "§2.10 guard: only the paused Run's own holder/run may be released",
	"ErrRerouteNotInProgress":                 "§2.10 guard: a re-route re-dispatches live (in_progress) work only",
	"ProdClaimer.ClaimNextCredentialed":       "§2.10/§6.2 SKIP-LOCKED pop that skips items a live hold pins for this credential (7.6)",
	"ProdClaimer.AcquireSpecificCredentialed": "§2.10/§6.2 guarded acquire of a named item, refused while a live hold pins this credential (7.6)",

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

	// §6.4 READ side of the durable step (ISI-2655 slice-3): the Run status
	// controller projects reconcile_step → Run.status. Read-only — no advance, no
	// audit/outbox, no agent-to-agent channel; it only reports the committed step.
	"ReconcileStepReader":                 "§6.4 read-only reader of the committed reconcile_step (status projection, AC2)",
	"NewReconcileStepReader":              "§6.4 constructor (binds the read-only step reader to the coord pool)",
	"ReconcileStepReader.StepForWorkItem": "§6.4 read the committed reconcile_step by work_item_id (status projection, AC2)",

	// §6.4 Run reconcile machine's side-effect seam (reconcile.Effects) bound to the
	// prod coord schema (Story 3.1 / ISI-2655, child ISI-2802). Custody/execution-only:
	// each effect is an at-most-once durable marker (coord.sandbox_bind / a2a_dispatch /
	// artifact) plus a §6.5 audit row, keyed by the Run's deterministic id so a §6.4
	// crash-window re-drive reattaches rather than re-applying. No parameter carries
	// worker-authored content, and nothing recorded re-enters coordination (no-P2P). The
	// physical warm-pool/A2A mechanisms are the SandboxBinder/TaskDispatcher execution
	// ports (§10.1 run-execution dispatch), NOT an agent-to-agent chat channel.
	"ProdEffects":             "§6.4 durable reconcile.Effects bound to the prod coord schema",
	"NewProdEffects":          "§6.4 constructor (binds one Run's effect markers + physical ports)",
	"ProdEffects.BindSandbox": "§6.2/§9 warm-pool bind, run_id-keyed (reattach, never re-provision)",
	"ProdEffects.Dispatch":    "§6.4/§10.1 A2A shim submit, a2a_task_id-keyed (reattach, never re-execute)",
	"ProdEffects.Collect":     "§6.1 content-addressed artifact upsert (republish, never dupe)",
	"ProdEffects.Terminal":    "§6.5 record the terminal transition (at-most-once per committed advance)",
	"ProdEffects.Err":         "§6.4 sticky infrastructure-error accessor (requeue signal)",
	"SandboxBinder":           "§9 physical warm-pool bind port (custody/execution, run-id only)",
	"TaskDispatcher":          "§10.1 physical A2A shim submit port (run-execution dispatch, no content)",

	// Story 3.7 prod resume binding (resumeprod.go, ISI-2883): the uuid-keyed
	// scheduled-resume surface — custody/schedule operations on the pause
	// episode row, no agent-to-agent channel.
	"ProdResumeStore":           "§8 tier-2 scheduled resume bound to coord.run_pause (custody/schedule)",
	"NewProdResumeStore":        "§8 constructor (uuid-keyed pause-episode store, migration 0009)",
	"DefaultProdResumeConfig":   "§8 v1 policy + the production Pause table binding",
	"ProdResumeStore.Pause":     "§8 record/refresh the single durable pause episode (resume_at)",
	"ProdResumeStore.Pending":   "§8 pending-episode probe (the driver's park guard)",
	"ProdResumeStore.NextWake":  "§8 earliest pending resume_at (the single wake derivation)",
	"ProdResumeStore.ResumeDue": "§8 exactly-once SKIP LOCKED claim of due episodes",
	"ProdResumeStore.Stats":     "§8 statement counters (the no-polling proof surface)",
	"ProdResumeStore.DB":        "§8 backing handle accessor (same surface as ResumeStore.DB)",
	"ProdDuePause":              "§8 one claimed resume: real coord uuids + attempt + resume_at",
	"ProdTimer":                 "§8 the production single-wake scheduler (uuid instantiation)",
	"NewProdTimer":              "§8 constructor (wake loop + OnDue re-entry callback)",

	// §6.3 background crash-safe reclaim sweeper (Story 2.4 / ISI-3104): a
	// periodic scan that reclaims expired leases the same custody way ClaimNext
	// does opportunistically — one data-modifying CTE co-commits the monotonic
	// fence bump (fences the dead holder), the reclaim_fenced_at stamp + holder/
	// lease release, and work_item→open, under FOR UPDATE OF claim SKIP LOCKED.
	// Custody-only: no parameter carries worker-authored content, the loop is
	// stateless (crash-safe re-derivation from durable claim state), and the
	// OnReclaim callback is the §6.3 resource-layer fence seam, not an A2A channel.
	"SweepConfig":                            "§6.3 sweeper config (interval/batch/jitter), code-supplied",
	"DefaultSweepConfig":                     "§6.3 sane default sweeper config",
	"Reclaimed":                              "§6.3 one reclaimed lease (item + fenced holder), custody record",
	"SweepStore":                             "§6.3 durable expired-lease reclaim store bound to coord.claim",
	"NewSweepStore":                          "§6.3 constructor",
	"NewSweepForTest":                        "§6.3 chaos-harness constructor (int-keyed schema)",
	"SweepStore.ReclaimExpired":              "§6.3 fence-first batch reclaim of expired leases (CTE, SKIP LOCKED)",
	"SweepStore.Scans":                       "§6.3 scan-counter introspection for tests/ops",
	"SweepStore.DB":                          "§6.3 harness handle accessor",
	"Sweeper":                                "§6.3 the periodic reclaim loop (stateless, crash-safe)",
	"NewSweeper":                             "§6.3 constructor (store + metrics + OnReclaim fence callback)",
	"Sweeper.Run":                            "§6.3 run the periodic scan until context cancellation",
	"SweeperMetrics":                         "§6.3 sweeper metrics sink interface (cycle/reclaim/duration)",
	"PrometheusSweeperMetrics":               "§6.3 Prometheus metrics sink for the sweeper",
	"NewPrometheusSweeperMetrics":            "§6.3 constructor",
	"PrometheusSweeperMetrics.IncSweepCycle": "§6.3 count one completed sweep cycle",
	"PrometheusSweeperMetrics.AddSweepReclaims":     "§6.3 add the reclaims from one cycle",
	"PrometheusSweeperMetrics.ObserveSweepDuration": "§6.3 record one sweep-cycle duration",
	"PrometheusSweeperMetrics.Signals":              "§6.3 emitted metric names (NFR-OBS3 cardinality proof)",

	// §8.6/§13 human board-lane status transition (Story 8.14a / ISI-2909, gap
	// ISI-2876). The Kanban board is a PROJECTION of work_item.state; this is the
	// write path for a HUMAN to move a card between lanes. Custody-only in the
	// FR-B3 sense: it changes a work_item's own lane + writes a §6.5 audit row,
	// carries NO worker-authored content, and — ADR-037 — holds no claim and bumps
	// no fence (audit fence_token NULL, the claim untouched). Not an agent channel:
	// agents hand off via comment + Complete (§6.1/§2.8), never through this op.
	"StateTransition":                 "§8.6 outcome of a human lane move (from/to lane projection, read-only)",
	"ErrInvalidState":                 "§8.6 guard: target is not one of the pinned board lanes (→400)",
	"ErrWorkItemNotFound":             "§12.1 guard: item outside the caller's Team scope is 404-not-403",
	"ErrStateConflict":                "§8.6 guard: fromState precondition missed / already in target lane (→409)",
	"HumanStateStore":                 "§8.6/§13 human board-lane transition store bound to the prod schema",
	"NewHumanStateStore":              "§8.6 constructor",
	"HumanStateStore.TransitionState": "§8.6/§6.5 conditional lane CAS + audit, no-fence (ADR-037), Team-scoped",

	// §10 pause/resume + §11 per-user credentials + §7.2 credentialLifecycle
	// (Stories 7.4+7.6 / ISI-2898, gap ISI-2876). Reuses the 2.11/3.7 resume
	// machinery, keyed on the per-user credential (attribution, 7.6) with a
	// legible reason family (7.4). Custody-only: an episode names a credential
	// and moves a Run into a Paused(reason) step + one §6.6 outbox event;
	// ApplyCredentialSignal co-commits the three facts in ONE txn (AC6). No
	// parameter carries worker-authored content, nothing published re-enters
	// coordination (§17.4 no-P2P) — the shim signal is a lifecycle observation,
	// not an agent-to-agent channel. SelectAlternate/EarliestResume are the pure
	// read-side advisories the §2.10 re-route consumes.
	"CredentialPauseStore":                 "§10/7.6 per-credential pause/resume ledger (single-wake, no polling)",
	"NewCredentialPauseStore":              "§10 constructor",
	"NewCredPauseForTest":                  "§10 chaos-harness constructor (self-contained schema)",
	"CredPauseConfig":                      "§10 credential-pause store config (backoff/jitter), code-supplied",
	"DefaultCredPauseConfig":               "§10 sane default credential-pause config",
	"CredPauseReason":                      "§7.4 legible pause reason family (rate_limited|expired|rotated|unreachable)",
	"CredPauseReason.Valid":                "§7.4 fail-closed reason guard",
	"CredPauseReason.TimerResumed":         "§7.4 resume-mode split: timer (rate_limited) vs refresh",
	"CredentialClass":                      "§11 story-pinned credential model tag (claude_oauth|api_key|byo_endpoint)",
	"CredentialClass.Valid":                "§11 fail-closed class guard",
	"CredentialRef":                        "§11 per-user credential identity (the attribution key), code-supplied",
	"CredPauseRequest":                     "§10 one credential-lifecycle observation to record (custody-only)",
	"CredPauseInfo":                        "§10 durable outcome of a credential pause (reason + resume horizon)",
	"CredDuePause":                         "§8 a credential pause whose resume_at elapsed and is due to wake",
	"CredPauseView":                        "§2.10/8.6 advisory read of a held credential (reason + horizon, read-only)",
	"CredentialPauseStore.PauseCredential": "§10 persist/refresh a credential pause episode",
	"CredentialPauseStore.ResumeOnRefresh": "§7.4 clear a refresh-mode hold when fresh material lands (idempotent)",
	"CredentialPauseStore.ResumeDue":       "§8 pop credential pauses whose resume_at has elapsed (SKIP LOCKED)",
	"CredentialPauseStore.NextWake":        "§8 re-derive the next durable timer wake from resume_at",
	"CredentialPauseStore.PausedSet":       "§2.10/8.6 read the pending held set (advisory projection)",
	"CredentialPauseStore.DB":              "§10 harness handle accessor",
	"ReasonRateLimited":                    "§7.6 reason: subscription throttled (timer resume)",
	"ReasonCredentialExpired":              "§7.4 reason: credential expired (refresh resume)",
	"ReasonCredentialRotated":              "§7.4 reason: credential rotated (refresh resume)",
	"ReasonEndpointUnreachable":            "§7.4/7.5 reason: BYO endpoint unreachable (refresh resume)",
	"ClassClaudeOAuth":                     "§7.2 credential class: per-user Claude OAuth seat token",
	"ClassAPIKey":                          "§7.3 credential class: long-lived provider API key",
	"ClassBYOEndpoint":                     "§7.5 credential class: BYO/Ollama endpoint URL (+ optional token)",
	"SelectAlternate":                      "§2.10/7.6 pure re-route advisory: pick an unheld credential",
	"EarliestResume":                       "§2.10 pure scheduling hint: earliest timer horizon in the held set",
	"ApplyCredentialSignal":                "§10/7.4 atomic shim-signal applier (episode + guarded step + §6.6 outbox, one txn)",
	"ApplyResult":                          "§10 outcome of an applied signal (episode + whether the step moved)",
	"CredentialSignal":                     "§7.2 one credentialLifecycle observation from the shim (custody-only, code-supplied)",
	"CredentialEvent":                      "§7.2 the lifecycle event kind (expired|rotated|rate_limited|unreachable)",
	"EventExpired":                         "§7.2 lifecycle event: token no longer authenticates",
	"EventRotated":                         "§7.2 lifecycle event: Secret rotated mid-Run",
	"EventRateLimited":                     "§7.2 lifecycle event: provider throttle (Retry-After)",
	"EventUnreachable":                     "§7.2/7.5 lifecycle event: BYO endpoint not answering",
	"SignalStep":                           "§7.4 pure map: lifecycle event → durable Paused(reason) step",
	"EventReason":                          "§7.4 pure map: lifecycle event → ledger reason",
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
