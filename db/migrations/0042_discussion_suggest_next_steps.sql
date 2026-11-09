-- 0042_discussion_suggest_next_steps.sql — ISI-7218 (ISI-7075 C4,
-- docs/board-interaction-taxonomy.md §5): the suggest_next_steps interaction kind.
--
-- A coordinator reporting blocked/stuck must propose concrete next actions (tickets
-- to create, who to assign, what to unblock) as a card the board picks from — the
-- same option shape, the same card rules, the same DecisionCard renderer as a
-- decision_request. One option grammar, two kinds; the message kind is the only
-- discriminator.
--
-- ONE additive change, mirroring the widening moves of 0025/0031/0033/0036:
--   - kind='suggest_next_steps' joins the allowed message kinds (0036's CHECK is
--     widened; the re-ADD carries the full then-current set PLUS the new kind).
--
-- Deliberately NO new side table: the lifecycle rides discussion.decision_request
-- (0031) — one state machine (open → answered | rejected | expired | superseded),
-- one answer/reject CAS, one resume_agent_on_answer continuation, one Inbox arm
-- for BOTH card kinds. The kind-specific contract (modes choose_one|choose_many|
-- free_form, at most one recommended step, "blocked with zero suggested next
-- steps" refused) is enforced in application code (internal/discussion/suggest.go
-- ValidateSuggestNextSteps), the single place a validator lives — the CHECK here
-- stays the representability floor only.
--
-- normalizeKind (store.go) is UNTOUCHED, exactly as for decision_request/party_start/
-- reset: a suggest card is posted only through Store.PostSuggestNextSteps, never the
-- generic message path, so a client cannot forge one by setting kind on an ordinary
-- post. Forward-only, additive; applied once by the apiserver migration runner in
-- filename order. See [[discussion-schema-fence-column-denylist]].

-- Widen the kind CHECK (last set in 0036) so a kind='suggest_next_steps' card is
-- representable. Additive: every pre-0042 kind is retained; 'suggest_next_steps'
-- joins the set. No other column or table changes.
ALTER TABLE discussion.message DROP CONSTRAINT kind_must_be_text_or_extension;
ALTER TABLE discussion.message
    ADD CONSTRAINT kind_must_be_text_or_extension CHECK (
        kind = 'text' OR kind IN ('structured', 'task', 'decision', 'vote', 'proposal',
                                   'decision_request', 'party_start', 'reset', 'suggest_next_steps')
    );
