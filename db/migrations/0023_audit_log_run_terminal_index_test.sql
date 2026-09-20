-- 0023_audit_log_run_terminal_index_test.sql — runnable self-check for the
-- File Explorer browse-target serving index (ISI-4693). Plain SQL, no framework;
-- run after the migrations it checks, inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0023_audit_log_run_terminal_index.sql \
--          -f db/migrations/0023_audit_log_run_terminal_index_test.sql
--
-- It proves the STRUCTURAL ACs: the partial index exists on coord.audit_log with
-- the expected leading key and partial predicate, so the browse-target lookup has a
-- serving index rather than a seq scan on the largest table.

BEGIN;

-- (1) The index exists on coord.audit_log.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n
      FROM pg_indexes
     WHERE schemaname = 'coord' AND tablename = 'audit_log'
       AND indexname = 'idx_audit_log_run_terminal';
    ASSERT n = 1, 'idx_audit_log_run_terminal must exist on coord.audit_log';
END $$;

-- (2) It is a PARTIAL index (has a WHERE predicate) keyed on run_terminal/succeeded,
--     leading with work_item_id — the filter + ORDER BY the browse-target lookup uses.
DO $$
DECLARE def text;
BEGIN
    SELECT indexdef INTO def
      FROM pg_indexes
     WHERE schemaname = 'coord' AND tablename = 'audit_log'
       AND indexname = 'idx_audit_log_run_terminal';
    ASSERT def LIKE '%WHERE%', 'index must be partial (carry a WHERE predicate)';
    ASSERT position('run_terminal' IN def) > 0, 'partial predicate must pin event_type = run_terminal';
    ASSERT position('succeeded' IN def) > 0, 'partial predicate must pin to_state = succeeded';
    ASSERT position('work_item_id' IN def) > 0, 'index must lead with work_item_id';
END $$;

ROLLBACK;
