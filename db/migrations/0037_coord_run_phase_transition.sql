-- 0037_coord_run_phase_transition.sql — the durable RUN PHASE-TRANSITION JOURNAL
-- (ISI-5746, WS-1 of ISI-5744; ADR-0032, approved by Henrik via confirmation e69fae04).
--
-- Forward-only, versioned migration (same discipline as 0001..0036: applied exactly once, in
-- filename order, by the apiserver migration runner / db.dbmigrate.Apply on startup; there is NO
-- down migration — a mistake is corrected by a new forward file. HEAD was 0036 per ISI-5732).
--
-- WHY THIS TABLE EXISTS (ADR-0032 / ISI-5744):
-- The fleet overview's "Runs by status over time · 30d" stacked-area graph (ISI-5738 §3) needs a
-- true TIME SERIES of how many Runs sat in each status on each day. Unlike the tickets-by-status
-- series — which reconstructs point-in-time snapshots from the existing append-only
-- coord.audit_log `state_transition` rows (see pkg/coord/overviewread.go) — the RUN side has no
-- such history: a Run's phase lives ONLY on the Run CR in etcd (the live value, overwritten on
-- every transition), and coord.outbox carries only `thinking` progress events (ProgressMirror
-- ISI-5193), never phase edges. So there was no in-cluster source to reconstruct the series from.
-- This table is that source: an append-only journal of every Run phase transition, mirroring the
-- tickets path (one immutable row per transition, replayed into daily buckets by the WS-3
-- reconstruction over the SAME reconstructStatusSnapshots pure projection).
--
-- APPEND-ONLY, modelled on coord.audit_log (§6.5) / coord.change_ref (0015): a bigserial monotonic
-- sequence + a BEFORE UPDATE/DELETE row trigger and a BEFORE TRUNCATE statement trigger (TRUNCATE
-- does not fire row triggers — the F2 evasion), reusing the coord.reject_mutation() function
-- declared in 0001. A transition row is a FACT about a phase edge that happened — never edited,
-- never deleted. There is deliberately NO update/delete path in pkg/coord either (WS-1 ships only
-- RecordRunPhaseTransition, a plain INSERT); the trigger makes that structural.
--
-- STATUS ENUM — the four run-status CHIP TOKENS the FE renders, lowercased: claiming | running |
-- complete | failed (match internal/apiserver RunsSeriesTile / RunsByStatusTile and the FE chip
-- bucketing; see squadseries.go). The journal stores the NORMALIZED display token, not the raw CR
-- phase (Claiming/Running/Succeeded/Failed/…): the WS-2 operator emitter maps CR phase → chip token
-- so WS-3's reconstruction keys line up with coord.StatusSnapshot.Counts with no re-mapping.
-- from_phase is NULL on a Run's first observed transition (nothing was left yet), exactly like
-- coord.audit_log.from_state on a first state_transition. Widening the set is a forward migration,
-- never a silent widening.
--
-- TENANCY — team_id / project_id are the Run's OWNING Team / Project CR UIDs, derived from the Run
-- CR (NOT from any linked work_item: not every Run has one — WS-2 concern, but the column names and
-- meaning are pinned here). They mirror coord.work_item.(team_id, project_id) so the WS-3 read
-- reuses the same `($n::uuid IS NULL OR team_id = $n::uuid)` tenancy predicate as the tickets path
-- (empty/NULL param ⇒ trusted fleet-admin, ISI-3937). team_id is nullable (a Run whose Team is
-- unresolved still records an honest row; a scoped read simply won't match it, fleet-admin still
-- sees it), project_id is required.
--
-- NOT a custody surface: this expresses no coordination claim/lease/fence (0008 discipline) — it is
-- pure observability history, like coord.change_ref.

CREATE TABLE IF NOT EXISTS coord.run_phase_transition (
    id         bigserial   PRIMARY KEY,                 -- monotonic journal sequence (same-ts tie-break for replay)
    run_uid    uuid        NOT NULL,                    -- Run CR metadata.uid (the per-run replay key)
    team_id    uuid,                                    -- owning Team CR UID (NULL ⇒ unresolved; fleet-admin still sees it)
    project_id uuid        NOT NULL,                    -- owning Project CR UID (tenancy predicate, §12.1)
    -- from_phase is the token the Run is LEAVING; NULL on its first observed transition.
    from_phase text        CHECK (from_phase IS NULL OR from_phase IN ('claiming','running','complete','failed')),
    -- to_phase is the token the Run now sits in; one of the four chip tokens.
    to_phase   text        NOT NULL
               CHECK (to_phase IN ('claiming','running','complete','failed')),
    ts         timestamptz NOT NULL DEFAULT now(),      -- when the transition was observed (operator-supplied; defaults to now())
    created_at timestamptz NOT NULL DEFAULT now()       -- when the row was written
);

-- Tenancy + daily-bucket scan: the WS-3 fleet read walks (team_id, ts) windows to bucket by day.
CREATE INDEX IF NOT EXISTS run_phase_transition_team_ts_idx
    ON coord.run_phase_transition (team_id, ts);
-- Project-scoped variant of the same daily-bucket scan (per-Project overview reuse).
CREATE INDEX IF NOT EXISTS run_phase_transition_project_ts_idx
    ON coord.run_phase_transition (project_id, ts);
-- Per-run replay ordering: reconstruction groups a Run's edges in chronological order (ts, then the
-- monotonic id to break same-instant ties).
CREATE INDEX IF NOT EXISTS run_phase_transition_run_idx
    ON coord.run_phase_transition (run_uid, ts, id);

-- Append-only enforcement (§6.5, same shape as coord.comment / coord.audit_log / coord.change_ref):
-- row trigger rejects UPDATE/DELETE; a STATEMENT-level BEFORE TRUNCATE trigger closes the TRUNCATE
-- evasion. Reuses coord.reject_mutation() from 0001 — the error message names the table honestly.
CREATE TRIGGER run_phase_transition_append_only
    BEFORE UPDATE OR DELETE ON coord.run_phase_transition
    FOR EACH ROW EXECUTE FUNCTION coord.reject_mutation();
CREATE TRIGGER run_phase_transition_no_truncate
    BEFORE TRUNCATE ON coord.run_phase_transition
    FOR EACH STATEMENT EXECUTE FUNCTION coord.reject_mutation();
