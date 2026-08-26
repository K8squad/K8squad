-- 0012_work_item_search_test.sql — runnable self-check for Story 8.18 work-item full-text search.
--
-- No framework, no fixture: plain SQL that fails loudly if the search-index invariants break. In CI
-- (db / migrations self-check) it runs against a throwaway Postgres AFTER THE FULL migration set is
-- applied (0001..000N in filename order):
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0012_work_item_search_test.sql
--
-- Everything runs inside one transaction that is ROLLED BACK at the end, so the check leaves no
-- residue. Any failed assertion aborts with a non-zero exit (ON_ERROR_STOP). This file proves the
-- *structural* contract (the generated tsvector exists, stays in lock-step with title/body, weights
-- title above body, and the GIN index backs @@); the RBAC scoping + ranking + snippet path is
-- exercised by the Go unit/integration tests (pkg/search, internal/apiserver search tests).

BEGIN;

-- The generated column exists on coord.work_item and is a tsvector.
DO $$
DECLARE t text;
BEGIN
    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='work_item' AND column_name='search_tsv';
    ASSERT t = 'tsvector', format('expected coord.work_item.search_tsv tsvector, found %s', coalesce(t,'<absent>'));
END $$;

-- It is a GENERATED column (drift-proof): a plain column that a trigger fills would fail this.
DO $$
DECLARE g text;
BEGIN
    SELECT is_generated INTO g FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='work_item' AND column_name='search_tsv';
    ASSERT g = 'ALWAYS', format('expected search_tsv GENERATED ALWAYS, found %s', coalesce(g,'<absent>'));
END $$;

-- A GIN index backs the FTS match.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM pg_indexes
     WHERE schemaname='coord' AND tablename='work_item' AND indexname='idx_work_item_search';
    ASSERT n = 1, format('expected GIN index idx_work_item_search, found %s', n);
END $$;

-- Seed two items: one whose TITLE carries the term, one whose BODY carries it, one unrelated.
INSERT INTO coord.work_item (id, project_id, team_id, title, body, created_by) VALUES
  ('20000000-0000-0000-0000-000000000001', gen_random_uuid(), gen_random_uuid(),
   'Fix checkout latency', 'unrelated detail', 'admin'),
  ('20000000-0000-0000-0000-000000000002', gen_random_uuid(), gen_random_uuid(),
   'Unrelated title', 'the checkout flow needs work', 'admin'),
  ('20000000-0000-0000-0000-000000000003', gen_random_uuid(), gen_random_uuid(),
   'Billing rollup', 'nothing to see', 'admin');

-- The vector is populated (not NULL) from title+body.
DO $$
DECLARE nz int;
BEGIN
    SELECT count(*) INTO nz FROM coord.work_item
     WHERE id IN ('20000000-0000-0000-0000-000000000001','20000000-0000-0000-0000-000000000002')
       AND search_tsv IS NOT NULL AND search_tsv <> '';
    ASSERT nz = 2, format('expected 2 populated search vectors, found %s', nz);
END $$;

-- @@ match finds BOTH the title hit and the body hit for 'checkout', and NOT the unrelated row.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM coord.work_item
     WHERE search_tsv @@ to_tsquery('english','checkout')
       AND id::text LIKE '20000000-%';
    ASSERT n = 2, format('expected 2 checkout matches, found %s', n);
END $$;

-- Weighting: a TITLE hit outranks a BODY-only hit for the same term (setweight A > B).
DO $$
DECLARE title_rank real; body_rank real;
BEGIN
    SELECT ts_rank_cd(search_tsv, to_tsquery('english','checkout')) INTO title_rank
      FROM coord.work_item WHERE id = '20000000-0000-0000-0000-000000000001';
    SELECT ts_rank_cd(search_tsv, to_tsquery('english','checkout')) INTO body_rank
      FROM coord.work_item WHERE id = '20000000-0000-0000-0000-000000000002';
    ASSERT title_rank > body_rank,
        format('expected title hit (%s) to outrank body hit (%s)', title_rank, body_rank);
END $$;

-- Lock-step: an UPDATE to body re-derives the vector (no trigger, no drift) — the term now matches.
UPDATE coord.work_item SET body = 'now mentions refund'
 WHERE id = '20000000-0000-0000-0000-000000000003';
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM coord.work_item
     WHERE id = '20000000-0000-0000-0000-000000000003'
       AND search_tsv @@ to_tsquery('english','refund');
    ASSERT n = 1, 'expected the regenerated vector to match the updated body term "refund"';
END $$;

ROLLBACK;
