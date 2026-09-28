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

package telemetry

import (
	"context"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// AttrReconcileNoop marks a run.reconcile span whose reconcile pass advanced no
// durable state — a poll-loop tick the drive loop stamps so the export pipeline
// drops it (ISI-5145). Before this, a Run in flight opened one run.reconcile
// span every ~2s requeue regardless of change (a live trace held 302 of them),
// drowning the O(state-transition) spans that actually describe the Run's flow.
// The poll CADENCE stays observable as the count of the low-cardinality
// ksquad.controller.reconcile.duration{controller} histogram (cphealth records
// it every pass); the per-tick SPAN is the noise this attribute suppresses.
// Kept low-cardinality — a bounded bool, never a join key.
const AttrReconcileNoop = "ksquad.reconcile.noop"

// reconcileSpanName is the drive loop's per-pass span (pkg/controller/rundrive).
const reconcileSpanName = "run.reconcile"

// dropNoopReconcile wraps the export SpanProcessor and drops the poll-loop-noise
// run.reconcile spans the drive loop marked with AttrReconcileNoop=true. Those
// spans are childless LEAVES — the child spans of a reconcile pass
// (contextasm.assemble, the coord phase-transition marker) are only opened on
// the passes that DO advance the machine — so dropping one orphans nothing. A
// pass that errored or advanced never carries the mark, so its span (and any
// children) exports unchanged.
type dropNoopReconcile struct{ next sdktrace.SpanProcessor }

func (p dropNoopReconcile) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	p.next.OnStart(parent, s)
}

func (p dropNoopReconcile) OnEnd(s sdktrace.ReadOnlySpan) {
	if isNoopReconcile(s) {
		return
	}
	p.next.OnEnd(s)
}

func (p dropNoopReconcile) Shutdown(ctx context.Context) error   { return p.next.Shutdown(ctx) }
func (p dropNoopReconcile) ForceFlush(ctx context.Context) error { return p.next.ForceFlush(ctx) }

// isNoopReconcile reports whether s is a run.reconcile span the drive loop
// stamped as a changeless poll tick.
func isNoopReconcile(s sdktrace.ReadOnlySpan) bool {
	if s.Name() != reconcileSpanName {
		return false
	}
	for _, kv := range s.Attributes() {
		if string(kv.Key) == AttrReconcileNoop {
			return kv.Value.AsBool()
		}
	}
	return false
}
