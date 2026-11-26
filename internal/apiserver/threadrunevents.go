package apiserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/events"
)

// ============================================================================
// GET /api/projects/{projectId}/discussion/threads/{threadId}/run-events —
// durable discussion-feed hydration (ISI-7869, WS-C of parent ISI-7864)
// ============================================================================
//
// The discussion Room's live feed is SSE-only for run-scoped rows: thinking
// envelopes, ticket_comment echoes and run_ended milestones carry no thread
// id, so the Room correlates them by author to its LIVE working watches — a
// fresh tab (or a reload after the run finished) has no watches and renders
// an empty step feed even though every row is durable in coord.outbox.
//
// This handler is the discussion-room analog of the ISI-7484 run Activity
// fallback: it resolves the thread's dispatched runs through the durable
// mention_dispatch ledger (the same join the party-round enrichment and the
// reply-hop resolver ride: message_id -> work_item_id -> coord.claim.run_id)
// and returns each run's outbox run-entity tail — filtered to the event types
// the project bus bridges onto the room's named SSE events — together with
// the ANCHOR message id that dispatched the run, the correlation the live
// wire lacks. The console seeds its per-message thinking map from this tail
// on mount and folds discussion_message rows through the same idempotent
// reducers the live frames ride; overlap dedupes by (runId, seq) exactly like
// an SSE replay. Read-only downstream projection (§17.4): nothing here writes
// the outbox or re-enters coordination, and no raw telemetry is re-persisted
// into coord.comment (ISI-7454 guardrail).

const (
	// defaultThreadRunEventLimit is the per-run hydration page: the most
	// recent N outbox rows per run, mirroring the ISI-7484 Activity tail
	// size, while staying a bounded single query per run.
	defaultThreadRunEventLimit = 500
	// maxThreadRuns caps the runs hydrated for one thread so a party fan-out
	// cannot turn one request into an unbounded fan-out of per-run queries.
	maxThreadRuns = 64
)

// ThreadRunAnchorReader is the thread-to-runs read side this route rides
// (implemented by *discussion.Store). Narrow on purpose: the handler needs
// the ledger join and nothing else.
type ThreadRunAnchorReader interface {
	// ThreadRunAnchors returns the runs dispatched from the thread (ledger
	// rows whose work item a run claimed), each with the message that
	// dispatched it, newest dispatch first.
	ThreadRunAnchors(ctx context.Context, projectID string, threadID uuid.UUID) ([]discussion.ThreadRunAnchor, error)
}

// ThreadRunEventItem is one durable outbox row in the hydration response —
// the same field quartet the ISI-7484 run history serves (ID is the outbox
// row id, Type the outbox event_type, Data the payload verbatim, At the row's
// commit stamp), so the console shapes rows with the same code path it
// shapes live frames.
type ThreadRunEventItem struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	At   time.Time       `json:"at"`
}

// ThreadRunEventsRun is one dispatched run's hydration slice.
type ThreadRunEventsRun struct {
	RunID           string               `json:"runId"`
	AnchorMessageID string               `json:"anchorMessageId"`
	Events          []ThreadRunEventItem `json:"events"`
}

// ThreadRunEventsResponse is the GET .../run-events body. Runs is always an
// array (empty, never null) — an unknown, foreign, or dispatch-less thread is
// indistinguishable from an empty one (existence-hiding, §13).
type ThreadRunEventsResponse struct {
	ThreadID string               `json:"threadId"`
	Runs     []ThreadRunEventsRun `json:"runs"`
}

// threadRoomEventTypes is the allowlist of outbox run-entity event types the
// room's feed renders (the projector's projectBridgeEventTypes, keyed by
// outbox type): live thinking deltas, the final work-item comment echo, the
// terminal ended milestone, and the run-authored message echo. Everything
// else (raw frames the room never bridges) stays out of the hydration tail.
var threadRoomEventTypes = map[string]bool{
	"thinking":           true,
	"ticket_comment":     true,
	"ended":              true,
	"discussion_message": true,
}

// threadRunEventsHandler serves GET /api/projects/{projectId}/discussion/
// threads/{threadId}/run-events. Authorization rides the same §13 choke point
// as every discussion route; the ledger query is project-scoped by the path's
// project slug, so a foreign or missing thread yields an empty tail, never
// another Project's rows.
func threadRunEventsHandler(anchors ThreadRunAnchorReader, reader events.RunEventReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		vars := mux.Vars(r)
		projectID := decodePathVar(vars["projectId"])
		if projectID == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid projectId")
			return
		}
		threadID, err := uuid.Parse(decodePathVar(vars["threadId"]))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid threadId")
			return
		}

		limit := defaultThreadRunEventLimit
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				writeJSONError(w, http.StatusBadRequest, "invalid limit")
				return
			}
			if n < maxRunEventHistoryLimit {
				limit = n
			} else {
				limit = maxRunEventHistoryLimit
			}
		}

		runs, err := anchors.ThreadRunAnchors(r.Context(), projectID, threadID)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		if len(runs) > maxThreadRuns {
			runs = runs[:maxThreadRuns]
		}

		resp := ThreadRunEventsResponse{
			ThreadID: threadID.String(),
			Runs:     make([]ThreadRunEventsRun, 0, len(runs)),
		}
		for _, a := range runs {
			out := ThreadRunEventsRun{
				RunID:           a.RunID,
				AnchorMessageID: a.AnchorMessageID,
				Events:          make([]ThreadRunEventItem, 0),
			}
			rows, err := reader.RunEventsForRun(r.Context(), a.RunID, 0, limit)
			if err != nil {
				// Best-effort per run, like every room-echo seam: one run's
				// read failure never fails the whole hydration — the SSE tail
				// still covers that run live, and the message rows stay the
				// durable record either way.
				slog.WarnContext(r.Context(), "apiserver: thread run-events hydration skipped a run (read failed)",
					"threadID", threadID, "runID", a.RunID, "err", err)
				continue
			}
			for _, row := range rows {
				if !threadRoomEventTypes[row.EventType] {
					continue
				}
				item := ThreadRunEventItem{
					ID:   strconv.FormatInt(row.ID, 10),
					Type: row.EventType,
					Data: json.RawMessage(row.Payload),
					At:   row.OccurredAt,
				}
				if len(item.Data) == 0 {
					item.Data = json.RawMessage("{}")
				}
				out.Events = append(out.Events, item)
			}
			resp.Runs = append(resp.Runs, out)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
