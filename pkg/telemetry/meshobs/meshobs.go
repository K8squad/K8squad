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

// Package meshobs is the pod-side AgentMesh lifecycle telemetry observer
// (chapter §8.2/§8.3, AgentMesh P1.2 — ISI-5823). It implements the generic
// pkg/mesh.LifecycleObserver seam on the k8squad OTel spine:
//
//   - mesh.route / mesh.spawn spans (registry: pkg/telemetry/semconv — the
//     conformance test drives THIS package, so the registry contract and the
//     emitter cannot drift),
//   - the ksquad.mesh.* latency/count instruments that prove the lifecycle
//     SLOs: a2a.ready_latency (A2A arrival → process-ready, the ≤3 s
//     recreate-from-envelope target, D4.1) and spawn.latency{source} (envelope
//     vs cold).
//
// The generic mesh gateway reports through the seam and stays free of any
// telemetry SDK (extraction contract, chapter §1A); this package is the
// k8squad-side plug. Hibernation instruments are registered but stay at zero
// in P1 — REV-4 cut the SIGSTOP tier from the batch path (S4, ISI-5831):
// hibernate.misses_total counts had-to-recreate routes (the P1.2 signal);
// hits wake-from-hibernate and never increments until S4.
//
// Cardinality (NFR-OBS3 firewall): every label is bounded — team and role are
// the pool identity, source ∈ {envelope, cold}, state is the fixed §6 enum.
// Endpoint ids and run ids are span attributes only, never metric labels.
package meshobs

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/K8squad/K8squad/pkg/mesh"
	ksqsemconv "github.com/K8squad/K8squad/pkg/telemetry/semconv"
)

// Instrument names (OTel dot-form; exported to Prometheus as ksquad_*).
const (
	readyLatencyName  = "ksquad.mesh.a2a.ready_latency"
	spawnLatencyName  = "ksquad.mesh.spawn.latency"
	hibernateHitsName = "ksquad.mesh.hibernate.hits_total"
	hibernateMissName = "ksquad.mesh.hibernate.misses_total"
	processesGauge    = "ksquad.mesh.processes.active"
)

// Bounded label keys (chapter §8.3 D6.2: {team, role, state} / {team, role,
// source} — nothing wider).
const (
	attrTeam   = "team"
	attrRole   = "role"
	attrState  = "state"
	attrSource = "source"
)

// Labels is the pool identity stamped on every span and metric the observer
// emits — the bounded {team, role, pod} routing identity of the gateway.
type Labels struct {
	Team string
	Role string
	Pod  string
}

// Observer implements mesh.LifecycleObserver on OTel. Construct with New.
type Observer struct {
	tracer trace.Tracer
	labels Labels

	readyLatency  metric.Float64Histogram
	spawnLatency  metric.Float64Histogram
	hibernateHits metric.Int64Counter
	hibernateMiss metric.Int64Counter
}

// New builds the observer on the given tracer (mesh.route/mesh.spawn spans)
// and meter (ksquad.mesh.* instruments). nil tracer/meter degrade to the OTel
// globals' no-ops — the gateway never crashes for telemetry's sake.
func New(tracer trace.Tracer, meter metric.Meter, labels Labels) *Observer {
	if tracer == nil {
		tracer = noop.NewTracerProvider().Tracer("")
	}
	o := &Observer{tracer: tracer, labels: labels}
	if meter != nil {
		// On construction error OTel hands back a no-op instrument, so a
		// meter dispute can never take the gateway down.
		o.readyLatency, _ = meter.Float64Histogram(readyLatencyName,
			metric.WithUnit("ms"), metric.WithDescription("Mesh route: A2A arrival → process-ready, ms. The ≤3s recreate-from-envelope SLO window (D4.1)."))
		o.spawnLatency, _ = meter.Float64Histogram(spawnLatencyName,
			metric.WithUnit("ms"), metric.WithDescription("Standing-process spawn/recreate wall-clock, ms, by source (envelope = recreate path, cold = pool boot)."))
		o.hibernateHits, _ = meter.Int64Counter(hibernateHitsName,
			metric.WithDescription("Routes served by waking a hibernated process. Zero in P1 — hibernation ships at S4 (ISI-5831, REV-4)."))
		o.hibernateMiss, _ = meter.Int64Counter(hibernateMissName,
			metric.WithDescription("Routes that had to recreate from the envelope (no idle standing process) — the RAM-reclaim trade paying out."))
	}
	return o
}

// poolAttrs are the mesh-identity span attributes every mesh span carries
// (registry meshIdentity(): ksquad.team.name / ksquad.mesh.role / ksquad.mesh.pod).
func (o *Observer) poolAttrs(endpointID string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("ksquad.team.name", o.labels.Team),
		attribute.String("ksquad.mesh.role", o.labels.Role),
		attribute.String("ksquad.mesh.pod", o.labels.Pod),
		attribute.String("ksquad.mesh.endpoint", endpointID),
	}
}

// outcome maps an event error to the bounded outcome vocabulary.
func outcome(err error) string {
	switch err {
	case nil:
		return "success"
	default:
		return "error"
	}
}

// ObserveRoute emits one mesh.route span (nested under the dispatch request's
// trace context — §8.2: a routed run's trace stays continuous from board to
// LLM call) and records the ready-latency histogram point that backs the ≤3s
// recreate SLO.
func (o *Observer) ObserveRoute(ctx context.Context, e mesh.RouteEvent) {
	attrs := append(o.poolAttrs(e.EndpointID),
		attribute.String("ksquad.mesh.selection", string(e.Selection)),
		attribute.Int("ksquad.duration.ms", durationMS(e.Duration)),
		attribute.String("ksquad.outcome", outcome(e.Err)),
	)
	_, span := o.tracer.Start(ctx, ksqsemconv.SpanMeshRoute, trace.WithAttributes(attrs...))
	if e.Err != nil {
		span.SetStatus(codes.Error, e.Err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()

	if o.readyLatency != nil {
		o.readyLatency.Record(ctx, float64(durationMS(e.Duration)),
			metric.WithAttributes(
				attribute.String(attrTeam, o.labels.Team),
				attribute.String(attrRole, o.labels.Role),
			))
	}
}

// ObserveSpawn emits one mesh.spawn span (nested under the triggering
// mesh.route on the recreate path) and records the spawn-latency histogram
// point by source. A recreate (source=envelope) also increments
// hibernate.misses_total — the "had-to-recreate" counter of the §8.3 set.
func (o *Observer) ObserveSpawn(ctx context.Context, e mesh.SpawnEvent) {
	attrs := append(o.poolAttrs(e.EndpointID),
		attribute.String("ksquad.mesh.spawn.source", string(e.Source)),
		attribute.Int("ksquad.duration.ms", durationMS(e.Duration)),
		attribute.String("ksquad.outcome", outcome(e.Err)),
	)
	_, span := o.tracer.Start(ctx, ksqsemconv.SpanMeshSpawn, trace.WithAttributes(attrs...))
	if e.Err != nil {
		span.SetStatus(codes.Error, e.Err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()

	if o.spawnLatency != nil {
		o.spawnLatency.Record(ctx, float64(durationMS(e.Duration)),
			metric.WithAttributes(
				attribute.String(attrTeam, o.labels.Team),
				attribute.String(attrRole, o.labels.Role),
				attribute.String(attrSource, string(e.Source)),
			))
	}
	if o.hibernateMiss != nil && e.Err == nil && e.Source == mesh.SpawnSourceEnvelope {
		o.hibernateMiss.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String(attrTeam, o.labels.Team),
				attribute.String(attrRole, o.labels.Role),
			))
	}
}

// RegisterProcessesGauge registers the ksquad.mesh.processes.active gauge
// (§8.3: standing processes by state, bounded {team, role, state} labels).
// snapshot is invoked on every collection cycle and MUST be cheap and
// non-blocking — back it with the gateway's in-memory Processes() bucketed by
// state, never a live call into a process.
func RegisterProcessesGauge(meter metric.Meter, team, role string, snapshot func() map[mesh.ProcState]int64) (metric.Registration, error) {
	if meter == nil {
		return nil, nil
	}
	gauge, err := meter.Int64ObservableGauge(processesGauge,
		metric.WithDescription("Mesh pool slots by lifecycle state (active|idle|hibernated|stopped). Stopped slots are recreate-on-demand capacity."))
	if err != nil {
		return nil, err
	}
	return meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for state, n := range snapshot() {
			o.ObserveInt64(gauge, n,
				metric.WithAttributes(
					attribute.String(attrTeam, team),
					attribute.String(attrRole, role),
					attribute.String(attrState, string(state)),
				))
		}
		return nil
	}, gauge)
}

// durationMS truncates a duration to whole milliseconds — the registry's
// ksquad.duration.ms type is int.
func durationMS(d time.Duration) int {
	return int(d.Milliseconds())
}
