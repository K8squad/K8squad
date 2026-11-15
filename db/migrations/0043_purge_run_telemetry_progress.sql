-- 0043_purge_run_telemetry_progress.sql — ISI-7454 (parent ISI-7074): raw run
-- telemetry out of the ticket comment store.
--
-- Grounded on work_item 8fc6e9c9 (run f4d45f5b, agent sam/deepseek-chat,
-- 2026-11-14): 19 of 20 comments were ProgressMirror machine rows — 18x
-- '[run <id>][llm:response] <model> · N tok' token-usage digests plus
-- '[run <id>][status] completed' — 0 of them human-readable. That is the
-- "hard to understand" flood the board rejected (ISI-7074). The mirror now
-- keeps usage/status events on the live run stream only (their durable home
-- is Run.Status.LLMInteractions, ISI-4238, plus run lifecycle events), and
-- this migration purges the already-written telemetry corpus so existing
-- boards read clean: every kind='progress' row whose body carries a
-- ProgressMirror envelope.
--
-- The three envelope shapes are the mirror's entire durable corpus by
-- construction (mirrorBody renders only [llm:response], [status] and [tool:]
-- after the '[run <short>]' tag), including pre-ISI-7099 narration rows.
-- Settle's terminal progress row (engine principal, no '[run ' prefix) is
-- platform narration and stays; human and summary rows are untouched.
--
-- coord.comment is append-only (comment_append_only BEFORE UPDATE OR DELETE,
-- 0001). A one-time controlled purge is the sanctioned exception, exactly
-- like 0042's reclassification backfill: the trigger is disabled for just
-- this DELETE and re-enabled in the same transaction — the append-only
-- guarantee is unchanged for every runtime write before and after.

ALTER TABLE coord.comment DISABLE TRIGGER comment_append_only;
DELETE FROM coord.comment
 WHERE kind = 'progress'
   AND (body LIKE '[run %][llm:%'
        OR body LIKE '[run %][status] %'
        OR body LIKE '[run %][tool:%');
ALTER TABLE coord.comment ENABLE TRIGGER comment_append_only;
