-- 0023_audit_log_run_terminal_index.sql — serving index for the File Explorer
-- browse-target lookup (ISI-4693, cursor review F1 on PR #520).
--
-- Forward-only companion to 0001_coord_schema.sql. coord.audit_log carries only
-- (work_item_id, id) and (run_id, id) indexes — nothing on event_type, to_state
-- or created_at — and it is the fastest-growing table in the schema (a row per
-- claim acquire/renew/release, never pruned). CoordReaderSpecResolver.latestBrowseTarget
-- (internal/apiserver/readerspec.go) resolves a project's LATEST completed run with:
--
--     WHERE event_type = 'run_terminal' AND to_state = 'succeeded'
--     ORDER BY created_at DESC, id DESC LIMIT 1
--
-- joined per project work item. Without a matching index that is O(all coordination
-- events for the project). This partial index collapses to ~one row per succeeded
-- run and matches the filter + deterministic ORDER BY exactly.
--
-- Non-exclusive: CREATE INDEX CONCURRENTLY cannot run inside the transaction the
-- migration runner wraps each file in, and File Explorer read volume is negligible,
-- so a plain CREATE INDEX IF NOT EXISTS is the right trade (mirrors 0008).

CREATE INDEX IF NOT EXISTS idx_audit_log_run_terminal
    ON coord.audit_log (work_item_id, created_at DESC, id DESC)
    WHERE event_type = 'run_terminal' AND to_state = 'succeeded';
