-- 0012_work_item_search.sql — Story 8.18 global search: Postgres full-text index over coord.work_item.
--
-- Forward-only, versioned migration (same discipline as 0001..0011). Applied exactly once, in
-- filename order, by the apiserver migration runner (§12.3). There is NO down migration — a mistake
-- is corrected by a new forward migration.
--
-- WHAT THIS IS: the search index the 8.18 global-search API (pkg/search) queries. Story 8.18 asks for
-- a global search API "per ADR-039 RBAC-scoped-in-query"; the only durable, human-authored corpus in
-- the coordination store is coord.work_item (title + body, 0001) — Projects/Teams/Runs are CRDs
-- (informer cache, not rows) and the audit_log / discussion streams are activity, not searchable
-- entities. So the search corpus is work items, and this migration makes them full-text searchable
-- WITHOUT a second store or an external engine (ADR-001: Postgres is the single source of truth; the
-- same "use the store we have, not a bespoke engine" posture Epic 6 took for pgvector).
--
-- WHY A GENERATED COLUMN (not a trigger, not query-time to_tsvector):
--   * A STORED generated column keeps the tsvector in lock-step with (title, body) structurally —
--     every INSERT/UPDATE recomputes it in the same txn, so the index can never drift from the row
--     (a hand-rolled AFTER trigger is the classic drift/UPDATE-storm footgun this avoids).
--   * It is GIN-indexable, so a query is an index scan, not a per-row to_tsvector recomputation.
--   * to_tsvector(regconfig, text) — the TWO-arg form with an explicit config — is IMMUTABLE, which
--     is the hard requirement for a generated-column expression. The one-arg form (default config)
--     is only STABLE and would be rejected here; pinning 'english' is both required and correct.
--
-- WEIGHTING (ADR-039 relevance): the title is the human-authored summary of the item, the body its
-- detail — so title terms outrank body terms. setweight tags title 'A' and body 'B'; the pkg/search
-- ranker (ts_rank_cd with the default {0.1,0.2,0.4,1.0} D<C<B<A weights) then floats a title hit
-- above a body-only hit for the same term. Language is 'english' (the console/UX language, ADR-039);
-- a per-Team language is a future migration, not a v1 axis.
--
-- TENANCY / RBAC (ADR-039 "scoped in query", NOT in this migration): this index is tenancy-blind by
-- design — the RBAC scope (team_id = caller's Team, admin = fleet-wide) is applied as a WHERE
-- predicate by pkg/search on every query, alongside the FTS match. The GIN index accelerates the
-- @@ match; the existing idx_work_item_project / the row's team_id carry the scope. Keeping the
-- scope in the query (not baked into a per-Team index) is the ADR-039 contract and matches how
-- coord.HumanStateStore / the dashboard already fence tenancy (§12.1).

-- ---------------------------------------------------------------------------
-- search_tsv — STORED generated full-text vector over (title, body).
-- ---------------------------------------------------------------------------
ALTER TABLE coord.work_item
    ADD COLUMN search_tsv tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(body,  '')), 'B')
    ) STORED;

-- FTS hot path: the @@ tsquery match rides this GIN index (the search corpus scan). The tenancy
-- predicate (team_id = $caller) narrows within it; there is no per-Team index (ADR-039 scope-in-query).
CREATE INDEX idx_work_item_search ON coord.work_item USING GIN (search_tsv);
