/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package instscoreingest

import (
	"context"
	"fmt"
	"time"
)

// Phase 4 of the ISI-7741 plan (ISI-7750): the READ side of the
// instrumentation-score store. The ingestion service writes through Store
// (store.go); these queries feed the apiserver's
// GET /api/v1/instscore/services and /services/{name} read surface.
//
// The shape mirrors migration 0042's design comments exactly:
//   - the services LIST is an aggregate over instscore.evidence_window (a
//     service "exists" because telemetry arrived — even before the 10-minute
//     re-evaluation throttle fires, so its score may still be NULL) joined to
//     the LATEST score_history point and the rule_verdict tallies;
//   - the DETAIL is the stored latest verdicts + the hourly score history.
//
// Registry enrichment (rule description, failed-rule guidance) happens in the
// apiserver layer over pkg/telemetry/instscore — this file returns exactly
// what Postgres holds, nothing vendor-flavored.

// ServiceSummary is one row of the services list: what a service scored, how
// much evidence backed it, and when it was last evaluated. Score/Category/
// EvaluatedAt are nil until the service's first evaluation lands (evidence
// present, throttle window not yet elapsed — the honest "pending" state).
type ServiceSummary struct {
	ServiceName string
	Score       *float64
	Category    string
	Implemented int
	Total       int
	EvaluatedAt *time.Time
	PassCount   int
	FailCount   int
	SkipCount   int // verdicts stored as not_evaluated
	SpanCount   int
	LogCount    int
	MetricCount int
	// ResourceCount is the summed per-batch resource count, so it counts
	// resource GROUPS across batches, not distinct resources.
	ResourceCount int
	FirstBatchAt  *time.Time
	LastBatchAt   *time.Time
}

// VerdictDetail is one stored latest verdict (instscore.rule_verdict).
type VerdictDetail struct {
	RuleID      string
	Status      string // pass | fail | not_evaluated
	Evidence    string
	Impact      string // Critical | Important | Normal | Low
	EvaluatedAt time.Time
}

// HistoryPoint is one hourly score_history sample.
type HistoryPoint struct {
	BucketHour time.Time
	Score      float64
}

// ServiceDetail is the per-service read: the list-row summary plus the full
// rule-by-rule verdict table and the hourly score history (oldest first).
type ServiceDetail struct {
	Summary  ServiceSummary
	Verdicts []VerdictDetail
	History  []HistoryPoint
}

// maxHistoryPoints caps the detail's history tail. The 30-day window holds at
// most 720 hourly points; the cap keeps a corrupt/long-lived row set from
// turning the detail payload into a dump.
const maxHistoryPoints = 720

// ListServices returns every observed service — every distinct
// evidence_window.service_name, including the empty string (telemetry that
// arrived with a missing/blank service.name; RES-005 scores it as a
// violation, so the evidence must stay visible, not be dropped). Ordering is
// worst-score-first, then name, so the console's "what needs attention" read
// falls out of the query; unevaluated services sort last (their score is
// pending, not zero).
func (s *SQLStore) ListServices(ctx context.Context) ([]ServiceSummary, error) {
	// latest: the newest score_history row per service (DISTINCT ON over the
	// evaluated_at DESC ordering — score_history is upserted per hour bucket,
	// so the max evaluated_at is the live point).
	rows, err := s.db.QueryContext(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (service_name)
			       service_name, score, category, implemented, total, evaluated_at
			FROM instscore.score_history
			ORDER BY service_name, evaluated_at DESC
		), verdicts AS (
			SELECT service_name,
			       count(*) FILTER (WHERE status = 'pass') AS pass_count,
			       count(*) FILTER (WHERE status = 'fail') AS fail_count,
			       count(*) FILTER (WHERE status = 'not_evaluated') AS skip_count
			FROM instscore.rule_verdict
			GROUP BY service_name
		)
		SELECT e.service_name,
		       l.score, l.category, COALESCE(l.implemented, 0), COALESCE(l.total, 0), l.evaluated_at,
		       COALESCE(v.pass_count, 0), COALESCE(v.fail_count, 0), COALESCE(v.skip_count, 0),
		       COALESCE(sum(e.span_count), 0), COALESCE(sum(e.log_count), 0),
		       COALESCE(sum(e.metric_count), 0), COALESCE(sum(e.resource_count), 0),
		       min(e.received_at), max(e.received_at)
		FROM instscore.evidence_window e
		LEFT JOIN latest l   ON l.service_name = e.service_name
		LEFT JOIN verdicts v ON v.service_name = e.service_name
		GROUP BY e.service_name, l.score, l.category, l.implemented, l.total,
		         l.evaluated_at, v.pass_count, v.fail_count, v.skip_count
		ORDER BY l.score ASC NULLS LAST, e.service_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("instscoreingest.ListServices: %w", err)
	}
	defer rows.Close()

	var out []ServiceSummary
	for rows.Next() {
		var sm ServiceSummary
		var score *float64
		var category *string
		var evaluatedAt *time.Time
		if err := rows.Scan(
			&sm.ServiceName, &score, &category, &sm.Implemented, &sm.Total, &evaluatedAt,
			&sm.PassCount, &sm.FailCount, &sm.SkipCount,
			&sm.SpanCount, &sm.LogCount, &sm.MetricCount, &sm.ResourceCount,
			&sm.FirstBatchAt, &sm.LastBatchAt,
		); err != nil {
			return nil, fmt.Errorf("instscoreingest.ListServices: scan: %w", err)
		}
		if score != nil {
			v := *score
			sm.Score = &v
		}
		if category != nil {
			sm.Category = *category
		}
		sm.EvaluatedAt = evaluatedAt
		out = append(out, sm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("instscoreingest.ListServices: %w", err)
	}
	return out, nil
}

// GetService returns one service's detail. found=false when the service has
// no evidence, no verdicts, and no history at all (the caller 404s); a
// service mid-throttle still resolves with Score nil and empty verdicts.
// History is oldest-first, capped at maxHistoryPoints (the newest tail).
func (s *SQLStore) GetService(ctx context.Context, serviceName string) (ServiceDetail, bool, error) {
	detail := ServiceDetail{}

	// The summary arm reuses the list aggregate for the one service; a service
	// with verdicts/history but its window already pruned (retention is 30
	// days; verdicts are the DURABLE report per migration 0042) still resolves
	// from the score_history + rule_verdict arms alone. The no-group-by
	// aggregates over the service each yield exactly one row; LEFT JOIN latest
	// keeps that row when no score point exists yet (Score stays NULL).
	rows, err := s.db.QueryContext(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (service_name)
			       service_name, score, category, implemented, total, evaluated_at
			FROM instscore.score_history
			WHERE service_name = $1
			ORDER BY service_name, evaluated_at DESC
		), v AS (
			SELECT count(*) FILTER (WHERE status = 'pass') AS pass_count,
			       count(*) FILTER (WHERE status = 'fail') AS fail_count,
			       count(*) FILTER (WHERE status = 'not_evaluated') AS skip_count
			FROM instscore.rule_verdict
			WHERE service_name = $1
		), e AS (
			SELECT COALESCE(sum(span_count), 0) AS spans,
			       COALESCE(sum(log_count), 0) AS logs,
			       COALESCE(sum(metric_count), 0) AS metrics,
			       COALESCE(sum(resource_count), 0) AS resources,
			       min(received_at) AS first_batch, max(received_at) AS last_batch
			FROM instscore.evidence_window
			WHERE service_name = $1
		)
		SELECT $1, l.score, l.category, COALESCE(l.implemented, 0), COALESCE(l.total, 0), l.evaluated_at,
		       v.pass_count, v.fail_count, v.skip_count,
		       e.spans, e.logs, e.metrics, e.resources, e.first_batch, e.last_batch
		FROM v CROSS JOIN e LEFT JOIN latest l ON true`, serviceName)
	if err != nil {
		return detail, false, fmt.Errorf("instscoreingest.GetService: %w", err)
	}
	defer rows.Close()

	if rows.Next() {
		var score *float64
		var category *string
		var evaluatedAt *time.Time
		if err := rows.Scan(
			&detail.Summary.ServiceName, &score, &category, &detail.Summary.Implemented,
			&detail.Summary.Total, &evaluatedAt,
			&detail.Summary.PassCount, &detail.Summary.FailCount, &detail.Summary.SkipCount,
			&detail.Summary.SpanCount, &detail.Summary.LogCount,
			&detail.Summary.MetricCount, &detail.Summary.ResourceCount,
			&detail.Summary.FirstBatchAt, &detail.Summary.LastBatchAt,
		); err != nil {
			return detail, false, fmt.Errorf("instscoreingest.GetService: scan: %w", err)
		}
		if score != nil {
			v := *score
			detail.Summary.Score = &v
		}
		if category != nil {
			detail.Summary.Category = *category
		}
		detail.Summary.EvaluatedAt = evaluatedAt
	}
	if err := rows.Err(); err != nil {
		return detail, false, fmt.Errorf("instscoreingest.GetService: %w", err)
	}

	// Verdicts: the latest per-rule table, rule-ID order for a stable render.
	vrows, err := s.db.QueryContext(ctx, `
		SELECT rule_id, status, evidence, impact, evaluated_at
		FROM instscore.rule_verdict
		WHERE service_name = $1
		ORDER BY rule_id ASC`, serviceName)
	if err != nil {
		return detail, false, fmt.Errorf("instscoreingest.GetService: verdicts: %w", err)
	}
	defer vrows.Close()
	for vrows.Next() {
		var v VerdictDetail
		if err := vrows.Scan(&v.RuleID, &v.Status, &v.Evidence, &v.Impact, &v.EvaluatedAt); err != nil {
			return detail, false, fmt.Errorf("instscoreingest.GetService: verdict scan: %w", err)
		}
		detail.Verdicts = append(detail.Verdicts, v)
	}
	if err := vrows.Err(); err != nil {
		return detail, false, fmt.Errorf("instscoreingest.GetService: verdicts: %w", err)
	}

	if detail.Summary.LastBatchAt == nil && len(detail.Verdicts) == 0 && detail.Summary.Score == nil {
		return detail, false, nil
	}

	// History: oldest-first tail capped at maxHistoryPoints.
	hrows, err := s.db.QueryContext(ctx, `
		SELECT bucket_hour, score
		FROM instscore.score_history
		WHERE service_name = $1
		ORDER BY bucket_hour DESC
		LIMIT $2`, serviceName, maxHistoryPoints)
	if err != nil {
		return detail, false, fmt.Errorf("instscoreingest.GetService: history: %w", err)
	}
	defer hrows.Close()
	for hrows.Next() {
		var p HistoryPoint
		if err := hrows.Scan(&p.BucketHour, &p.Score); err != nil {
			return detail, false, fmt.Errorf("instscoreingest.GetService: history scan: %w", err)
		}
		detail.History = append(detail.History, p)
	}
	if err := hrows.Err(); err != nil {
		return detail, false, fmt.Errorf("instscoreingest.GetService: history: %w", err)
	}
	// Reverse in place: queried DESC (newest first, for the LIMIT tail) but
	// returned oldest-first so the console's sparkline reads left→right.
	for i, j := 0, len(detail.History)-1; i < j; i, j = i+1, j-1 {
		detail.History[i], detail.History[j] = detail.History[j], detail.History[i]
	}
	return detail, true, nil
}
