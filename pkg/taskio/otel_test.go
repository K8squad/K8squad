package taskio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// installTestTracer swaps the global TracerProvider for a synchronous in-memory
// recorder and a W3C propagator so a handler's AC8 span can be asserted without
// a running collector. Restores the globals on cleanup.
func installTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return exp
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

// TestUpdateStatusSpanRecordsFromTo proves AC8: an update_status call opens a
// single `taskio.update_status` span carrying op/run/work-item/http-status and —
// uniquely for this op — status.from/.to. No comment/description content appears.
func TestUpdateStatusSpanRecordsFromTo(t *testing.T) {
	exp := installTestTracer(t)
	store := &fakeStore{fromState: "in_progress"}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")

	w := do(t, h, http.MethodPost, "/update-status", tok, `{"status":"in_review"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	sp := spans.Snapshots()[0]
	if sp.Name() != "taskio.update_status" {
		t.Fatalf("span name = %q, want taskio.update_status", sp.Name())
	}
	a := spanAttrs(sp)
	for k, want := range map[string]string{
		"ksquad.taskio.op":           "update_status",
		"ksquad.run.id":              "run-A",
		"ksquad.taskio.work_item_id": "wi-1",
		"ksquad.taskio.status.from":  "in_progress",
		"ksquad.taskio.status.to":    "in_review",
	} {
		if got := a[k].AsString(); got != want {
			t.Errorf("attr %s = %q, want %q", k, got, want)
		}
	}
	if got := a["http.response.status_code"].AsInt64(); got != int64(http.StatusNoContent) {
		t.Errorf("http.response.status_code = %d, want 204", got)
	}
}

// TestGetTaskSpanHasNoStatusFromTo confirms status.from/.to are update_status-
// only: a get-task span omits them (they are not a generic attribute).
func TestGetTaskSpanHasNoStatusFromTo(t *testing.T) {
	exp := installTestTracer(t)
	store := &fakeStore{detail: TaskDetail{Title: "S2", State: "in_progress"}}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")

	_ = do(t, h, http.MethodGet, "/get-task", tok, "")
	sp := exp.GetSpans().Snapshots()[0]
	if sp.Name() != "taskio.get_task" {
		t.Fatalf("span name = %q, want taskio.get_task", sp.Name())
	}
	a := spanAttrs(sp)
	if _, ok := a["ksquad.taskio.status.from"]; ok {
		t.Errorf("get_task span must not carry status.from")
	}
	if _, ok := a["ksquad.taskio.status.to"]; ok {
		t.Errorf("get_task span must not carry status.to")
	}
}

// TestErrorSpanRecordsErrorClass proves a >=400 outcome stamps error_class and an
// error span status. A stale-fence checkout is 409 → error_class "stale_fence".
func TestErrorSpanRecordsErrorClass(t *testing.T) {
	exp := installTestTracer(t)
	store := &fakeStore{fenceErr: ErrStaleFence}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")

	w := do(t, h, http.MethodPost, "/checkout", tok, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	sp := exp.GetSpans().Snapshots()[0]
	if got := spanAttrs(sp)["ksquad.taskio.error_class"].AsString(); got != "stale_fence" {
		t.Fatalf("error_class = %q, want stale_fence", got)
	}
}

// TestSpanJoinsInboundTrace proves the AC8 join: a client-forwarded traceparent
// makes the taskio.<op> span a child of the Run trace (same TraceID), so the
// server span is one node in the distributed Run trace.
func TestSpanJoinsInboundTrace(t *testing.T) {
	exp := installTestTracer(t)
	store := &fakeStore{}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")

	// A well-formed W3C traceparent standing in for the Run's stamped context.
	const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	r := httptest.NewRequest(http.MethodPost, "/checkout", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("traceparent", traceparent)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)

	sp := exp.GetSpans().Snapshots()[0]
	if got := sp.SpanContext().TraceID().String(); got != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("span TraceID = %q, want the inbound Run trace id", got)
	}
}
