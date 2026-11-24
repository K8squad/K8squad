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
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WindowRow is one persisted evidence slice: a single (batch × service ×
// signal) piece of the per-service evidence window (instscore.evidence_window,
// migration 0042). Evidence is the protojson export request restricted to that
// service's resource groups — byte-compatible with instscore.LoadDumpInto.
type WindowRow struct {
	ServiceName   string
	Signal        string // "traces" | "metrics" | "logs"
	Evidence      []byte
	ReceivedAt    time.Time
	SpanCount     int
	LogCount      int
	MetricCount   int
	ResourceCount int
}

// VerdictRow is one latest per-rule verdict (instscore.rule_verdict).
type VerdictRow struct {
	RuleID   string
	Status   string // pass | fail | not_evaluated
	Evidence string
	Impact   string // Critical | Important | Normal | Low
}

// ScoreRow is one hourly score-history point (instscore.score_history).
type ScoreRow struct {
	ServiceName string
	BucketHour  time.Time
	Score       float64
	Category    string
	Implemented int
	Total       int
	EvaluatedAt time.Time
}

// Store is the persistence seam. The production binding is SQLStore over the
// shared Postgres handle; tests bind an in-memory fake.
type Store interface {
	// AppendWindow persists evidence rows (one transaction).
	AppendWindow(ctx context.Context, rows []WindowRow) error
	// LoadWindow returns one service's evidence rows received after since,
	// oldest first — the re-fold input for evaluation.
	LoadWindow(ctx context.Context, serviceName string, since time.Time) ([]WindowRow, error)
	// LatestEvaluatedAt reports the service's newest rule_verdict stamp.
	LatestEvaluatedAt(ctx context.Context, serviceName string) (time.Time, bool, error)
	// SaveEvaluation upserts the rule verdicts and the score-history point in
	// one transaction, stamping evaluated_at.
	SaveEvaluation(ctx context.Context, serviceName string, verdicts []VerdictRow, score ScoreRow) error
	// PruneBefore deletes evidence rows older than cutoff (window retention).
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// SQLStore is the production Store over db/migrations/0042_instscore_ingest.sql.
// It holds no mutable state beyond the *sql.DB, so it is safe for concurrent use.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore binds the ingestion store to the coordination Postgres handle.
func NewSQLStore(db *sql.DB) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("instscoreingest.NewSQLStore: nil db")
	}
	return &SQLStore{db: db}, nil
}

// AppendWindow inserts the batch's per-service evidence rows.
func (s *SQLStore) AppendWindow(ctx context.Context, rows []WindowRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("instscoreingest.AppendWindow: begin: %w", err)
	}
	defer rollbackIfActive(tx)
	for _, r := range rows {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO instscore.evidence_window
			       (service_name, received_at, signal, evidence,
			        span_count, log_count, metric_count, resource_count)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8)`,
			r.ServiceName, r.ReceivedAt, r.Signal, string(r.Evidence),
			r.SpanCount, r.LogCount, r.MetricCount, r.ResourceCount)
		if err != nil {
			return fmt.Errorf("instscoreingest.AppendWindow: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("instscoreingest.AppendWindow: commit: %w", err)
	}
	return nil
}

// LoadWindow reads one service's window inside the sliding interval, oldest
// first — the (service_name, received_at) index serves it directly.
func (s *SQLStore) LoadWindow(ctx context.Context, serviceName string, since time.Time) ([]WindowRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT received_at, signal, evidence::text,
		       span_count, log_count, metric_count, resource_count
		  FROM instscore.evidence_window
		 WHERE service_name = $1 AND received_at > $2
		 ORDER BY received_at, id`, serviceName, since)
	if err != nil {
		return nil, fmt.Errorf("instscoreingest.LoadWindow: %w", err)
	}
	defer rows.Close()
	var out []WindowRow
	for rows.Next() {
		var r WindowRow
		var evidence string
		if err := rows.Scan(&r.ReceivedAt, &r.Signal, &evidence,
			&r.SpanCount, &r.LogCount, &r.MetricCount, &r.ResourceCount); err != nil {
			return nil, fmt.Errorf("instscoreingest.LoadWindow: scan: %w", err)
		}
		r.ServiceName = serviceName
		r.Evidence = []byte(evidence)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("instscoreingest.LoadWindow: iteration: %w", err)
	}
	return out, nil
}

// LatestEvaluatedAt reports the newest verdict stamp for the service.
func (s *SQLStore) LatestEvaluatedAt(ctx context.Context, serviceName string) (time.Time, bool, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT max(evaluated_at) FROM instscore.rule_verdict WHERE service_name = $1`,
		serviceName).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("instscoreingest.LatestEvaluatedAt: %w", err)
	}
	return at.Time, at.Valid, nil
}

// SaveEvaluation upserts the latest verdicts and the hourly score point in one
// transaction: re-evaluations replace verdict rows (PK service+rule) and the
// current hour's history point (PK service+bucket), so both surfaces read
// "latest" without history drift.
func (s *SQLStore) SaveEvaluation(ctx context.Context, serviceName string, verdicts []VerdictRow, score ScoreRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("instscoreingest.SaveEvaluation: begin: %w", err)
	}
	defer rollbackIfActive(tx)
	for _, v := range verdicts {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO instscore.rule_verdict
			       (service_name, rule_id, status, evidence, impact, evaluated_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (service_name, rule_id)
			DO UPDATE SET status = excluded.status, evidence = excluded.evidence,
			              impact = excluded.impact, evaluated_at = excluded.evaluated_at`,
			serviceName, v.RuleID, v.Status, v.Evidence, v.Impact, score.EvaluatedAt)
		if err != nil {
			return fmt.Errorf("instscoreingest.SaveEvaluation: verdict %s: %w", v.RuleID, err)
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO instscore.score_history
		       (service_name, bucket_hour, score, category, implemented, total, evaluated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (service_name, bucket_hour)
		DO UPDATE SET score = excluded.score, category = excluded.category,
		              implemented = excluded.implemented, total = excluded.total,
		              evaluated_at = excluded.evaluated_at`,
		score.ServiceName, score.BucketHour, score.Score, score.Category,
		score.Implemented, score.Total, score.EvaluatedAt)
	if err != nil {
		return fmt.Errorf("instscoreingest.SaveEvaluation: score: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("instscoreingest.SaveEvaluation: commit: %w", err)
	}
	return nil
}

// PruneBefore drops evidence outside the sliding window. Verdicts and score
// history are kept: they are the durable report, and the row counts are
// bounded by (services × hours), not by telemetry volume.
func (s *SQLStore) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM instscore.evidence_window WHERE received_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("instscoreingest.PruneBefore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("instscoreingest.PruneBefore: rows: %w", err)
	}
	return n, nil
}

// rollbackIfActive rolls back an in-flight transaction, ignoring the
// already-committed/rolled-back case the deferred call hits on the happy path.
func rollbackIfActive(tx *sql.Tx) {
	_ = tx.Rollback()
}
