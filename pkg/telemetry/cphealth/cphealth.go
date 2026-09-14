// Package cphealth registers the operator's control-plane health instruments on
// the process OTel meter (ADR-0021 WS-E / decision D5, ISI-4384).
//
// Before this package the operator's native OTel meter had ZERO instruments:
// telemetry.Meter() was wired to a MeterProvider + OTLP exporter, but no code
// ever created a counter/histogram/gauge on it, so the metrics leg exported
// nothing to the gateway/Dynatrace — the "operator meter has no instruments"
// gap (ISI-4102/4128). Every existing ksquad_* series lives on the SEPARATE
// controller-runtime Prometheus registry (scraped at /metrics), which never
// reaches the OTLP push path. Registering the D5 health instruments here both
// resolves that gap and delivers the control-plane health surface:
//
//	ksquad.runs.active{phase,team}          gauge      active Runs by phase + owning team
//	ksquad.dependency.up{dep}               gauge      1/0 liveness of postgres|nats|apiserver
//	ksquad.controller.reconcile.duration{controller}  histogram (s)  per-controller reconcile latency
//	ksquad.controller.reconcile.errors{controller}    counter        per-controller reconcile errors
//
// Cardinality (NFR-OBS3 firewall): every label is BOUNDED — phase is the fixed
// §8 enum, team is the tenant set, dep is a fixed 3-element set, controller is
// the fixed controller name. run.id / work-item / ticket are NEVER metric
// labels; those stay on spans and events. (When the OTLP metrics pipeline is
// bridged to Prometheus these become ksquad_runs_active, ksquad_dependency_up,
// etc. — the same names the ADR's D5 spec uses.)
package cphealth

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Instrument names (OTel dot-delimited; exported to Prometheus as ksquad_*).
const (
	runsActiveName        = "ksquad.runs.active"
	dependencyUpName      = "ksquad.dependency.up"
	reconcileDurationName = "ksquad.controller.reconcile.duration"
	reconcileErrorsName   = "ksquad.controller.reconcile.errors"
)

// Bounded attribute keys.
const (
	attrPhase      = "phase"
	attrTeam       = "team"
	attrDep        = "dep"
	attrController = "controller"
)

const (
	defaultProbeInterval = 15 * time.Second
	defaultProbeTimeout  = 5 * time.Second
)

// depUnknown is the sentinel for a dependency not yet probed: the observable
// gauge skips it so a slow first probe never reports a misleading 0 ("down").
const depUnknown int64 = -1

// RunSnapshot is one bucket of the runs.active gauge: the count of active Runs
// in a given phase owned by a given team. The caller buckets the Run cache into
// these so cphealth never touches the Kubernetes API itself.
type RunSnapshot struct {
	Phase string
	Team  string
	Count int64
}

// Dependency is a named external dependency and its liveness probe. Probe must
// return promptly (it runs under a bounded timeout on the probe loop) and true
// only when the dependency is reachable and healthy.
type Dependency struct {
	Name  string
	Probe func(context.Context) bool
}

// Options configures Register.
type Options struct {
	// ActiveRuns returns the current active-Run buckets by (phase, team). It is
	// invoked on every metric collection cycle, so it must be cheap and
	// non-blocking — back it with an in-memory informer-cache lister, never a
	// live API/DB call. Nil disables the runs.active gauge.
	ActiveRuns func(context.Context) []RunSnapshot

	// Dependencies are probed off the collection path on a background ticker;
	// the gauge reports the last cached result. Empty disables dependency.up.
	Dependencies []Dependency

	// ProbeInterval is how often dependencies are probed (default 15s).
	ProbeInterval time.Duration
	// ProbeTimeout bounds a single probe (default 5s).
	ProbeTimeout time.Duration
}

// Metrics holds the registered control-plane health instruments and the cached
// dependency state. The zero value is not usable; construct with Register.
type Metrics struct {
	reconcileDuration metric.Float64Histogram
	reconcileErrors   metric.Int64Counter

	deps          []Dependency
	probeInterval time.Duration
	probeTimeout  time.Duration

	mu       sync.RWMutex
	depState map[string]int64 // dep name -> 1 up / 0 down / depUnknown
}

// Register creates the D5 control-plane health instruments on meter and returns
// a handle for recording reconcile outcomes and running the dependency prober.
// Registering these instruments is itself the fix for the operator meter having
// no instruments (ISI-4102/4128).
func Register(meter metric.Meter, opts Options) (*Metrics, error) {
	m := &Metrics{
		deps:          opts.Dependencies,
		probeInterval: opts.ProbeInterval,
		probeTimeout:  opts.ProbeTimeout,
		depState:      make(map[string]int64, len(opts.Dependencies)),
	}
	if m.probeInterval <= 0 {
		m.probeInterval = defaultProbeInterval
	}
	if m.probeTimeout <= 0 {
		m.probeTimeout = defaultProbeTimeout
	}
	for _, d := range opts.Dependencies {
		m.depState[d.Name] = depUnknown
	}

	var err error
	if m.reconcileDuration, err = meter.Float64Histogram(
		reconcileDurationName,
		metric.WithUnit("s"),
		metric.WithDescription("Per-controller reconcile latency (WS-E/D5)."),
	); err != nil {
		return nil, err
	}
	if m.reconcileErrors, err = meter.Int64Counter(
		reconcileErrorsName,
		metric.WithDescription("Per-controller reconcile errors (WS-E/D5)."),
	); err != nil {
		return nil, err
	}

	if opts.ActiveRuns != nil {
		activeRuns := opts.ActiveRuns
		if _, err = meter.Int64ObservableGauge(
			runsActiveName,
			metric.WithDescription("Active Runs by phase and owning team (WS-E/D5)."),
			metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
				for _, s := range activeRuns(ctx) {
					o.Observe(s.Count, metric.WithAttributes(
						attribute.String(attrPhase, s.Phase),
						attribute.String(attrTeam, s.Team),
					))
				}
				return nil
			}),
		); err != nil {
			return nil, err
		}
	}

	if len(opts.Dependencies) > 0 {
		if _, err = meter.Int64ObservableGauge(
			dependencyUpName,
			metric.WithDescription("Liveness (1=up, 0=down) of each control-plane dependency (WS-E/D5)."),
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
				m.mu.RLock()
				defer m.mu.RUnlock()
				for name, up := range m.depState {
					if up == depUnknown {
						continue // not yet probed: emitting 0 would falsely read as "down"
					}
					o.Observe(up, metric.WithAttributes(attribute.String(attrDep, name)))
				}
				return nil
			}),
		); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// ObserveReconcile records one reconcile outcome: its duration always, plus a
// reconcile-error increment when err != nil. Nil-safe so callers need not guard.
func (m *Metrics) ObserveReconcile(ctx context.Context, controller string, d time.Duration, err error) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String(attrController, controller))
	m.reconcileDuration.Record(ctx, d.Seconds(), attrs)
	if err != nil {
		m.reconcileErrors.Add(ctx, 1, attrs)
	}
}

// Start runs the dependency probe loop until ctx is cancelled: it probes once
// immediately, then every ProbeInterval, caching each result for the gauge. The
// signature satisfies sigs.k8s.io/controller-runtime manager.Runnable, so it can
// be added to the manager with mgr.Add(m). Nil-safe.
func (m *Metrics) Start(ctx context.Context) error {
	if m == nil || len(m.deps) == 0 {
		<-ctx.Done()
		return nil
	}
	m.probeAll(ctx)
	t := time.NewTicker(m.probeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			m.probeAll(ctx)
		}
	}
}

func (m *Metrics) probeAll(ctx context.Context) {
	for _, d := range m.deps {
		pctx, cancel := context.WithTimeout(ctx, m.probeTimeout)
		up := int64(0)
		if d.Probe(pctx) {
			up = 1
		}
		cancel()
		m.mu.Lock()
		m.depState[d.Name] = up
		m.mu.Unlock()
	}
}

// WrapReconciler decorates r so every Reconcile call records its latency and (on
// error) an error increment under the given controller label. Returns r
// unchanged when m is nil, so wiring stays a no-op when health metrics are off.
func WrapReconciler(m *Metrics, controller string, r reconcile.Reconciler) reconcile.Reconciler {
	if m == nil {
		return r
	}
	return &timedReconciler{m: m, controller: controller, inner: r}
}

type timedReconciler struct {
	m          *Metrics
	controller string
	inner      reconcile.Reconciler
}

func (t *timedReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	start := time.Now()
	res, err := t.inner.Reconcile(ctx, req)
	t.m.ObserveReconcile(ctx, t.controller, time.Since(start), err)
	return res, err
}
