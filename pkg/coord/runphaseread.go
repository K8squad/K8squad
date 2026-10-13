// runphaseread.go — WS-3 of ISI-5744 (ISI-5750; ADR-0032): the READ side of the
// Run phase-transition journal — the daily per-run-phase snapshot reconstruction
// that backs the fleet overview's "Runs by status over time · 30d" stacked-area
// graph (ISI-5738 §3).
//
// It is the exact run-side mirror of the tickets-by-status reconstruction
// (overviewread.go / FleetStatusSnapshots): a pure point-in-time replay over an
// append-only history, counted into daily buckets. The ONLY differences from the
// tickets path are the source table (coord.run_phase_transition, the WS-1 journal
// — db/migrations/0037) and the closed state set (the four run-status chip tokens
// canonicalRunPhases, not the §13 board enum). The replay primitive
// (reconstructStatusSnapshots) and the daily-bucket boundaries (dailyInstants)
// are reused verbatim.
//
// WHY THE JOURNAL IS THE ONLY SOURCE (ADR-0032): a Run's phase lives only on the
// Run CR in etcd (the live value, overwritten each transition), so — unlike board
// items, whose creation row + current state give a fallback timeline — a Run has
// NO reconstructable history outside this journal. A run therefore first appears
// in the series at its earliest journal row (the earliest transition we observed)
// and in that row's to_phase; we never back-date to a from_phase we have no
// timestamp for. This is the honest earliest-known state, matching the FR-I3
// "never fabricate a band" discipline the degradable tile documents.
//
// TENANCY mirrors FleetStatusSnapshots exactly: teamID scopes
// coord.run_phase_transition.team_id (empty ⇒ trusted fleet-admin, ISI-3937); the
// shared helper also takes a projectID for a future per-Project reuse (the 0037
// project_ts index exists for it), empty ⇒ no project filter.
package coord

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// canonicalRunPhases is the closed set of run-status chip tokens (the four
// coord.run_phase_transition.to_phase values, == pkg/coord RunPhase* constants ==
// internal/apiserver RunsSeriesTile bucketing), in display order. Every run-phase
// snapshot zero-fills all four so the stacked-area chart renders a continuous
// band per phase — a zero is an honest "no runs in this phase on this day", never
// an absent key. It plays the exact role canonicalStates plays for the board enum.
var canonicalRunPhases = []string{
	RunPhaseClaiming, // "claiming"
	RunPhaseRunning,  // "running"
	RunPhaseComplete, // "complete"
	RunPhaseFailed,   // "failed"
}

// runPhaseRow is one scanned journal edge: the Run's phase AS OF ts became
// toPhase. from_phase is deliberately dropped on read — the series counts the
// phase a run SITS in at each instant, and the run's earliest row already
// establishes its earliest-known phase (see the file header on back-dating).
type runPhaseRow struct {
	runUID  string
	toPhase string
	at      time.Time
}

// runHistories groups journal rows into one replayable itemHistory per Run,
// reusing the tickets-path itemHistory/stateAsOf machinery verbatim. It is pure
// (no DB, no clock) so the grouping + daily replay is unit-tested without a
// database, exactly like reconstructStatusSnapshots.
//
// Each run's timeline is its edges in chronological order: it exists from its
// earliest edge's ts (createdAt) in that edge's to_phase (initialState), and each
// subsequent edge moves it. Rows arrive ordered by (run_uid, ts, id) from SQL;
// we re-sort each run's edges by ts defensively (stable, so same-ts ties keep the
// monotonic-id order the query already applied).
func runHistories(rows []runPhaseRow) []itemHistory {
	byRun := map[string]*itemHistory{}
	order := []string{}
	for _, r := range rows {
		h, ok := byRun[r.runUID]
		if !ok {
			h = &itemHistory{}
			byRun[r.runUID] = h
			order = append(order, r.runUID)
		}
		h.transitions = append(h.transitions, stateAt{at: r.at, state: r.toPhase})
	}
	out := make([]itemHistory, 0, len(order))
	for _, id := range order {
		h := byRun[id]
		sort.SliceStable(h.transitions, func(a, b int) bool {
			return h.transitions[a].at.Before(h.transitions[b].at)
		})
		// A run exists from its earliest observed edge, in that edge's to_phase.
		// Including the first edge in transitions too is harmless: stateAsOf
		// re-applies it to the same value at createdAt.
		h.createdAt = h.transitions[0].at
		h.initialState = h.transitions[0].state
		out = append(out, *h)
	}
	return out
}

// FleetRunPhaseSnapshots returns the daily per-run-phase snapshot series across
// EVERY Run in scope over [from, to] (inclusive days) — the fleet/Team-wide
// read-model backing the ISI-5738 §3 "Runs by status over time" graph. It is the
// run-side generalization of FleetStatusSnapshots: empty teamID ⇒ trusted
// fleet-admin (every Run's edges), non-empty ⇒ one Team's Runs
// (coord.run_phase_transition.team_id). It satisfies
// internal/apiserver.FleetRunPhaseHistorySource — the production wiring of the
// previously-nil run-phase seam (cmd/apiserver/main.go).
func (s *WorkItemReadStore) FleetRunPhaseSnapshots(ctx context.Context, teamID string, from, to time.Time) ([]StatusSnapshot, error) {
	return s.runPhaseSnapshots(ctx, teamID, "", from, to)
}

// runPhaseSnapshots is the shared reconstruction behind FleetRunPhaseSnapshots
// (and a future per-Project variant). projectID scopes to one Project
// (coord.run_phase_transition.project_id) when non-empty; empty ⇒ no project
// filter. teamID scopes tenancy exactly like statusSnapshots (empty ⇒
// fleet-admin). The single read is bounded by ts <= to: an edge after the window
// cannot affect a snapshot that ends at `to`, while an edge BEFORE the window is
// kept — it establishes the phase a long-lived run carries INTO the window (no
// from bound, mirroring the tickets path).
func (s *WorkItemReadStore) runPhaseSnapshots(ctx context.Context, teamID, projectID string, from, to time.Time) ([]StatusSnapshot, error) {
	if to.Before(from) {
		return nil, fmt.Errorf("coord.runPhaseSnapshots: window end %s precedes start %s", to, from)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT run_uid::text, to_phase, ts
		  FROM coord.run_phase_transition
		 WHERE ($1::uuid IS NULL OR project_id = $1::uuid)
		   AND ($2::uuid IS NULL OR team_id = $2::uuid)
		   AND ts <= $3
		 ORDER BY run_uid, ts, id`, nullUUID(projectID), nullUUID(teamID), to)
	if err != nil {
		return nil, fmt.Errorf("coord.runPhaseSnapshots: query %s: %w", projectID, err)
	}
	var scanned []runPhaseRow
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r runPhaseRow
			if err = rows.Scan(&r.runUID, &r.toPhase, &r.at); err != nil {
				return
			}
			scanned = append(scanned, r)
		}
		if err == nil {
			err = rows.Err()
		}
	}()
	if err != nil {
		return nil, fmt.Errorf("coord.runPhaseSnapshots: scan: %w", err)
	}

	return reconstructStatusSnapshots(runHistories(scanned), dailyInstants(from, to), canonicalRunPhases), nil
}
