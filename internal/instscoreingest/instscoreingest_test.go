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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	logscolpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscolpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecolpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/K8squad/K8squad/pkg/telemetry/instscore"
)

func resource(service string, extra ...*commonpb.KeyValue) *resourcepb.Resource {
	attrs := []*commonpb.KeyValue{
		{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}}},
	}
	attrs = append(attrs, extra...)
	return &resourcepb.Resource{Attributes: attrs}
}

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// ── splitter ─────────────────────────────────────────────────────────────────

func TestSplitTracesGroupsByServiceName(t *testing.T) {
	req := &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{
		{Resource: resource("payments"), ScopeSpans: []*tracepb.ScopeSpans{{
			Scope: &commonpb.InstrumentationScope{Name: "payments-sdk"},
			Spans: []*tracepb.Span{
				{SpanId: []byte{1}, TraceId: []byte{1}, Name: "charge"},
				{SpanId: []byte{2}, TraceId: []byte{1}, Name: "refund"},
			},
		}}},
		{Resource: resource("checkout"), ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{SpanId: []byte{3}, TraceId: []byte{2}, Name: "cart"}},
		}}},
		{Resource: resource("payments"), ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{SpanId: []byte{4}, TraceId: []byte{1}, Name: "settle"}},
		}}},
	}}

	splits := SplitTraces(req)
	if len(splits) != 2 {
		t.Fatalf("want 2 service splits, got %d", len(splits))
	}
	byName := map[string]TraceSplit{}
	for _, s := range splits {
		byName[s.ServiceName] = s
	}
	if got := byName["payments"].Spans; got != 3 {
		t.Errorf("payments spans = %d, want 3 (both resource groups)", got)
	}
	if got := byName["payments"].Resources; got != 1 {
		t.Errorf("payments resources = %d, want 1 (identical attribute sets dedupe)", got)
	}
	if got := byName["checkout"].Spans; got != 1 {
		t.Errorf("checkout spans = %d, want 1", got)
	}
	if n := len(byName["payments"].Request.ResourceSpans); n != 2 {
		t.Errorf("payments request holds %d resource groups, want 2", n)
	}
}

func TestSplitMissingServiceNameFoldsIntoEmptyWindow(t *testing.T) {
	req := &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{}, // no service.name
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{SpanId: []byte{1}, TraceId: []byte{1}, Name: "orphan"}},
		}},
	}}}
	splits := SplitTraces(req)
	if len(splits) != 1 || splits[0].ServiceName != "" {
		t.Fatalf("want one unnamed split, got %+v", splits)
	}
	if splits[0].Spans != 1 {
		t.Errorf("unnamed split spans = %d, want 1", splits[0].Spans)
	}
}

func TestSplitMetricsAndLogsCountRecords(t *testing.T) {
	mreq := &metricscolpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: resource("api"),
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Metrics: []*metricspb.Metric{
				{Name: "http.server.duration"},
				{Name: "http.server.request.size"},
			},
		}},
	}}}
	ms := SplitMetrics(mreq)
	if len(ms) != 1 || ms[0].Metrics != 2 || ms[0].Resources != 1 {
		t.Fatalf("metrics split = %+v", ms)
	}

	lreq := &logscolpb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: resource("api"),
		ScopeLogs: []*logspb.ScopeLogs{{
			LogRecords: []*logspb.LogRecord{
				{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "a"}}},
				{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "b"}}},
			},
		}},
	}}}
	ls := SplitLogs(lreq)
	if len(ls) != 1 || ls[0].Logs != 2 {
		t.Fatalf("logs split = %+v", ls)
	}
}

// ── service orchestration ────────────────────────────────────────────────────

func newTestService(t *testing.T, store Store) *Service {
	t.Helper()
	svc, err := New(store, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func TestIngestPersistsRowsAndEvaluates(t *testing.T) {
	store := newMemStore()
	svc := newTestService(t, store)

	req := &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: resource("payments", strKV("service.version", "1.2.3"), strKV("telemetry.sdk.name", "opentelemetry")),
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{SpanId: []byte{1}, TraceId: []byte{1}, Name: "charge"}},
		}},
	}}}
	if err := svc.IngestTraces(context.Background(), req); err != nil {
		t.Fatalf("IngestTraces: %v", err)
	}
	if store.appends != 1 {
		t.Fatalf("appends = %d, want 1", store.appends)
	}
	if len(store.window) != 1 || store.window[0].ServiceName != "payments" {
		t.Fatalf("window rows = %+v", store.window)
	}
	if store.window[0].Signal != "traces" || store.window[0].SpanCount != 1 {
		t.Fatalf("row = %+v", store.window[0])
	}
	// First sight evaluates immediately: verdicts + one score point exist.
	if _, ok := store.scores["payments"]; !ok {
		t.Fatal("no score persisted after first ingest")
	}
	if len(store.verdicts["payments"]) != len(instscore.Rules) {
		t.Fatalf("verdicts = %d, want one per registry rule (%d)",
			len(store.verdicts["payments"]), len(instscore.Rules))
	}
}

func TestIngestThrottlesReevaluation(t *testing.T) {
	store := newMemStore()
	svc := newTestService(t, store)
	base := time.Now()
	svc.now = func() time.Time { return base }

	req := &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   resource("api"),
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{SpanId: []byte{1}, TraceId: []byte{1}, Name: "r"}}}},
	}}}
	for i := 0; i < 3; i++ {
		if err := svc.IngestTraces(context.Background(), req); err != nil {
			t.Fatalf("IngestTraces %d: %v", i, err)
		}
	}
	if store.appends != 3 {
		t.Fatalf("appends = %d, want 3 (rows always persist)", store.appends)
	}
	if got := len(store.verdicts["api"]); got != len(instscore.Rules) {
		t.Fatalf("verdicts = %d, want exactly one per rule (re-eval throttled)", got)
	}

	// Past the throttle window, the next batch re-evaluates.
	svc.now = func() time.Time { return base.Add(2 * DefaultMinReeval) }
	if err := svc.IngestTraces(context.Background(), req); err != nil {
		t.Fatalf("IngestTraces after throttle: %v", err)
	}
	if got := len(store.verdicts["api"]); got != len(instscore.Rules) {
		t.Fatalf("verdicts after re-eval = %d, still one per rule", got)
	}
}

func TestEvaluationFailureDoesNotFailIngest(t *testing.T) {
	store := newMemStore()
	store.loadErr = context.DeadlineExceeded
	svc := newTestService(t, store)

	req := &logscolpb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  resource("api"),
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}},
	}}}
	if err := svc.IngestLogs(context.Background(), req); err != nil {
		t.Fatalf("ingest must not fail when the report-only follow-up does: %v", err)
	}
	if len(store.window) != 1 {
		t.Fatalf("rows = %d, want 1 (durable before evaluation)", len(store.window))
	}
}

func TestStoreAppendFailureFailsIngest(t *testing.T) {
	store := newMemStore()
	store.appendErr = context.Canceled
	svc := newTestService(t, store)
	req := &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   resource("api"),
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{SpanId: []byte{1}, TraceId: []byte{1}, Name: "r"}}}},
	}}}
	if err := svc.IngestTraces(context.Background(), req); err == nil {
		t.Fatal("ingest must fail when persistence fails (batch will be retried)")
	}
}

func TestWindowPruneRunsAtMostOncePerInterval(t *testing.T) {
	store := newMemStore()
	svc := newTestService(t, store)
	base := time.Now().Truncate(time.Second)
	svc.now = func() time.Time { return base }
	// Pre-seed an out-of-window row.
	store.window = append(store.window, WindowRow{
		ServiceName: "old", Signal: "traces", ReceivedAt: base.Add(-DefaultWindow - time.Hour),
	})

	req := &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   resource("api"),
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{SpanId: []byte{1}, TraceId: []byte{1}, Name: "r"}}}},
	}}}
	if err := svc.IngestTraces(context.Background(), req); err != nil {
		t.Fatalf("IngestTraces: %v", err)
	}
	if store.prunes != 1 {
		t.Fatalf("prunes = %d, want 1 on first ingest", store.prunes)
	}
	for _, r := range store.window {
		if r.ServiceName == "old" {
			t.Fatal("out-of-window row survived the prune")
		}
	}
	// Second ingest within pruneEvery does not prune again.
	svc.now = func() time.Time { return base.Add(time.Minute) }
	if err := svc.IngestTraces(context.Background(), req); err != nil {
		t.Fatalf("IngestTraces 2: %v", err)
	}
	if store.prunes != 1 {
		t.Fatalf("prunes = %d, want 1 (throttled)", store.prunes)
	}
}

// The stored evidence must round-trip through the engine's offline dump path:
// re-folding the persisted rows must reproduce the same window the batch
// produced — this is the contract between the tap's storage shape and
// instscore.LoadDump (phase-2 fixtures use the identical encoding).
func TestStoredEvidenceRefoldsToEngineWindow(t *testing.T) {
	store := newMemStore()
	svc := newTestService(t, store)

	traceReq := &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: resource("payments", strKV("telemetry.sdk.name", "opentelemetry"), strKV("telemetry.sdk.language", "go")),
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
			{SpanId: []byte{1}, TraceId: []byte{1}, Name: "charge", Kind: tracepb.Span_SPAN_KIND_SERVER},
		}}},
	}}}
	logReq := &logscolpb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: resource("payments"),
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			{SeverityText: "INFO", SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
				Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "charged"}}},
		}}},
	}}}
	if err := svc.IngestTraces(context.Background(), traceReq); err != nil {
		t.Fatalf("traces: %v", err)
	}
	if err := svc.IngestLogs(context.Background(), logReq); err != nil {
		t.Fatalf("logs: %v", err)
	}

	doc, ok := buildDump(store.window)
	if !ok {
		t.Fatal("no dump built from stored rows")
	}
	acc, err := instscore.LoadDump(doc)
	if err != nil {
		t.Fatalf("LoadDump of stored evidence: %v", err)
	}
	w := acc.Window("payments")
	if w == nil {
		t.Fatal("no payments window after re-fold")
	}
	if len(w.Spans) != 1 || len(w.Logs) != 1 {
		t.Fatalf("re-folded window spans=%d logs=%d, want 1/1", len(w.Spans), len(w.Logs))
	}
	if len(acc.Windows()) != 1 {
		t.Fatalf("re-fold produced %d windows, want 1 (rows are service-scoped)", len(acc.Windows()))
	}
}

// ── HTTP surface ─────────────────────────────────────────────────────────────

func TestHandlerAuthAndRouting(t *testing.T) {
	store := newMemStore()
	svc := newTestService(t, store)
	h := svc.Handler("seekrit")

	req := mustMarshal(t, &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   resource("api"),
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{SpanId: []byte{1}, TraceId: []byte{1}, Name: "r"}}}},
	}}})

	cases := []struct {
		name       string
		path       string
		method     string
		token      string
		contentTyp string
		body       []byte
		want       int
	}{
		{name: "no token", path: "/v1/traces", method: http.MethodPost, token: "", contentTyp: contentTypeProtobuf, body: req, want: http.StatusUnauthorized},
		{name: "wrong token", path: "/v1/traces", method: http.MethodPost, token: "nope", contentTyp: contentTypeProtobuf, body: req, want: http.StatusUnauthorized},
		{name: "wrong method", path: "/v1/traces", method: http.MethodGet, token: "seekrit", contentTyp: contentTypeProtobuf, want: http.StatusMethodNotAllowed},
		{name: "bad content type", path: "/v1/traces", method: http.MethodPost, token: "seekrit", contentTyp: "text/plain", body: req, want: http.StatusUnsupportedMediaType},
		{name: "garbage payload", path: "/v1/traces", method: http.MethodPost, token: "seekrit", contentTyp: contentTypeProtobuf, body: []byte("not-protobuf"), want: http.StatusBadRequest},
		{name: "unknown path", path: "/v1/profiles", method: http.MethodPost, token: "seekrit", contentTyp: contentTypeProtobuf, body: req, want: http.StatusNotFound},
		{name: "protobuf traces ok", path: "/v1/traces", method: http.MethodPost, token: "seekrit", contentTyp: contentTypeProtobuf, body: req, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.contentTyp != "" {
				r.Header.Set("Content-Type", tc.contentTyp)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
	if len(store.window) != 1 {
		t.Fatalf("persisted rows = %d, want 1 (only the ok case)", len(store.window))
	}
}

func TestHandlerJSONAndGzip(t *testing.T) {
	store := newMemStore()
	svc := newTestService(t, store)
	h := svc.Handler("seekrit")

	// JSON-encoded OTLP request (protojson, the same encoding the storage
	// layer persists) posted gzipped.
	jsonReq := &metricscolpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: resource("api"),
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "http.server.request.duration", Unit: "s"},
		}}},
	}}}
	body, err := protojson.Marshal(jsonReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	zw.Close()

	r := httptest.NewRequest(http.MethodPost, "/v1/metrics", &gz)
	r.Header.Set("Authorization", "Bearer seekrit")
	r.Header.Set("Content-Type", contentTypeJSON)
	r.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("response content type = %q, want %q", ct, contentTypeJSON)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(store.window) != 1 || store.window[0].Signal != "metrics" || store.window[0].MetricCount != 1 {
		t.Fatalf("rows = %+v", store.window)
	}
	// The persisted evidence is valid protojson of the scoped request.
	var check map[string]json.RawMessage
	if err := json.Unmarshal(store.window[0].Evidence, &check); err != nil {
		t.Fatalf("stored evidence is not JSON: %v", err)
	}
	if _, ok := check["resourceMetrics"]; !ok {
		t.Fatalf("stored evidence missing resourceMetrics: %s", store.window[0].Evidence)
	}
}

func TestHandlerOtlpResponseShape(t *testing.T) {
	store := newMemStore()
	svc := newTestService(t, store)
	h := svc.Handler("seekrit")

	req := mustMarshal(t, &logscolpb.ExportLogsServiceRequest{})
	r := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(req))
	r.Header.Set("Authorization", "Bearer seekrit")
	r.Header.Set("Content-Type", contentTypeProtobuf)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != contentTypeProtobuf {
		t.Errorf("response content type = %q", ct)
	}
	// The empty ExportLogsServiceResponse unmarshals cleanly (protocol-valid body).
	resp := &logscolpb.ExportLogsServiceResponse{}
	if len(w.Body.Bytes()) > 0 {
		if err := proto.Unmarshal(w.Body.Bytes(), resp); err != nil {
			t.Fatalf("response body is not a valid ExportLogsServiceResponse: %v", err)
		}
	}
}

// Empty (nil/zero) requests are a no-op, not an error: a collector heartbeat
// with no records must not mint evidence rows.
func TestEmptyRequestsAreNoOp(t *testing.T) {
	store := newMemStore()
	svc := newTestService(t, store)
	ctx := context.Background()
	if err := svc.IngestTraces(ctx, nil); err != nil {
		t.Fatalf("nil traces: %v", err)
	}
	if err := svc.IngestMetrics(ctx, &metricscolpb.ExportMetricsServiceRequest{}); err != nil {
		t.Fatalf("empty metrics: %v", err)
	}
	if err := svc.IngestLogs(ctx, nil); err != nil {
		t.Fatalf("nil logs: %v", err)
	}
	if len(store.window) != 0 || store.appends != 0 {
		t.Fatalf("rows=%d appends=%d, want 0/0", len(store.window), store.appends)
	}
}

// Guard: the store's evidence must never leak into logs (redaction ran
// upstream; the tap still treats telemetry payloads as data, not log lines).
func TestEvidenceNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	store := newMemStore()
	svc, err := New(store, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := &tracecolpb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: resource("api"),
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			SpanId: []byte{1}, TraceId: []byte{1}, Name: "charge-card-4242",
			Attributes: []*commonpb.KeyValue{strKV("card", "4242424242424242")},
		}}}},
	}}}
	if err := svc.IngestTraces(context.Background(), req); err != nil {
		t.Fatalf("IngestTraces: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "4242424242424242") || strings.Contains(out, "charge-card-4242") {
		t.Fatalf("telemetry values leaked into logs: %s", out)
	}
}

// mustMarshal is the test shorthand for a guaranteed proto encode.
func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	out, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}
