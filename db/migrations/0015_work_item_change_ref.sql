-- 0015_work_item_change_ref.sql — M1.5 agent change reporting (ISI-4131).
--
-- Forward-only, versioned migration (same discipline as 0001: applied exactly
-- once, in filename order, by the apiserver migration runner; no down migration).
--
-- M1.5 closes the reporting half of the core loop: the agent's ticket must show
-- agent-authored comments + status changes + CHANGE REFS (commit SHAs / PR
-- links) without human relay (epic ISI-4126). Comments (coord.comment) and
-- status transitions (work_item.state + audit_log) already had sanctioned
-- agent write paths; the change summary had NO surface at all. This is it.
--
-- Design notes:
--   * Append-only history, exactly like coord.comment (§6.1/§6.5): a change ref
--     is a fact the run reported, never edited or deleted. The shared
--     coord.reject_mutation trigger makes that structural.
--   * NOT folded into coord.artifact: artifact is content-addressed OUTPUT
--     (sha256 NOT NULL, UNIQUE (work_item_id, run_id, kind) — one row per kind
--     per run). A run reports MANY commits/PRs and a PR link carries no sha;
--     forcing that shape here would lie about both.
--   * kind is a tight enum ('commit' | 'pull_request') — the epic's "commits/PR
--     links". Widening is a forward migration, never a silent widening.
--   * run_id is nullable provenance (the reporting Run), like audit_log.run_id;
--     author_principal is the server-supplied token principal, never client
--     text.

CREATE TABLE coord.change_ref (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id     uuid        NOT NULL REFERENCES coord.work_item(id) ON DELETE RESTRICT,  -- history pins the item
    run_id           uuid,                                                            -- reporting Run (provenance)
    kind             text        NOT NULL CHECK (kind IN ('commit','pull_request')),
    ref              text        NOT NULL,                                             -- commit SHA or PR URL
    summary          text,                                                            -- one-line what-changed note
    author_principal text        NOT NULL,                                             -- server-supplied (token principal)
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_change_ref_work_item ON coord.change_ref (work_item_id, created_at);
CREATE INDEX idx_change_ref_run       ON coord.change_ref (run_id);

-- Append-only enforcement (§6.5, same trigger as comment/audit_log).
CREATE TRIGGER change_ref_append_only
    BEFORE UPDATE OR DELETE ON coord.change_ref
    FOR EACH ROW EXECUTE FUNCTION coord.reject_mutation();
CREATE TRIGGER change_ref_no_truncate
    BEFORE TRUNCATE ON coord.change_ref
    FOR EACH STATEMENT EXECUTE FUNCTION coord.reject_mutation();
