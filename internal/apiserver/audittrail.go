package apiserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Audit-trail query API (story 2.6, ISI-2881) — the read side of coord.audit_log.
//
// Every coordination write (pkg/coord prod claim/dispatch/effects/handoff, the
// human status transition, and the Epic-15 admin mutations via the ADR-040 sink)
// already appends an immutable coord.audit_log row (§6.5 — append-only BY
// TRIGGER, 0001). What was missing is the QUERY surface the README's §17.3
// diagram promises ("audit query API"): this file is it.
//
//   GET /api/audit?work_item=<uuid>&run=<uuid>&actor=<principal>&event_type=<t>
//                 &from=<rfc3339>&to=<rfc3339>&before=<id>&limit=<n>
//
// Mounted behind the §13 BFFAuthz choke point (identity is the server-derived
// AuthorContext, never a header). RBAC scoping is deny-by-default (§12.3):
//
//   - admin (global_role=admin): the full trail, every filter, every actor —
//     the operator's "activity across Runs" view story 2.6 asks for;
//   - non-admin: SELF-SCOPED — the query is pinned to the caller's own
//     principal (their human actions: status transitions, comments, admin-less
//     writes). An explicit foreign `actor` is a 403, not a silent clamp: the
//     caller learns the surface is role-scoped, not that the rows do not exist.
//     Team-scoping the audit is NOT possible in v1 — coord.audit_log carries no
//     team column (§6.1: the coord schema predates 15.3 memberships) — so
//     self-scope is the narrowest honest v1 bound; widening rides 15.3.
//
// Pagination rides the log's bigserial id (the monotonic audit sequence §6.5):
// pages descend newest-first; `nextBefore` carries the last id of the page and
// the next page asks `id < before`. No OFFSET — the sequence is the cursor.
// ============================================================================

// AuditEvent is one immutable coord.audit_log row — who (principal, on whose
// behalf), what (event_type + state provenance + payload), when (created_at),
// result (to_state / payload.detail). The JSON shape is the API contract the
// console's audit screen (console/components/audit) renders.
type AuditEvent struct {
	ID                int64           `json:"id"`
	WorkItemID        *string         `json:"workItemId,omitempty"` // uuid | null (platform events)
	RunID             *string         `json:"runId,omitempty"`      // uuid | null
	EventType         string          `json:"eventType"`
	Principal         string          `json:"principal"` // who acted (§6.5)
	InitiatedByUserID *string         `json:"initiatedByUserId,omitempty"`
	FenceToken        *int64          `json:"fenceToken,omitempty"`
	FromState         *string         `json:"fromState,omitempty"`
	ToState           *string         `json:"toState,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"` // structured detail (jsonb)
	CreatedAt         time.Time       `json:"createdAt"`
}

// AuditTrailQuery is the server-side query after RBAC scoping — the handler
// builds it from the request + AuthorContext; the reader executes it verbatim.
type AuditTrailQuery struct {
	WorkItemID *uuid.UUID // work_item filter (per-item history)
	RunID      *uuid.UUID // run filter
	Actor      string     // principal filter (SELF-SCOPED for non-admins by the handler)
	EventType  string     // event_type filter
	From       *time.Time // created_at >= From
	To         *time.Time // created_at <= To
	Before     int64      // cursor: id < Before (0 ⇒ first page, newest first)
	Limit      int        // page size (handler clamps 1..200, default 50)
}

// AuditTrailPage is one newest-first page plus the cursor for the next older
// page (nil when this page is the tail — no more rows match).
type AuditTrailPage struct {
	Events     []AuditEvent `json:"events"`
	NextBefore *int64       `json:"nextBefore"`
}

// ErrAuditTrailUnavailable marks a backing-store failure (the reader exists but
// the query failed) — surfaced as 502, distinct from a bad request (400).
var ErrAuditTrailUnavailable = errors.New("audit trail unavailable")

// AuditTrailReader answers scoped audit-trail queries. Production wires the
// Postgres reader over the shared DSN; tests wire a fake.
type AuditTrailReader interface {
	Query(ctx context.Context, q AuditTrailQuery) (AuditTrailPage, error)
}

// auditRows is the slice of *sql.Rows the reader scans — an interface so the
// unit lane can drive the scan/cursor/error paths with canned rows; the SQL
// text itself is proven against real Postgres by the integration lane
// (audittrail_integration_test.go, discussion_integration tag).
type auditRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// auditQueryer executes one bounded query. *sql.Rows satisfies auditRows, so
// the prod adapter is a one-method wrapper over *sql.DB.
type auditQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (auditRows, error)
}

type dbAuditQueryer struct{ db *sql.DB }

func (d dbAuditQueryer) QueryContext(ctx context.Context, query string, args ...any) (auditRows, error) {
	return d.db.QueryContext(ctx, query, args...)
}

// PostgresAuditTrailReader is the production reader over coord.audit_log.
// Read-only; every filter is a bound parameter — the dynamic WHERE is built
// from server-side values, never from raw request text.
type PostgresAuditTrailReader struct {
	query auditQueryer
}

// NewPostgresAuditTrailReader builds the reader over an open pool.
func NewPostgresAuditTrailReader(db *sql.DB) *PostgresAuditTrailReader {
	return &PostgresAuditTrailReader{query: dbAuditQueryer{db: db}}
}

// auditSelect is the stable projection every query shares. Ordering is the
// log's own monotonic sequence, DESC (newest first) — the §6.5 design point
// that makes cursor pagination exact with no OFFSET scan.
const auditSelect = `SELECT id, work_item_id, run_id, event_type, principal,
       initiated_by_user_id, fence_token, from_state, to_state, payload, created_at
  FROM coord.audit_log`

// Query executes the scoped query: dynamic WHERE (bound args), ORDER BY id DESC,
// LIMIT n+1 so a full page proves more rows follow without a COUNT scan.
func (r *PostgresAuditTrailReader) Query(ctx context.Context, q AuditTrailQuery) (AuditTrailPage, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	where, args := auditWhereClause(q)

	page := AuditTrailPage{Events: []AuditEvent{}}
	fetch := q.Limit + 1
	args = append(args, fetch)
	rows, err := r.query.QueryContext(ctx, auditSelect+where+" ORDER BY id DESC LIMIT $"+strconv.Itoa(len(args)), args...)
	if err != nil {
		return AuditTrailPage{}, ErrAuditTrailUnavailable
	}
	defer rows.Close()

	for rows.Next() {
		var e AuditEvent
		var workItem, run, initiatedBy, fromState, toState sql.NullString
		var fence sql.NullInt64
		var payload sql.NullString // jsonb scans as string; re-bound verbatim below
		if err := rows.Scan(&e.ID, &workItem, &run, &e.EventType, &e.Principal,
			&initiatedBy, &fence, &fromState, &toState, &payload, &e.CreatedAt); err != nil {
			return AuditTrailPage{}, ErrAuditTrailUnavailable
		}
		if workItem.Valid {
			s := workItem.String
			e.WorkItemID = &s
		}
		if run.Valid {
			s := run.String
			e.RunID = &s
		}
		if initiatedBy.Valid {
			s := initiatedBy.String
			e.InitiatedByUserID = &s
		}
		if fence.Valid {
			f := fence.Int64
			e.FenceToken = &f
		}
		if fromState.Valid {
			s := fromState.String
			e.FromState = &s
		}
		if toState.Valid {
			s := toState.String
			e.ToState = &s
		}
		if payload.Valid && payload.String != "" {
			e.Payload = json.RawMessage(payload.String)
		}
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return AuditTrailPage{}, ErrAuditTrailUnavailable
	}

	if len(page.Events) > q.Limit {
		page.Events = page.Events[:q.Limit]
		next := page.Events[q.Limit-1].ID
		page.NextBefore = &next
	}
	return page, nil
}

// auditWhereClause builds " WHERE …" ("" when unfiltered) with $n-bound args in
// clause order. Pure function — unit-tested without a database.
func auditWhereClause(q AuditTrailQuery) (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, cond+" $"+strconv.Itoa(len(args)))
	}
	if q.WorkItemID != nil {
		add("work_item_id =", q.WorkItemID.String())
	}
	if q.RunID != nil {
		add("run_id =", q.RunID.String())
	}
	if q.Actor != "" {
		add("principal =", q.Actor)
	}
	if q.EventType != "" {
		add("event_type =", q.EventType)
	}
	if q.From != nil {
		add("created_at >=", *q.From)
	}
	if q.To != nil {
		add("created_at <=", *q.To)
	}
	if q.Before > 0 {
		add("id <", q.Before)
	}
	if len(conds) == 0 {
		return "", nil
	}
	clause := " WHERE"
	for i, c := range conds {
		if i > 0 {
			clause += " AND"
		}
		clause += " " + c
	}
	return clause, args
}

// auditTrailHandler serves GET /api/audit. Rides the §13 choke point (mounted
// in routes); applies the 2.6 RBAC scoping; parses/validates every filter.
func auditTrailHandler(reader AuditTrailReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		author, ok := discussion.AuthFromContext(r.Context())
		if !ok || author.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}

		q := AuditTrailQuery{Limit: auditLimitParam(r)}

		if v := r.URL.Query().Get("work_item"); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "work_item must be a uuid")
				return
			}
			q.WorkItemID = &id
		}
		if v := r.URL.Query().Get("run"); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "run must be a uuid")
				return
			}
			q.RunID = &id
		}
		if v := r.URL.Query().Get("event_type"); v != "" {
			q.EventType = v
		}
		for name, dst := range map[string]**time.Time{"from": &q.From, "to": &q.To} {
			if v := r.URL.Query().Get(name); v != "" {
				t, err := time.Parse(time.RFC3339, v)
				if err != nil {
					writeJSONError(w, http.StatusBadRequest, name+" must be an RFC3339 timestamp")
					return
				}
				*dst = &t
			}
		}
		if v := r.URL.Query().Get("before"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				writeJSONError(w, http.StatusBadRequest, "before must be a positive audit id")
				return
			}
			q.Before = n
		}

		// ── 2.6 RBAC scoping (§12.3 deny-by-default) ──
		q.Actor = r.URL.Query().Get("actor")
		if !author.IsAdmin {
			if q.Actor != "" && q.Actor != author.Principal {
				writeJSONError(w, http.StatusForbidden, "the audit trail is admin-scoped; you may query your own principal only")
				return
			}
			q.Actor = author.Principal // self-scope, explicit or not
		}

		page, err := reader.Query(r.Context(), q)
		if errors.Is(err, ErrAuditTrailUnavailable) {
			writeJSONError(w, http.StatusBadGateway, "audit trail read model unavailable")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "audit query failed")
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}

// auditLimitParam clamps ?limit into the house 1..200 window (default 50) —
// same bounds as the /admin/users pagination.
func auditLimitParam(r *http.Request) int {
	limit := intQuery(r, "limit", 50)
	if limit <= 0 || limit > 200 {
		return 50
	}
	return limit
}
