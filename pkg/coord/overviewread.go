// overviewread.go — S4 (ISI-4509): the STATUS-HISTORY ROLLUP that backs the
// project-overview "tickets by status over time" stacked-area chart
// (DESIGN-SPEC-ISI-4505 §4/§6 open-Q1).
//
// The open question the design flagged was storage: does a "tickets per status
// over time" series need a net-new snapshot table? It does NOT. Every lane move
// already lands in coord.audit_log as an append-only `state_transition` row
// (from_state, to_state, created_at, work_item_id) — the SAME seam
// readStatusHistory (workitemread.go) already tails for the ticket thread. So a
// point-in-time reconstruction is a pure projection over two existing, indexed
// reads: the item set (for existence + the fallback initial state) and the
// transition log (for every state change up to the snapshot instant). No new
// write path, no rollup datastore, no retention surface — the same ADR-020
// "read model over existing stores" discipline the 8.8a dashboard holds.
//
// Reconstruction is deliberately a PURE function (reconstructStatusSnapshots)
// fed by the two queries, so the day-by-day state-machine replay is unit-tested
// without a database — the SQL stays thin and the algorithm stays legible.
//
// Tenancy mirrors the sibling board reads (§12.1): teamID scopes both queries
// exactly like ListWorkItems — a scoped Team sees only its own items; an empty
// teamID is the trusted fleet-admin path (ISI-3937).
package coord

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// canonicalStates is the §13 board enum (coord.work_item.state CHECK), in board
// order. Every snapshot carries a count for each — a zero is an honest "no items
// in this lane on this day", never an absent key, so the stacked-area chart
// renders a continuous band per status.
var canonicalStates = []string{"backlog", "todo", "in_progress", "in_review", "done"}

// StatusSnapshot is one day's point-in-time count of items in each state — the
// item's state AS OF the end of Date (UTC), reconstructed from its transition
// history. Counts is keyed by the §13 state enum and always carries every
// canonical state (zero-filled).
type StatusSnapshot struct {
	Date   string         `json:"date"` // YYYY-MM-DD (UTC; end-of-day snapshot)
	Counts map[string]int `json:"counts"`
}

// stateAt is one transition applied at an instant — the to_state the item
// entered at that time.
type stateAt struct {
	at    time.Time
	state string
}

// itemHistory is one work item's reconstructable timeline: it exists from
// createdAt in initialState, then each transition moves it. initialState is the
// from_state of its earliest transition (the state it was created into) or, for
// an item that never moved, its current state.
type itemHistory struct {
	createdAt    time.Time
	initialState string
	transitions  []stateAt // ascending by time
}

// stateAsOf returns the item's state at instant t, or "" if the item did not yet
// exist. The latest transition with at <= t wins; before any transition the item
// sits in initialState.
func (h itemHistory) stateAsOf(t time.Time) string {
	if t.Before(h.createdAt) {
		return ""
	}
	state := h.initialState
	for _, tr := range h.transitions {
		if tr.at.After(t) {
			break
		}
		state = tr.state
	}
	return state
}

// reconstructStatusSnapshots replays every item's history against each snapshot
// instant and counts items per state. It is pure (no DB, no clock) so the replay
// is unit-tested directly. instants are the per-day end-of-day boundaries; the
// Date label is the instant's UTC calendar day. An item counts under whatever
// state it holds at the instant; an item that does not yet exist is skipped.
func reconstructStatusSnapshots(items []itemHistory, instants []time.Time) []StatusSnapshot {
	out := make([]StatusSnapshot, 0, len(instants))
	for _, inst := range instants {
		counts := make(map[string]int, len(canonicalStates))
		for _, s := range canonicalStates {
			counts[s] = 0
		}
		for _, it := range items {
			st := it.stateAsOf(inst)
			if st == "" {
				continue
			}
			counts[st]++ // an off-enum state (should be impossible under the CHECK) still counts honestly
		}
		out = append(out, StatusSnapshot{Date: inst.In(time.UTC).Format("2006-01-02"), Counts: counts})
	}
	return out
}

// dailyInstants returns one end-of-day (23:59:59.999999999 UTC) instant per
// calendar day in [from, to], inclusive of both endpoints' days. The window is
// clamped/validated by the caller; here we just walk days.
func dailyInstants(from, to time.Time) []time.Time {
	from = from.In(time.UTC)
	to = to.In(time.UTC)
	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	last := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	var out []time.Time
	for !day.After(last) {
		out = append(out, day.Add(24*time.Hour-time.Nanosecond)) // end of this day
		day = day.Add(24 * time.Hour)
	}
	return out
}

// ProjectStatusSnapshots returns the daily per-status snapshot series for one
// Project over [from, to] (inclusive days). It is the read-model backing for the
// project-overview stacked-area chart (ISI-4509 / ISI-4505 S3). projectID is the
// coord.work_item.project_id (the Project CR UID, resolved by the apiserver);
// teamID scopes tenancy exactly like ListWorkItems (empty ⇒ fleet-admin).
//
// The two reads are bounded by created_at <= to: an item created after the
// window never affects any in-window snapshot, and a transition after the window
// is irrelevant to a snapshot that ends at `to`.
func (s *WorkItemReadStore) ProjectStatusSnapshots(ctx context.Context, teamID, projectID string, from, to time.Time) ([]StatusSnapshot, error) {
	if projectID == "" {
		return nil, fmt.Errorf("coord.ProjectStatusSnapshots: projectID required")
	}
	if to.Before(from) {
		return nil, fmt.Errorf("coord.ProjectStatusSnapshots: window end %s precedes start %s", to, from)
	}

	// ── item set: existence anchor (created_at) + fallback current state ──────
	itemRows, err := s.db.QueryContext(ctx, `
		SELECT id::text, created_at, state
		  FROM coord.work_item
		 WHERE project_id = $1::uuid
		   AND ($2::uuid IS NULL OR team_id = $2::uuid)
		   AND created_at <= $3`, projectID, nullUUID(teamID), to)
	if err != nil {
		return nil, fmt.Errorf("coord.ProjectStatusSnapshots: items %s: %w", projectID, err)
	}
	histories := map[string]*itemHistory{}
	order := []string{}
	func() {
		defer func() { _ = itemRows.Close() }()
		for itemRows.Next() {
			var id, state string
			var created time.Time
			if err = itemRows.Scan(&id, &created, &state); err != nil {
				return
			}
			histories[id] = &itemHistory{createdAt: created, initialState: state}
			order = append(order, id)
		}
		err = itemRows.Err()
	}()
	if err != nil {
		return nil, fmt.Errorf("coord.ProjectStatusSnapshots: scan items: %w", err)
	}
	if len(histories) == 0 {
		return reconstructStatusSnapshots(nil, dailyInstants(from, to)), nil
	}

	// ── transition log: every state_transition up to `to`, item-then-sequence ─
	trRows, err := s.db.QueryContext(ctx, `
		SELECT a.work_item_id::text, a.from_state, a.to_state, a.created_at
		  FROM coord.audit_log a
		  JOIN coord.work_item w ON w.id = a.work_item_id
		 WHERE w.project_id = $1::uuid
		   AND ($2::uuid IS NULL OR w.team_id = $2::uuid)
		   AND a.event_type = 'state_transition'
		   AND a.created_at <= $3
		 ORDER BY a.work_item_id, a.id`, projectID, nullUUID(teamID), to)
	if err != nil {
		return nil, fmt.Errorf("coord.ProjectStatusSnapshots: transitions %s: %w", projectID, err)
	}
	firstFromSet := map[string]bool{}
	func() {
		defer func() { _ = trRows.Close() }()
		for trRows.Next() {
			var wid, toState string
			var fromState sql.NullString
			var at time.Time
			if err = trRows.Scan(&wid, &fromState, &toState, &at); err != nil {
				return
			}
			h, ok := histories[wid]
			if !ok {
				continue // transition for an out-of-scope/after-window item
			}
			// The earliest transition's from_state is the state the item was
			// created into — a truer initial state than the current-state
			// fallback (which reflects ALL moves, including ones after `to`).
			if !firstFromSet[wid] {
				firstFromSet[wid] = true
				if fromState.Valid && fromState.String != "" {
					h.initialState = fromState.String
				}
			}
			h.transitions = append(h.transitions, stateAt{at: at, state: toState})
		}
		err = trRows.Err()
	}()
	if err != nil {
		return nil, fmt.Errorf("coord.ProjectStatusSnapshots: scan transitions: %w", err)
	}

	items := make([]itemHistory, 0, len(order))
	for _, id := range order {
		items = append(items, *histories[id])
	}
	// Transitions arrive ordered by (work_item_id, id); id is the monotonic audit
	// sequence, so per-item they are already time-ordered. Sort defensively in
	// case created_at (not id) is ever the intended order.
	for i := range items {
		sort.SliceStable(items[i].transitions, func(a, b int) bool {
			return items[i].transitions[a].at.Before(items[i].transitions[b].at)
		})
	}
	return reconstructStatusSnapshots(items, dailyInstants(from, to)), nil
}
