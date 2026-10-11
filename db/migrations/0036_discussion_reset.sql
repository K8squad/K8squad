-- 0036_discussion_reset.sql — ISI-5672 (ISI-5598 WS-B, ADR-0030 Option 2 "reset-marker"):
-- the append-only-native /reset semantic for a discussion thread.
--
-- The discussion schema is append-only with one-way soft-retract and NO hard delete (0004 triggers):
-- "reset" therefore cannot mean "delete the rows". ADR-0030 adopts the reset-marker: /reset appends
-- exactly ONE kind='reset' message to the thread; the reader renders only the live, non-marker
-- messages whose created_at is strictly AFTER the latest live reset marker (the reset floor). The
-- marker is itself soft-retractable, so "undo reset" needs no new write path — retracting it drops it
-- from the live-marker set and the prior transcript reappears.
--
-- This is the LIGHTEST-TOUCH option against the ADR-0019 fence of the three ADR-0030 carried:
--   - ZERO new tables, ZERO new columns. Reset carries no lifecycle (unlike proposal/decision_request/
--     party_session, which each needed a mutable side table) — the marker's existence + its created_at
--     ARE the entire state. There is nowhere to express custody, so it cannot breach the fence by
--     construction, and fence_test.go (a COLUMN-name allow-list + forbidden-token guard, never a `kind`
--     value check) is UNTOUCHED.
--   - One widened `kind` CHECK value, the identical additive move proposal (0024/0025), decision_request
--     (0031), and party_start (0033) made. The re-ADD below carries the full then-current set (0033's
--     {text, structured, task, decision, vote, proposal, decision_request, party_start}) PLUS 'reset'.
--
-- Human-only + dedicated endpoint (NOT the generic normalizeKind path): a reset marker is stamped only
-- by Store.ResetThread via the human-only POST .../threads/{id}/reset endpoint, exactly as party_start
-- posts only through StartPartySession. normalizeKind stays unchanged so a client cannot forge a reset
-- by setting kind='reset' on an ordinary message (ADR-0030 §(a)6/§(a)7).
--
-- Forward-only, additive; applied once by the apiserver migration runner in filename order (same
-- discipline as 0024/0025/0031/0033). See [[discussion-schema-fence-column-denylist]].

-- Widen the kind CHECK (last set in 0033) so a kind='reset' marker is representable. Additive: every
-- pre-0036 kind is retained; 'reset' joins the set. No other column or table changes.
ALTER TABLE discussion.message DROP CONSTRAINT kind_must_be_text_or_extension;
ALTER TABLE discussion.message
    ADD CONSTRAINT kind_must_be_text_or_extension CHECK (
        kind = 'text' OR kind IN ('structured', 'task', 'decision', 'vote', 'proposal',
                                   'decision_request', 'party_start', 'reset')
    );
