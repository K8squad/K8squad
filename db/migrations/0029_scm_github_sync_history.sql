-- 0029_scm_github_sync_history.sql — durable GitHub sync-history (ISI-5309, WS-D.2 of ISI-5279).
--
-- Forward-only, versioned migration (same discipline as 0001..0028: applied exactly once, in
-- filename order, by the apiserver migration runner / db.dbmigrate.Apply on startup; there is NO
-- down migration — a mistake is corrected by a new forward file).
--
-- WHAT THIS IS (ISI-5279 WS-D deliverable 2): a first-class, append-only history of every GitHub
-- SYNC/IMPORT pass, in the `scm` schema alongside scm.mirror_record / scm.issue_link / scm.repo
-- (GitHub-domain state). Until now the only durable trace of the sync loop was:
--   - scm.repo.last_mirror_update + Project.status.sync — the LATEST pass only (overwritten each
--     tick); no history, no "what did the last N passes do".
--   - the write-BACK relay's append-only 'github_writeback' markers in coord.audit_log — the
--     OUTBOUND (run-outcome → issue comment) direction only.
-- There was NO durable per-pass history for the INBOUND mirror/import direction, and no "last-sync
-- history" surface. This table is that history: one row per pass (webhook / poll mirror tick, or a
-- bridge issue IMPORT), recording WHEN, the pass KIND, the row COUNT, the optional per-issue REF,
-- the OUTCOME, and the honest PRINCIPAL that ran it.
--
-- MODELLED ON coord.audit_log (§6.5): a bigserial monotonic sequence + APPEND-ONLY enforcement
-- (UPDATE/DELETE/TRUNCATE all rejected by a trigger). A sync-history row is a FACT about a pass that
-- happened — never edited, never deleted. The trigger makes that structural, exactly like
-- coord.comment / coord.audit_log / coord.change_ref: a stray or compromised code path cannot
-- rewrite history; the write is rejected, not silently applied. The reject_mutation() function is
-- re-declared in the scm schema (rather than reusing coord.reject_mutation) so its error message
-- names the real schema+table via TG_TABLE_SCHEMA — honest provenance even in the failure path.
--
-- GUARDRAILS (ISI-5279): no secrets in the table. The columns are pass METADATA only — provider,
-- repo URL (public), kind, counts, issue ref, outcome, principal, and a structured `detail` jsonb
-- for count breakdowns / notes. The BYO credential is resolved and dropped in the operator; it has
-- no column here and never rides `detail`. `principal` is server-supplied (the operator SA / the
-- bridge principal), never client text — honest provenance, same posture as audit_log.principal.
--
-- FIELD OWNERSHIP: like scm.repo / scm.issue_link bookkeeping, this is written solely by the
-- coordination/sync path (the reposync relay, and later the ISI-5308 bridge import). It expresses no
-- coordination custody — there is no claim/lease/fence column — so it cannot take any (0008
-- discipline).

CREATE SCHEMA IF NOT EXISTS scm;

-- scm.reject_mutation — the scm-schema twin of coord.reject_mutation (0001). Generic over the
-- triggering table; the message names the real schema+table (TG_TABLE_SCHEMA/TG_TABLE_NAME) so an
-- append-only violation reports honestly which scm table it hit.
CREATE OR REPLACE FUNCTION scm.reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '%.% is append-only: % rejected (ISI-5309)', TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TABLE IF NOT EXISTS scm.github_sync_history (
    id                 bigserial    PRIMARY KEY,                 -- monotonic sync-history sequence
    project_namespace  varchar(253) NOT NULL,
    project_name       varchar(253) NOT NULL,
    provider           varchar(50)  NOT NULL DEFAULT 'github',   -- github (v1); behind the pkg/scm seam
    repo               varchar(512),                             -- repo URL (public identifier; never a token)
    -- kind is the pass TYPE. 'webhook'/'poll' are the reposync mirror triggers
    -- (scmmetrics.Trigger*); 'import' is the ISI-5308 bridge issue import; 'manual' is an
    -- operator/console "Sync now". Widening the set is a forward migration, never a silent widening.
    kind               varchar(20)  NOT NULL
                       CHECK (kind IN ('webhook','poll','import','manual','backfill')),
    -- Optional per-issue ref for an IMPORT pass (bare owner/repo#N, the mirror_record key space);
    -- NULL for a whole-repo mirror tick (which touches many records, not one issue).
    issue_ref          varchar(128),
    record_count       integer      NOT NULL DEFAULT 0,          -- rows the pass applied/imported
    -- outcome is the honest result: a pass that applied rows is 'success'; a pass that partially
    -- completed is 'partial'; a failed pass that still wanted an audit trail is 'failed'. The relay
    -- only records a row on a completed pass today, so 'success' dominates — the enum leaves room for
    -- the import path (ISI-5308) to record partials/failures without another migration.
    outcome            varchar(20)  NOT NULL DEFAULT 'success'
                       CHECK (outcome IN ('success','partial','failed')),
    principal          text         NOT NULL,                    -- server-supplied actor (operator SA / bridge); honest provenance
    detail             jsonb,                                    -- structured extras (count breakdown / note) — NEVER secrets
    synced_at          timestamptz  NOT NULL DEFAULT now(),      -- when the pass completed
    created_at         timestamptz  NOT NULL DEFAULT now()       -- when the row was written
);

-- Newest-first per-Project read (the "last-sync history" surface) + a provider/repo filter.
CREATE INDEX IF NOT EXISTS github_sync_history_project_idx
    ON scm.github_sync_history (project_namespace, project_name, id DESC);
CREATE INDEX IF NOT EXISTS github_sync_history_repo_idx
    ON scm.github_sync_history (repo);

-- Append-only enforcement (§6.5, same shape as coord.comment / coord.audit_log / coord.change_ref):
-- row triggers reject UPDATE/DELETE; a STATEMENT-level BEFORE TRUNCATE trigger closes the TRUNCATE
-- evasion (row triggers do not fire on TRUNCATE).
CREATE TRIGGER github_sync_history_append_only
    BEFORE UPDATE OR DELETE ON scm.github_sync_history
    FOR EACH ROW EXECUTE FUNCTION scm.reject_mutation();
CREATE TRIGGER github_sync_history_no_truncate
    BEFORE TRUNCATE ON scm.github_sync_history
    FOR EACH STATEMENT EXECUTE FUNCTION scm.reject_mutation();
