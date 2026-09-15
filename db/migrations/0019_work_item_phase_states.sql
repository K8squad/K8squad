-- 0019_work_item_phase_states.sql — extend the board-lane enum from the 5-lane
-- model to the ISI-4431/ISI-4452 10-phase human lifecycle (ISI-4455).
--
-- The 0001 work_item.state CHECK pinned 5 lanes:
--     backlog · todo · in_progress · in_review · done
-- The phase-status model adds the human work phases:
--     design · planning · implementation · code_review · testing · documentation · cancelled
--
-- This migration is ADDITIVE, not a replace:
--   * The 6 phase columns + cancelled are ADMITTED so a human can move a card
--     into them (pkg/coord humanStates, phaseTransitions).
--   * `in_progress` and `in_review` are RETAINED. They are the coordinator's
--     mechanical dispatch lanes — prodclaim.go writes `in_progress` on claim,
--     settle.go / prodreroute.go read it, agents self-report `in_review`. Dropping
--     them would break the dispatch engine (out of scope for this child). The
--     console folds them, for display, into the implementation / code_review
--     phase columns (ISI-4452 handoff). Retiring them (the literal ISI-4431
--     "Option A" enum replacement) is a later migration on the coordinator
--     auto-advance track, once the engine speaks the phase vocabulary directly.
--
-- `blocked` is still NOT a state — it is an orthogonal condition (blocked_reason),
-- per 0001 / §8.6. No data migration is needed: every existing row already holds
-- one of the retained 5 values, all of which stay valid.
--
-- The 0001 CHECK was an inline (auto-named) column constraint. We drop it by
-- INTROSPECTION (its generated name is not portable) and re-add a NAMED one so a
-- future migration can alter it deterministically.

-- Drop the existing state CHECK whatever Postgres auto-named it.
DO $$
DECLARE cname text;
BEGIN
    SELECT con.conname
      INTO cname
      FROM pg_constraint con
      JOIN pg_class     rel ON rel.oid = con.conrelid
      JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
     WHERE nsp.nspname = 'coord'
       AND rel.relname = 'work_item'
       AND con.contype = 'c'
       AND pg_get_constraintdef(con.oid) ILIKE '%state %IN%';
    IF cname IS NOT NULL THEN
        EXECUTE format('ALTER TABLE coord.work_item DROP CONSTRAINT %I', cname);
    END IF;
END $$;

ALTER TABLE coord.work_item
    ADD CONSTRAINT work_item_state_check
    CHECK (state IN (
        'backlog', 'todo',
        'design', 'planning', 'implementation',
        'code_review', 'testing', 'documentation',
        'done', 'cancelled',
        -- transitional engine lanes (retained; see header)
        'in_progress', 'in_review'
    ));
