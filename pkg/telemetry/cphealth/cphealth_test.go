package cphealth

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// newTestMeter returns a meter backed by a ManualReader so tests can Collect the
// exact instruments cphealth registers — the same reader shape the real OTLP
// MeterProvider uses, so "no instruments" would surface here as an empty scope.
func newTestMeter(t *testing.T) (metric.Meter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp.Meter("cphealth-test"), reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func attrVal(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.AsString()
}

// TestRegisterCreatesInstruments is the direct ISI-4102/4128 regression: after
// Register the meter's reader must yield the four D5 instruments, not an empty
// scope ("meter has no instruments").
func TestRegisterCreatesInstruments(t *testing.T) {
	meter, reader := newTestMeter(t)
	m, err := Register(meter, Options{
		ActiveRuns: func(context.Context) []RunSnapshot {
			return []RunSnapshot{{Phase: "Running", Team: "alpha", Count: 2}}
		},
		Dependencies: []Dependency{{Name: "postgres", Probe: func(context.Context) bool { return true }}},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Prime the dependency cache (the gauge suppresses un-probed deps).
	m.probeAll(context.Background())
	// Exercise the sync instruments so they materialize a data point.
	m.ObserveReconcile(context.Background(), "run-drive", 42*time.Millisecond, errors.New("boom"))

	got := collect(t, reader)
	for _, name := range []string{runsActiveName, dependencyUpName, reconcileDurationName, reconcileErrorsName} {
		if _, ok := got[name]; !ok {
			t.Errorf("instrument %q not exported (meter still has no instruments?)", name)
		}
	}
}

func TestRunsActiveGaugeLabels(t *testing.T) {
	meter, reader := newTestMeter(t)
	_, err := Register(meter, Options{
		ActiveRuns: func(context.Context) []RunSnapshot {
			return []RunSnapshot{
				{Phase: "Running", Team: "alpha", Count: 3},
				{Phase: "Claiming", Team: "beta", Count: 1},
			}
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	g, ok := collect(t, reader)[runsActiveName].Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("runs.active is not an int64 gauge: %T", collect(t, reader)[runsActiveName].Data)
	}
	byKey := map[string]int64{}
	for _, dp := range g.DataPoints {
		byKey[attrVal(dp.Attributes, attrPhase)+"/"+attrVal(dp.Attributes, attrTeam)] = dp.Value
	}
	if byKey["Running/alpha"] != 3 || byKey["Claiming/beta"] != 1 {
		t.Errorf("runs.active buckets = %v, want Running/alpha=3 Claiming/beta=1", byKey)
	}
}

func TestDependencyUpReflectsProbe(t *testing.T) {
	meter, reader := newTestMeter(t)
	pgUp := true
	m, err := Register(meter, Options{
		Dependencies: []Dependency{
			{Name: "postgres", Probe: func(context.Context) bool { return pgUp }},
			{Name: "nats", Probe: func(context.Context) bool { return false }},
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Before any probe the gauge suppresses unknown deps: no data points.
	if g, ok := collect(t, reader)[dependencyUpName].Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) != 0 {
		t.Errorf("un-probed dependency.up emitted %d points, want 0", len(g.DataPoints))
	}

	m.probeAll(context.Background())
	g := collect(t, reader)[dependencyUpName].Data.(metricdata.Gauge[int64])
	byDep := map[string]int64{}
	for _, dp := range g.DataPoints {
		byDep[attrVal(dp.Attributes, attrDep)] = dp.Value
	}
	if byDep["postgres"] != 1 || byDep["nats"] != 0 {
		t.Errorf("dependency.up = %v, want postgres=1 nats=0", byDep)
	}

	// Flip postgres down; the cached state must follow on the next probe.
	pgUp = false
	m.probeAll(context.Background())
	g = collect(t, reader)[dependencyUpName].Data.(metricdata.Gauge[int64])
	for _, dp := range g.DataPoints {
		if attrVal(dp.Attributes, attrDep) == "postgres" && dp.Value != 0 {
			t.Errorf("postgres dependency.up = %d after going down, want 0", dp.Value)
		}
	}
}

func TestObserveReconcileCountsErrorsOnly(t *testing.T) {
	meter, reader := newTestMeter(t)
	m, err := Register(meter, Options{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	m.ObserveReconcile(context.Background(), "run-drive", 10*time.Millisecond, nil)
	m.ObserveReconcile(context.Background(), "run-drive", 20*time.Millisecond, errors.New("x"))
	m.ObserveReconcile(context.Background(), "run", 5*time.Millisecond, errors.New("y"))

	got := collect(t, reader)
	hist := got[reconcileDurationName].Data.(metricdata.Histogram[float64])
	counts := map[string]uint64{}
	for _, dp := range hist.DataPoints {
		counts[attrVal(dp.Attributes, attrController)] = dp.Count
	}
	if counts["run-drive"] != 2 || counts["run"] != 1 {
		t.Errorf("reconcile.duration counts = %v, want run-drive=2 run=1", counts)
	}

	sum := got[reconcileErrorsName].Data.(metricdata.Sum[int64])
	errCounts := map[string]int64{}
	for _, dp := range sum.DataPoints {
		errCounts[attrVal(dp.Attributes, attrController)] = dp.Value
	}
	if errCounts["run-drive"] != 1 || errCounts["run"] != 1 {
		t.Errorf("reconcile.errors = %v, want run-drive=1 run=1", errCounts)
	}
}

func TestWrapReconcilerRecords(t *testing.T) {
	meter, reader := newTestMeter(t)
	m, err := Register(meter, Options{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	inner := reconcile.Func(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		return reconcile.Result{}, errors.New("reconcile failed")
	})
	wrapped := WrapReconciler(m, "run-drive", inner)
	if _, err := wrapped.Reconcile(context.Background(), reconcile.Request{}); err == nil {
		t.Fatal("wrapped reconciler swallowed the inner error")
	}

	sum := collect(t, reader)[reconcileErrorsName].Data.(metricdata.Sum[int64])
	if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 {
		t.Errorf("WrapReconciler did not record one error: %+v", sum.DataPoints)
	}
}

func TestWrapReconcilerNilMetricsIsPassthrough(t *testing.T) {
	inner := reconcile.Func(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		return reconcile.Result{Requeue: true}, nil
	})
	if got := WrapReconciler(nil, "x", inner); got == nil {
		t.Fatal("WrapReconciler(nil,...) returned nil")
	}
	// A nil *Metrics must not panic through the recording path either.
	var m *Metrics
	m.ObserveReconcile(context.Background(), "x", time.Second, nil)
}
