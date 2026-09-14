-- 0020_work_item_create_fields.sql — persistable create-time attributes on
-- coord.work_item: priority, work_mode, labels (ISI-4409, split from ISI-4400).
--
-- WHY: the console create-ticket slide-over (ISI-4399 S2) offers Work-mode /
-- Priority / Labels controls, but coord.work_item carried only
-- title/body/state — so those controls had nowhere to land. The board is a
-- PROJECTION of coord.work_item (§13), and the FE create form POSTs to the
-- coord-backed apiserver route, so coord must gain the columns. This adds the
-- three PERSISTABLE fields; assignee→start-a-Run is custody, not a field, and
-- stays out of the plain create (Part B / ISI-4410).
--
-- SHAPE — CHECK-constrained text + text[], matching existing conventions:
--   * priority / work_mode are nullable text with a CHECK enum, mirroring the
--     `state` column style (not a Postgres ENUM type — migrations stay cheap and
--     reversible, and the enum set can evolve with an ALTER … DROP/ADD CONSTRAINT
--     rather than the heavier ALTER TYPE dance).
--   * labels is text[] NOT NULL DEFAULT '{}', the SAME shape 0014 chose for
--     acceptance_criteria / goals — it maps 1:1 onto the read model's []string
--     field with no parse seam, and reads never null-check.
--
-- Enum vocabularies:
--   * priority  — low|medium|high|urgent (mirrors the Paperclip issue priority set).
--   * work_mode — standard|planning (the Paperclip issue-layer work-mode set,
--     confirmed via the issues API).
--   Go validates enum membership BEFORE insert (ErrInvalidWorkItem → 400), so a
--   bad value is a clean client error, not a raw CHECK-violation 502; the CHECK
--   is the belt-and-braces backstop at the schema wall.
--
-- FR-I3 no fabricated values / no backfill: priority and work_mode are nullable
-- and default NULL, labels defaults '{}', so every EXISTING row reads back as an
-- honest "none" (never a made-up default). Additive + forward-only + backfill-
-- safe: a no-op for in-flight items until a create/edit populates the columns.

ALTER TABLE coord.work_item
    ADD COLUMN priority  text,                       -- NULL = unset (honest "none")
    ADD COLUMN work_mode text,                       -- NULL = unset (honest "none")
    ADD COLUMN labels    text[] NOT NULL DEFAULT '{}'; -- '{}' = no labels, never NULL

ALTER TABLE coord.work_item
    ADD CONSTRAINT work_item_priority_chk
        CHECK (priority IS NULL OR priority IN ('low', 'medium', 'high', 'urgent'));

ALTER TABLE coord.work_item
    ADD CONSTRAINT work_item_work_mode_chk
        CHECK (work_mode IS NULL OR work_mode IN ('standard', 'planning'));
