-- 0047_upward_handoff.sql — the WS-B upward hand-off spine marker table
-- (ISI-7956, plan rev 3).
--
-- WHY: on the run-completion seam the platform must PUSH the finishing
-- agent's manager a typed completion hand-off — push, not poll — exactly once
-- per (work item, run, kind). The marker row IS the payload: from_agent (the
-- finishing agent, coord.claim.assignee_agent), manager (the resolved manager
-- agent name, one WS-A reports-to level up), the closed kind enum, the small
-- HandoffDoc brief (pkg/coord/prodhandoff.go), and the push time.
--
-- At-most-once is STRUCTURAL (the a2asettle.go discipline): UNIQUE
-- (work_item_id, run_id, kind) + INSERT ... ON CONFLICT DO NOTHING, so a
-- re-entry (re-drive, duplicate settle, second terminal path) matches nothing
-- and nothing double-fires. run_id is NOT NULL because the push only exists
-- on the run-completion seam — a runless settle has nothing to key the marker
-- on (PushUpwardHandoff no-ops there), and a nullable key would break the
-- conflict guard (NULL never conflicts).
--
-- The kinds: 'done' (bare completion), 'ticket' (commits reported), 'pr'
-- (pull requests reported) — the run-completion mapping; 'decision' is
-- reserved for the WS-C mid-run decision requests that will reuse this spine.
--
-- Forward-only (the 001 discipline): a new table touches no existing row;
-- there is no down migration, a mistake is corrected by a new file.
CREATE TABLE coord.upward_handoff (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id uuid        NOT NULL REFERENCES coord.work_item(id) ON DELETE RESTRICT,
    run_id       uuid        NOT NULL,
    from_agent   text        NOT NULL,             -- the finishing agent (coord.claim.assignee_agent at settle time)
    manager      text        NOT NULL,             -- the manager agent name the push wakes
    kind         text        NOT NULL CHECK (kind IN ('done','ticket','pr','decision')),
    brief        jsonb       NOT NULL,             -- HandoffDoc (pkg/coord/prodhandoff.go)
    artifact_uri text,                             -- optional pointer at a registered artifact (v1: unused, NULL)
    pushed_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT upward_handoff_once UNIQUE (work_item_id, run_id, kind)
);

CREATE INDEX idx_upward_handoff_item ON coord.upward_handoff (work_item_id, pushed_at DESC);

