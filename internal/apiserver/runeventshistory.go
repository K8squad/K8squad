package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/events"
)

// ============================================================================
// GET /api/runs/{runId}/events — durable Activity-history hydration (ISI-7484)
// ============================================================================
//
// Since the ISI-7074 family the run-step journal (thinking / tool / lifecycle /
// ticket_comment) is live-SSE-only: coord.outbox entity='run' rows fanned by
// the projector (runevents.go) and resumed via Last-Event-ID replay — but the
// console's Activity reconstruction had NO durable read, so a page reload or
// late join on a post-roll run rendered an EMPTY Activity even for runs that
// produced steps (ISI-7480/ISI-7482).
//
// This handler exposes the SAME journal as a plain JSON tail: the rows the SSE
// replayer would send, ascending, with the SAME id key (the outbox row id) the
// SSE id: line carries — so the console seeds its event list from this
// response, opens the live stream, and dedupes the hydration/live overlap
// exactly like a reconnect replay. It is a read-only downstream projection
// (§17.4): nothing here writes the outbox or re-enters coordination, and no
// raw telemetry is re-persisted into coord.comment (ISI-7454 guardrail).

const (
	// defaultRunEventHistoryLimit is the hydration page size when the caller
	// sends no ?limit=. Generous — a long run's full Activity in one round
	// trip — while staying a bounded single query.
	defaultRunEventHistoryLimit = 500
	// maxRunEventHistoryLimit caps a caller-supplied ?limit= so a hostile or
	// buggy caller cannot ask for an unbounded scan.
	maxRunEventHistoryLimit = 1000
)

// RunEventHistoryItem is one durable outbox row in the hydration response. The
// field trio mirrors the SSE wire form exactly (runevents.go runEventToSSE):
// ID is the outbox row id (the Last-Event-ID resume key), Type the outbox
// event_type (the SSE event: name), Data the row's payload verbatim (the SSE
// data: body) — so the client shapes rows with the SAME code path it shapes
// live frames.
type RunEventHistoryItem struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	At   time.Time       `json:"at"`
}

// RunEventHistoryResponse is the GET /api/runs/{runId}/events body. Events is
// always an array (empty, never null) so clients can map over it directly;
// LastID is the highest row id in the batch — the cursor a deep-history client
// passes back as ?afterId= to page further into the past of a long run.
type RunEventHistoryResponse struct {
	RunID  string                `json:"runId"`
	Events []RunEventHistoryItem `json:"events"`
	Count  int                   `json:"count"`
	LastID string                `json:"lastId,omitempty"`
}

// runEventUIDResolver resolves the wire run id (a run CR NAME) to the run UID
// the outbox keys on — the read-side sibling of sse.go's runUIDResolver seam,
// kept as a func type so tests stub resolution without a kube cache.
type runEventUIDResolver func(ctx context.Context, name string) (string, error)

// runEventsHandler serves GET /api/runs/{runId}/events: this run's durable
// outbox run-entity tail, ascending, from afterID (0 ⇒ from the beginning)
// bounded by limit. Authorization rides the same §13 choke point as every run
// route, plus the SAME name→UID resolution the stream route does (ISI-5248):
// for a tenant the resolver IS the scope check (a miss hides existence), while
// an admin may additionally hydrate by raw UID — the journal outlives the Run
// CR, and history for a deleted CR should still read for fleet-wide eyes only.
func runEventsHandler(reader events.RunEventReader, resolve runEventUIDResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		runID := decodePathVar(mux.Vars(r)["runId"])
		if runID == "" || !runIDPattern.MatchString(runID) {
			writeJSONError(w, http.StatusBadRequest, "invalid runId")
			return
		}

		afterID := int64(0)
		if raw := r.URL.Query().Get("afterId"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid afterId")
				return
			}
			afterID = n
		}
		limit := defaultRunEventHistoryLimit
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

		// Resolve name→UID (ISI-5248): the outbox keys rows by run.UID, not the
		// wire name. Best-effort like the stream route — but a miss is a 404
		// here (a hydration read has no raw-name fallback worth serving: rows
		// are keyed by UID and a name key would match nothing).
		uid := ""
		if resolve != nil {
			if u, err := resolve(r.Context(), runID); err == nil && runIDPattern.MatchString(u) {
				uid = u
			}
		}
		if uid == "" {
			// UID-native fallback (admin or resolver-less dev host only): the
			// journal outlives the Run CR, so history for a deleted CR is still
			// readable by address. Tenants never take this path — for them the
			// resolver IS the tenancy check, so a miss stays an honest 404.
			if _, err := uuid.Parse(runID); err == nil && (auth.IsAdmin || resolve == nil) {
				uid = runID
			} else {
				writeJSONError(w, http.StatusNotFound, "run not found")
				return
			}
		}

		rows, err := reader.RunEventsForRun(r.Context(), uid, afterID, limit)
		if err != nil {
			// A non-uuid uid cannot reach here (resolved UID or uuid.Parse'd),
			// so this is a genuine upstream read failure.
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}

		resp := RunEventHistoryResponse{
			RunID:  uid,
			Events: make([]RunEventHistoryItem, 0, len(rows)),
			Count:  len(rows),
		}
		for _, row := range rows {
			item := RunEventHistoryItem{
				ID:   strconv.FormatInt(row.ID, 10),
				Type: row.EventType,
				Data: json.RawMessage(row.Payload),
				At:   row.OccurredAt,
			}
			if len(item.Data) == 0 {
				item.Data = json.RawMessage("{}")
			}
			resp.Events = append(resp.Events, item)
			resp.LastID = item.ID
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
