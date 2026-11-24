-- 0042_instscore_ingest.sql — the instrumentation-score ingestion store (ISI-7749,
-- phase 3 of the ISI-7741 plan, revision 0dd9af62).
--
-- The OTel gateway's otlphttp/instscore tap exporter mirrors its (redacted,
-- post-k8sattributes/resource-processor) OTLP batches to the apiserver ingestion
-- route; this schema is what that route writes. Three tables, one per concern:
--
--   instscore.evidence_window — the per-service evidence the scoring engine folds.
--       One row per (received batch × service × signal): the batch's OTLP export
--       request restricted to that service's resource groups, stored as the same
--       protojson shape pkg/telemetry/instscore.LoadDumpInto re-folds offline.
--       The spec's sliding window (default 30 days) is retention, not row shape:
--       rows older than the window are pruned opportunistically on ingest, and
--       evaluation re-folds `received_at > now() - window` only. Denormalized
--       record counts keep the phase-4 services list a cheap aggregate instead
--       of a JSON walk.
--
--   instscore.rule_verdict — the LATEST per-rule verdict per service (upsert on
--       (service_name, rule_id)). This is the phase-4 read surface: per-service
--       rule-by-rule pass/fail with impact + evidence. Per-team/run aggregation
--       is computed FROM these stored verdicts (plan §3) — no snapshot table to
--       drift; an aggregate is always a query over the latest verdicts.
--
--   instscore.score_history — one row per (service, hour bucket): the score at
--       each evaluation, upserted within the bucket so a chatty re-evaluating
--       service still yields one point per hour (720 points / 30-day window).
--       History granularity is hourly by design; verdicts carry the "now" truth.
--
-- service_name is the engine's grouping key verbatim, INCLUDING the empty string
-- (telemetry that arrived with a missing/blank service.name — RES-005 reports it
-- as a violation, so the evidence must survive to be scored, not be dropped).
--
-- No triggers, no reject_mutation machinery: nothing here is coordination state.
-- The window is prune-eligible by design (batches are re-derivable from the
-- vendor backend; verdicts/history are the durable report). Impact/status values
-- mirror the Go registry's string forms exactly (pkg/telemetry/instscore/rules.go).

CREATE SCHEMA IF NOT EXISTS instscore;

CREATE TABLE instscore.evidence_window (
    id           bigserial PRIMARY KEY,
    service_name text        NOT NULL,             -- '' = batch carried no service.name
    received_at  timestamptz NOT NULL DEFAULT now(),
    signal       text        NOT NULL CHECK (signal IN ('traces', 'metrics', 'logs')),
    evidence     jsonb       NOT NULL,             -- protojson Export*ServiceRequest, service-scoped
    span_count   integer     NOT NULL DEFAULT 0,   -- denormalized fold counts (phase-4 list view)
    log_count    integer     NOT NULL DEFAULT 0,
    metric_count integer     NOT NULL DEFAULT 0,
    resource_count integer   NOT NULL DEFAULT 0
);

-- The evaluation read: everything one service contributed inside the window,
-- newest last. Also serves the phase-4 services list via the count aggregates.
CREATE INDEX idx_instscore_window_service_time
    ON instscore.evidence_window (service_name, received_at);

-- The retention prune: DELETE ... WHERE received_at < now() - interval '30 days'
-- walks this index, not the whole table.
CREATE INDEX idx_instscore_window_received
    ON instscore.evidence_window (received_at);

CREATE TABLE instscore.rule_verdict (
    service_name text        NOT NULL,
    rule_id      text        NOT NULL,
    status       text        NOT NULL CHECK (status IN ('pass', 'fail', 'not_evaluated')),
    evidence     text        NOT NULL,
    impact       text        NOT NULL CHECK (impact IN ('Critical', 'Important', 'Normal', 'Low')),
    evaluated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (service_name, rule_id)
);

CREATE TABLE instscore.score_history (
    service_name text          NOT NULL,
    bucket_hour  timestamptz   NOT NULL,           -- date_trunc('hour', evaluated_at)
    score        numeric(5, 2) NOT NULL
        CHECK (score >= 0 AND score <= 100),
    category     text          NOT NULL
        CHECK (category IN ('Excellent', 'Good', 'Needs Improvement', 'Poor')),
    implemented  integer       NOT NULL,
    total        integer       NOT NULL,
    evaluated_at timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (service_name, bucket_hour)
);
