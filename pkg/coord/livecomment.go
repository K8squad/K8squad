// livecomment.go — the per-run liveness echo for durable work-item thread
// comments (ISI-7274, parent ISI-7262 WS-2).
//
// WHY. On the issue/ticket page the agent's final coord.comment (the run's
// terminal recap: the settle summary / progress row, or the agent's own
// task-io post-comment) was never pushed live. The page surfaces it only by
// re-fetching the thread on the run's terminal `ended` milestone
// (console useTicketRunStream), which is racy: the settle commits its comment
// AFTER the `ended` lifecycle row (the machine advances to the terminal step
// first, then Terminal settles, its own transaction after the advance — an
// ordering that is load-bearing: the ended event is co-committed with the
// advance it narrates, and the settle can only narrate an advance that
// committed). A `ended`-keyed re-fetch can therefore miss the comment and the
// user must refresh manually (ISI-7262).
//
// THE SEAM (mirrors ISI-5193's progress mirror and WS-1's discussion_message,
// ISI-7272): the durable coord.comment row stays the record; a run-entity
// `ticket_comment` outbox row is the transient liveness signal. The apiserver's
// run-event projector (runevents.go) fans every entity='run'+run_id outbox
// row onto the per-run SSE hub — no allowlist change needed for the per-run
// fan, and the frame rides the SAME /api/runs/{id}/stream the ticket page
// already holds (§13 "one bus, no polling"). When the frame arrives the console
// re-fetches the thread, and because the echo commits in the SAME transaction
// as the comment (settle) or strictly AFTER it (taskio post-commit capture),
// the refetch always materializes the durable row — the fast-commit race on the
// `ended` milestone is closed without touching the machine's advance ordering.
//
// CONTRACT. Single-token event_type by §10.2 subject-taxonomy discipline. The
// payload mirrors the durable triple the thread renders (author, body, the
// server-stamped comment time) plus the comment kind (summary / progress /
// plain — empty for a plain comment), so the console can route it without a
// refetch when it chooses to; today the ticket surface just re-fetches, which
// is idempotent and renders from durable truth.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/K8squad/K8squad/pkg/events"
)

// LiveCommentEventType is the coord.outbox event_type of the durable
// work-item comment echo: single-token by subject-taxonomy discipline (§10.2
// — dots are NATS subject metacharacters). The apiserver run-event projector
// maps it onto the run's per-run SSE stream as `event: ticket_comment`.
const LiveCommentEventType = "ticket_comment"

// LiveCommentEcho is the payload frame carried on the per-run SSE stream: the
// durable coord.comment triple plus its kind, so the frame is self-describing
// without a refetch. Field-compatible across both producers (the settle's
// co-committed echo and the task-io post-comment capture).
type LiveCommentEcho struct {
	WorkItemID string    `json:"workItemId"`
	RunID      string    `json:"runId"`
	Author     string    `json:"author"`
	Body       string    `json:"body"`
	Kind       string    `json:"kind,omitempty"`
	At         time.Time `json:"at"`
}

// CaptureLiveCommentEcho appends ONE run-entity `ticket_comment` outbox row
// over the pool, best-effort: errors are logged and swallowed — the durable
// comment row is the record, a lost echo costs liveness, never correctness
// (the same posture as WS-1's discussion_message capture). Call it
// POST-commit, when the comment is already durable: the outbox row rides its
// own transaction, so an echo for a comment that does not exist is impossible
// and a lost echo degrades to reload-only. A missing workItemID/runID/body
// skips silently (defensive; nothing keys a per-run fan without run_id). The
// settle path instead co-commits via events.CaptureRunForWorkItem inside its
// own transaction (settle.go insertSettleComment) — there the echo and the
// comment commit as one unit.
func CaptureLiveCommentEcho(ctx context.Context, db *sql.DB, echo LiveCommentEcho) {
	if db == nil || echo.WorkItemID == "" || echo.RunID == "" || echo.Body == "" {
		return
	}
	payload, err := json.Marshal(echo)
	if err != nil {
		slog.DebugContext(ctx, "coord: live-comment echo skipped (marshal failed)", "err", err)
		return
	}
	if err := events.CaptureRunForWorkItem(ctx, db, echo.WorkItemID, echo.RunID, LiveCommentEventType, payload); err != nil {
		slog.WarnContext(ctx, "coord: live-comment echo capture failed (comment stays durable)", "runID", echo.RunID, "err", err)
	}
}
