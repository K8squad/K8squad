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
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	logscolpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	tracecolpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// openTestDB provisions the instscore schema from the SHIPPED 0042 migration
// against the DATABASE_URL Postgres and returns the handle — the same pattern
// as the audittrail/discussion suites: reset only this suite's schema and apply
// the real migration file (not inline DDL), so drift between 0042 and the store
// goes RED here. It deliberately does NOT walk the whole dbmigrate chain: the
// shared L1 lane's DATABASE_URL already carries unledgered coord/discussion
// schemas re-created by sibling suites (e.g. audittrail's DROP SCHEMA coord +
// raw 0001), and dbmigrate.Apply is forward-only — it cannot baseline past
// them. Gated on DATABASE_URL like the sibling suites — a plain `go test`
// skips these.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the instscoreingest integration test (needs a real Postgres)")
	}
	dbh, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dbh.PingContext(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	if _, err := dbh.ExecContext(ctx, `DROP SCHEMA IF EXISTS instscore CASCADE`); err != nil {
		t.Fatalf("reset instscore schema: %v", err)
	}
	var mig []byte
	for _, c := range []string{
		filepath.Join("..", "..", "db", "migrations", "0042_instscore_ingest.sql"),
		filepath.Join("db", "migrations", "0042_instscore_ingest.sql"),
	} {
		if mig, err = os.ReadFile(c); err == nil {
			break
		}
	}
	if mig == nil {
		t.Fatal("could not read shipped migration 0042_instscore_ingest.sql (cwd-relative candidates failed)")
	}
	if _, err := dbh.ExecContext(ctx, string(mig)); err != nil {
		t.Fatalf("apply 0042_instscore_ingest: %v", err)
	}
	return dbh
}

// TestSQLStoreRoundTrip proves the full storage loop against migration 0042:
// append service-scoped evidence, read the window back oldest-first, upsert
// verdicts + score (latest semantics), prune out-of-window rows.
func TestSQLStoreRoundTrip(t *testing.T) {
	dbh := openTestDB(t)
	store, err := NewSQLStore(dbh)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	ctx := context.Background()

	// Two services' rows, one out-of-window.
	now := time.Now().UTC()
	rows := []WindowRow{
		{ServiceName: "it-payments", Signal: "traces", Evidence: []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"it-payments"}}]}}]}`), ReceivedAt: now.Add(-time.Hour), SpanCount: 2, ResourceCount: 1},
		{ServiceName: "it-payments", Signal: "logs", Evidence: []byte(`{"resourceLogs":[]}`), ReceivedAt: now.Add(-30 * time.Minute), LogCount: 4},
		{ServiceName: "it-checkout", Signal: "traces", Evidence: []byte(`{"resourceSpans":[]}`), ReceivedAt: now.Add(-time.Minute), SpanCount: 1},
		{ServiceName: "it-payments", Signal: "traces", Evidence: []byte(`{"resourceSpans":[]}`), ReceivedAt: now.Add(-40 * 24 * time.Hour), SpanCount: 99},
	}
	if err := store.AppendWindow(ctx, rows); err != nil {
		t.Fatalf("AppendWindow: %v", err)
	}

	got, err := store.LoadWindow(ctx, "it-payments", now.Add(-DefaultWindow))
	if err != nil {
		t.Fatalf("LoadWindow: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("window rows = %d, want 2 (in-window only)", len(got))
	}
	if got[0].Signal != "traces" || got[1].Signal != "logs" {
		t.Fatalf("window order = [%s, %s], want oldest-first [traces, logs]", got[0].Signal, got[1].Signal)
	}
	if got[0].SpanCount != 2 || got[1].LogCount != 4 {
		t.Fatalf("counts not round-tripped: %+v", got)
	}

	// Verdict upsert semantics: same (service, rule) replaces; a second rule appends.
	verdicts := []VerdictRow{
		{RuleID: "RES-005", Status: "pass", Evidence: "service.name present on all resources", Impact: "Critical"},
		{RuleID: "LOG-001", Status: "fail", Evidence: "no log severity mapping", Impact: "Important"},
	}
	score := ScoreRow{ServiceName: "it-payments", BucketHour: now.Truncate(time.Hour), Score: 63.86, Category: "Needs Improvement", Implemented: 24, Total: 26, EvaluatedAt: now}
	if err := store.SaveEvaluation(ctx, "it-payments", verdicts, score); err != nil {
		t.Fatalf("SaveEvaluation: %v", err)
	}
	verdicts[1] = VerdictRow{RuleID: "LOG-001", Status: "pass", Evidence: "severities mapped", Impact: "Important"}
	if err := store.SaveEvaluation(ctx, "it-payments", verdicts, score); err != nil {
		t.Fatalf("SaveEvaluation 2: %v", err)
	}
	var status string
	if err := dbh.QueryRowContext(ctx,
		`SELECT status FROM instscore.rule_verdict WHERE service_name=$1 AND rule_id='LOG-001'`,
		"it-payments").Scan(&status); err != nil {
		t.Fatalf("query verdict: %v", err)
	}
	if status != "pass" {
		t.Fatalf("LOG-001 status = %q, want the latest (pass)", status)
	}
	var n int
	if err := dbh.QueryRowContext(ctx,
		`SELECT count(*) FROM instscore.rule_verdict WHERE service_name=$1`, "it-payments").Scan(&n); err != nil {
		t.Fatalf("count verdicts: %v", err)
	}
	if n != 2 {
		t.Fatalf("verdict rows = %d, want 2 (upsert, not append)", n)
	}
	at, ok, err := store.LatestEvaluatedAt(ctx, "it-payments")
	if err != nil || !ok {
		t.Fatalf("LatestEvaluatedAt: %v ok=%v", err, ok)
	}
	if !at.Equal(now.Truncate(time.Microsecond)) && at.Unix() != now.Unix() {
		t.Fatalf("LatestEvaluatedAt = %v, want ~%v", at, now)
	}

	// Prune removes only out-of-window rows.
	removed, err := store.PruneBefore(ctx, now.Add(-DefaultWindow))
	if err != nil {
		t.Fatalf("PruneBefore: %v", err)
	}
	if removed != 1 {
		t.Fatalf("pruned = %d, want 1 (the 40-day-old row)", removed)
	}
	got, err = store.LoadWindow(ctx, "it-payments", now.Add(-DefaultWindow))
	if err != nil {
		t.Fatalf("LoadWindow after prune: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("window rows after prune = %d, want 2", len(got))
	}
}

// TestServiceIngestEvaluatesAgainstPostgres runs the Service (split → persist
// → evaluate) against the real store: the re-fold must produce a full verdict
// set for the ingested service.
func TestServiceIngestEvaluatesAgainstPostgres(t *testing.T) {
	dbh := openTestDB(t)
	store, err := NewSQLStore(dbh)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	svc, err := New(store, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := svc.IngestTraces(ctx, traceReq("it-svc-a")); err != nil {
		t.Fatalf("IngestTraces: %v", err)
	}
	if err := svc.IngestLogs(ctx, logReq("it-svc-a")); err != nil {
		t.Fatalf("IngestLogs: %v", err)
	}

	var verdicts int
	if err := dbh.QueryRowContext(ctx,
		`SELECT count(*) FROM instscore.rule_verdict WHERE service_name=$1`, "it-svc-a").Scan(&verdicts); err != nil {
		t.Fatalf("count verdicts: %v", err)
	}
	if verdicts == 0 {
		t.Fatal("no verdicts persisted after first ingest")
	}
	var score sql.NullFloat64
	if err := dbh.QueryRowContext(ctx,
		`SELECT score FROM instscore.score_history WHERE service_name=$1`, "it-svc-a").Scan(&score); err != nil {
		t.Fatalf("query score: %v", err)
	}
	if !score.Valid {
		t.Fatal("no score-history point persisted")
	}
}

func traceReq(service string) *tracecolpb.ExportTraceServiceRequest {
	return &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: resource(service, strKV("telemetry.sdk.name", "opentelemetry"), strKV("telemetry.sdk.language", "go")),
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			SpanId: []byte{1}, TraceId: []byte{1}, Name: "op", Kind: tracepb.Span_SPAN_KIND_SERVER,
		}}}},
	}}}
}

func logReq(service string) *logscolpb.ExportLogsServiceRequest {
	return &logscolpb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: resource(service),
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			SeverityText: "INFO", SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hi"}},
		}}}},
	}}}
}
