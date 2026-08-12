-- 0001_coord_schema.sql — Coordination Record schema (Arch §6.1, Story 2.1 / ISI-2191)
--
-- Forward-only, versioned migration. Applied exactly once, in filename order, by the
-- apiserver migration runner on startup (mirrors the `auth` schema discipline, §12.3).
-- There is NO down migration — the spine is forward-only by contract (Story 2.1 AC).
--
-- This is the single most correctness-critical component of v1 (PRD R10 / Arch §6). The
-- structure here — not application discipline alone — enforces the spine's invariants:
--   * exactly one active claim row per work item        (F3: claim PK + auto-provision trigger)
--   * monotonic, never-reused fence token                (§6.1/§6.2, bumped by the acquire UPDATE)
--   * append-only comment + audit_log history            (§6.5, reject_mutation trigger)
--   * artifact idempotency under re-entry                (§6.4, UNIQUE upsert key)
--   * orphans-as-roots for sub-tickets                   (§6.1, parent_id ON DELETE SET NULL)
--   * NO agent-to-agent channel                          (I4/no-P2P: there is no `message` table)
--
-- Naming note (architect reconciliation of Story 2.1 wording ↔ Arch §6.1 authority):
--   Story 2.1 lists the tables loosely as `work_items / comments / artifacts / checkouts /
--   run_events|audit_log`. The AUTHORITATIVE names are §6.1's (singular, `coord` schema), and
--   the executable mechanism SQL in §6.2/§6.4 targets them verbatim (`UPDATE claim … WHERE
--   work_item_id`). So the durable names are: work_item, comment, artifact, claim, audit_log.
--   Mapping: checkouts ≡ claim ; audit_log ≡ the §6.5 coord audit ; "lease_epoch" ≡ fence_token.
--   The Story's "run_events" (high-volume shim trace, §10.1) is a SEPARATE `run_trace` table added
--   by Story 8.11's forward migration — see the audit_log block below (ISI-2339 F1 decision).

CREATE SCHEMA IF NOT EXISTS coord;

-- gen_random_uuid() is a core function since PG13 (CNPG ships ≥13) — no pgcrypto extension needed,
-- so the migration runs under a least-privilege app role without CREATE EXTENSION rights.

-- ---------------------------------------------------------------------------
-- work_item — the durable unit of coordination (§6.1)
-- ---------------------------------------------------------------------------
CREATE TABLE coord.work_item (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     uuid        NOT NULL,                       -- tenancy predicate (§12.1)
    team_id        uuid,                                        -- child inherits parent's (§6.1)
    parent_id      uuid        REFERENCES coord.work_item(id) ON DELETE SET NULL,  -- adjacency list; orphan → root
    title          text        NOT NULL,
    body           text,
    -- Canonical ordered board enum (§13 board-derivation, r25). The Kanban board is a PROJECTION
    -- of this column, never a second stored column. `blocked` is NOT a state — it is an orthogonal
    -- condition (blocked_reason), so an item never leaves its lane to become blocked (§8.6).
    state          text        NOT NULL DEFAULT 'backlog'
                               CHECK (state IN ('backlog','todo','in_progress','in_review','done')),
    blocked_reason text,                                        -- NULL = not blocked; e.g. 'needs_approval' (2.12/8.8c)
    created_by     text        NOT NULL,                        -- principal
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT work_item_no_self_parent CHECK (parent_id IS NULL OR parent_id <> id)
);
-- Deeper cycle rejection (parent must not be a descendant) is a bounded ancestor-walk in the
-- reconciler on write (§6.1) — not a recursive DB trigger. The cheap self-parent CHECK is here;
-- the walk lives in pkg/coord. ponytail: full cycle detection in SQL = recursive trigger, not worth
-- it at this tree size — upgrade path is a WITH RECURSIVE guard in the write path if reparenting churns.

CREATE INDEX idx_work_item_project ON coord.work_item (project_id);
CREATE INDEX idx_work_item_parent  ON coord.work_item (parent_id) WHERE parent_id IS NOT NULL;  -- "children of X" (§13 lazy-load)
CREATE INDEX idx_work_item_state   ON coord.work_item (project_id, state);                      -- board / filters (§13)
CREATE INDEX idx_work_item_updated ON coord.work_item (project_id, updated_at DESC);            -- List sort key (§13)

-- ---------------------------------------------------------------------------
-- comment — append-only, provenanced progress/handoff notes (§6.1/§6.5)
-- ---------------------------------------------------------------------------
CREATE TABLE coord.comment (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id    uuid        NOT NULL REFERENCES coord.work_item(id) ON DELETE RESTRICT,  -- history pins the item
    author_principal text       NOT NULL,
    body            text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_comment_work_item ON coord.comment (work_item_id, created_at);

-- ---------------------------------------------------------------------------
-- artifact — content-addressed outputs; upsert-keyed for re-entry idempotency (§6.1/§6.4)
-- ---------------------------------------------------------------------------
CREATE TABLE coord.artifact (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id uuid        NOT NULL REFERENCES coord.work_item(id) ON DELETE RESTRICT,
    run_id       uuid        NOT NULL,
    kind         text        NOT NULL,
    uri          text        NOT NULL,
    sha256       text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT artifact_upsert_key UNIQUE (work_item_id, run_id, kind)  -- re-entered Collecting republishes, never duplicates
);

-- ---------------------------------------------------------------------------
-- claim (≡ Story "checkouts") — exactly ONE active claim row per work item (§6.1/§6.2, F3)
-- ---------------------------------------------------------------------------
-- The row is rewritten IN PLACE on every (re)claim; fence_token is monotonically increasing across
-- the item's lifetime (never reset, never reused). No append-only claim history in the custody path
-- (history lives in audit_log / the outbox), so two live leases on one item are structurally
-- impossible. holder_principal IS NULL ⇒ unclaimed / reclaimable.
CREATE TABLE coord.claim (
    work_item_id         uuid        PRIMARY KEY REFERENCES coord.work_item(id) ON DELETE CASCADE,
    holder_principal     text,
    run_id               uuid,
    fence_token          bigint      NOT NULL DEFAULT 0,   -- "lease_epoch" in Story 2.1 wording
    lease_expires_at     timestamptz,
    acquired_at          timestamptz,
    renewed_at           timestamptz,
    initiated_by_user_id uuid,                             -- non-forgeable, control-plane-stamped (§12.4)
    CONSTRAINT claim_fence_nonneg CHECK (fence_token >= 0)
);
CREATE INDEX idx_claim_lease ON coord.claim (lease_expires_at);  -- reclaim scan for expired leases (§6.3)

-- Every work_item gets exactly one claim row at creation, unheld (holder NULL, fence 0). This makes
-- the §6.2 acquire a plain conditional UPDATE (the row always pre-exists) and makes "exactly one
-- claim row per item" a structural invariant, not an application obligation.
CREATE FUNCTION coord.provision_claim() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO coord.claim (work_item_id) VALUES (NEW.id);
    RETURN NEW;
END;
$$;
CREATE TRIGGER work_item_provision_claim
    AFTER INSERT ON coord.work_item
    FOR EACH ROW EXECUTE FUNCTION coord.provision_claim();

-- ---------------------------------------------------------------------------
-- audit_log (≡ Story "run_events / audit_log") — immutable coordination audit (§6.5)
-- ---------------------------------------------------------------------------
-- ARCHITECT DECISION (ISI-2339 F1, resolving the run_event-unification concern):
-- This table holds ONLY the low-volume, immutable COORDINATION audit — every checkout / comment /
-- artifact / completion, with principal + timestamp (§6.5). It is append-only and its bigserial id
-- is a clean monotonic audit sequence (no interleaving with trace noise). The HIGH-VOLUME shim Run
-- trace firehose (tool_call | llm_call | build_output | error, §10.1) does NOT live here — it lands
-- in a separate, TIME-PARTITIONED, retention-managed `coord.run_trace` table added by a forward
-- migration owned by Story 8.11 (retention via `DROP PARTITION`, NOT the append-only trigger, so the
-- firehose is prunable WITHOUT contradicting this table's structural immutability). Splitting the two
-- keeps the audit immutable + monotonic and the trace prunable — the two have different volume and
-- retention semantics and must not share a table. (The Story's slash-name "run_events/audit_log"
-- foreshadowed exactly this split: audit_log = the audit half; run_trace = the run-events half.)
CREATE TABLE coord.audit_log (
    id                   bigserial   PRIMARY KEY,   -- monotonic audit sequence (coord events only)
    work_item_id         uuid        REFERENCES coord.work_item(id) ON DELETE RESTRICT,
    run_id               uuid,
    event_type           text        NOT NULL,   -- claim_acquired|claim_renewed|claim_released|
                                                  -- comment_added|artifact_registered|state_transition|completed
    principal            text        NOT NULL,    -- who acted (§6.5)
    initiated_by_user_id uuid,                    -- on whose behalf (§12.4)
    fence_token          bigint,                  -- fence under which the coord event occurred
    from_state           text,                    -- state_transition provenance
    to_state             text,
    payload              jsonb,                   -- structured detail for the coord event
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_log_work_item ON coord.audit_log (work_item_id, id);
CREATE INDEX idx_audit_log_run       ON coord.audit_log (run_id, id);

-- ---------------------------------------------------------------------------
-- Append-only enforcement (Story 2.1 AC: comments and the audit log are append-only)
-- ---------------------------------------------------------------------------
-- The AC pins "no UPDATE/DELETE path in code". These triggers make that STRUCTURAL — a stray or
-- compromised code path cannot mutate history; the write is rejected, never silently applied. Row
-- triggers cover UPDATE/DELETE; a STATEMENT-level BEFORE TRUNCATE trigger closes the TRUNCATE evasion
-- (ISI-2339 F2 — row triggers do NOT fire on TRUNCATE, so an un-guarded table could be wiped whole).
CREATE FUNCTION coord.reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'coord.% is append-only: % rejected (§6.5)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER comment_append_only
    BEFORE UPDATE OR DELETE ON coord.comment
    FOR EACH ROW EXECUTE FUNCTION coord.reject_mutation();
CREATE TRIGGER comment_no_truncate
    BEFORE TRUNCATE ON coord.comment
    FOR EACH STATEMENT EXECUTE FUNCTION coord.reject_mutation();

CREATE TRIGGER audit_log_append_only
    BEFORE UPDATE OR DELETE ON coord.audit_log
    FOR EACH ROW EXECUTE FUNCTION coord.reject_mutation();
CREATE TRIGGER audit_log_no_truncate
    BEFORE TRUNCATE ON coord.audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION coord.reject_mutation();

-- ---------------------------------------------------------------------------
-- updated_at maintenance for work_item (List sort key trustworthiness, §13)
-- ---------------------------------------------------------------------------
CREATE FUNCTION coord.touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
CREATE TRIGGER work_item_touch_updated_at
    BEFORE UPDATE ON coord.work_item
    FOR EACH ROW EXECUTE FUNCTION coord.touch_updated_at();

-- ---------------------------------------------------------------------------
-- Tenancy inheritance for sub-tickets (§6.1(c) / §12.1; ISI-2339 F5)
-- ---------------------------------------------------------------------------
-- A child inherits its parent's project_id/team_id, and cross-Project re-parenting is rejected — so
-- the §12.1 tenancy filter stays a single `project_id` predicate. The reconciler in pkg/coord also
-- enforces this on write (with the depth-capped cycle walk); this trigger makes the tenancy boundary
-- STRUCTURAL defense-in-depth so a bug or direct write cannot straddle Projects. team_id is inherited
-- from the parent; a project_id that disagrees with the parent's is a hard error, not a silent rewrite.
CREATE FUNCTION coord.enforce_parent_tenancy() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE parent_project uuid; parent_team uuid;
BEGIN
    IF NEW.parent_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT project_id, team_id INTO parent_project, parent_team
      FROM coord.work_item WHERE id = NEW.parent_id;
    IF NOT FOUND THEN
        RETURN NEW;   -- nonexistent parent: let the FK constraint reject it
    END IF;
    IF NEW.project_id <> parent_project THEN
        RAISE EXCEPTION 'cross-Project re-parenting rejected: child project_id % <> parent project_id % (§12.1)',
            NEW.project_id, parent_project USING ERRCODE = 'restrict_violation';
    END IF;
    NEW.team_id := parent_team;   -- inherit
    RETURN NEW;
END;
$$;
CREATE TRIGGER work_item_enforce_parent_tenancy
    BEFORE INSERT OR UPDATE OF parent_id, project_id, team_id ON coord.work_item
    FOR EACH ROW EXECUTE FUNCTION coord.enforce_parent_tenancy();
