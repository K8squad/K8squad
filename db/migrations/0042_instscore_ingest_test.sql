-- 0042_instscore_ingest_test.sql — runnable self-check for the instrumentation-
-- score ingestion schema (ISI-7749, phase 3 of the ISI-7741 plan).
--
-- Same discipline as the 0005/0039 self-checks: plain SQL, no framework, run
-- after the migrations it checks, inside one transaction ROLLED BACK at the end
-- so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0042_instscore_ingest.sql \
--          -f db/migrations/0042_instscore_ingest_test.sql
--
-- The BEHAVIOURAL guarantees (per-service batch splitting, window re-folding,
-- verdict upserts) are exercised by the Go tests in internal/instscoreingest.
-- This file proves the *structural* ACs of the schema: closed enums, PK
-- upsert semantics, the empty-service.name row, and window query shapes.

BEGIN;

-- (1) The closed enums fail closed: a verdict row cannot drift off the
--     engine's status/impact vocabulary, and a window row cannot carry a
--     signal outside the three OTLP pipelines.
DO $$
BEGIN
    INSERT INTO instscore.rule_verdict (service_name, rule_id, status, evidence, impact)
    VALUES ('svc', 'RES-005', 'skipped', 'x', 'Critical');
    RAISE EXCEPTION 'rule_verdict.status must reject out-of-vocabulary values';
EXCEPTION WHEN check_violation THEN NULL;
END $$;

DO $$
BEGIN
    INSERT INTO instscore.evidence_window (service_name, signal, evidence)
    VALUES ('svc', 'profiles', '{}');
    RAISE EXCEPTION 'evidence_window.signal must reject non-OTLP-pipeline values';
EXCEPTION WHEN check_violation THEN NULL;
END $$;

-- (2) The empty service.name evidence row survives: telemetry without a
--     service.name is DATA for RES-005, not a constraint violation.
INSERT INTO instscore.evidence_window (service_name, signal, evidence)
VALUES ('', 'traces', '{"resourceSpans": []}');
DO $$
DECLARE n integer;
BEGIN
    SELECT count(*) INTO n FROM instscore.evidence_window WHERE service_name = '';
    ASSERT n = 1, 'the unnamed-service window row must be storable and queryable';
END $$;

-- (3) Verdict upsert semantics: re-evaluating a service replaces the LATEST
--     verdict per (service, rule) — one row, no history drift on this table.
INSERT INTO instscore.rule_verdict (service_name, rule_id, status, evidence, impact)
VALUES ('svc', 'LOG-001', 'fail', 'no log records observed in window', 'Important');
INSERT INTO instscore.rule_verdict (service_name, rule_id, status, evidence, impact)
VALUES ('svc', 'LOG-001', 'pass', '6 log records, severities mapped', 'Important')
ON CONFLICT (service_name, rule_id)
DO UPDATE SET status = excluded.status, evidence = excluded.evidence, evaluated_at = now();
DO $$
DECLARE n integer; st text;
BEGIN
    SELECT count(*) INTO n FROM instscore.rule_verdict WHERE service_name = 'svc';
    ASSERT n = 1, 'a rule re-evaluation must upsert, not append';
    SELECT status INTO st FROM instscore.rule_verdict WHERE service_name = 'svc' AND rule_id = 'LOG-001';
    ASSERT st = 'pass', 'the surviving verdict must be the latest evaluation';
END $$;

-- (4) Score history is hourly-bucketed: two evaluations inside one hour land
--     on one point; the next hour appends a new one.
INSERT INTO instscore.score_history (service_name, bucket_hour, score, category, implemented, total)
VALUES ('svc', date_trunc('hour', now()), 63.86, 'Needs Improvement', 24, 26);
INSERT INTO instscore.score_history (service_name, bucket_hour, score, category, implemented, total)
VALUES ('svc', date_trunc('hour', now()), 70.00, 'Needs Improvement', 24, 26)
ON CONFLICT (service_name, bucket_hour)
DO UPDATE SET score = excluded.score, category = excluded.category, evaluated_at = now();
INSERT INTO instscore.score_history (service_name, bucket_hour, score, category, implemented, total)
VALUES ('svc', date_trunc('hour', now()) + interval '1 hour', 75.00, 'Good', 24, 26);
DO $$
DECLARE n integer; sc numeric;
BEGIN
    SELECT count(*) INTO n FROM instscore.score_history WHERE service_name = 'svc';
    ASSERT n = 2, 'same-hour evaluations collapse to one point; a new hour appends one';
    SELECT score INTO sc FROM instscore.score_history
     WHERE service_name = 'svc' AND bucket_hour = date_trunc('hour', now());
    ASSERT sc = 70.00, 'the same-hour point must carry the latest evaluation';
END $$;

-- (5) The window evaluation read finds exactly one service's evidence across
--     signals inside the window — the (service_name, received_at) index shape
--     serves it without a scan over other services' rows.
INSERT INTO instscore.evidence_window (service_name, signal, evidence, span_count)
VALUES ('svc', 'traces', '{"resourceSpans": []}', 3),
       ('other', 'traces', '{"resourceSpans": []}', 99),
       ('svc', 'logs', '{"resourceLogs": []}', 5);
DO $$
DECLARE total integer;
BEGIN
    SELECT sum(span_count + log_count) INTO total FROM instscore.evidence_window
     WHERE service_name = 'svc'
       AND received_at > now() - interval '30 days';
    ASSERT total = 8, 'the per-service window query must fold only that service''s rows';
END $$;

-- (6) Score bounds: the numeric domain is the spec's 0-100, fail closed.
DO $$
BEGIN
    INSERT INTO instscore.score_history (service_name, bucket_hour, score, category, implemented, total)
    VALUES ('svc', date_trunc('hour', now()) + interval '2 hours', 104.2, 'Good', 26, 26);
    RAISE EXCEPTION 'score_history.score must reject values outside 0-100';
EXCEPTION WHEN check_violation THEN NULL;
END $$;

ROLLBACK;
