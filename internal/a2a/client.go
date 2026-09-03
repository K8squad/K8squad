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

// Package a2a is the core-side Agent-to-Agent client (arch §7.1, §11.2, story
// 5.1). It is the sanctioned southbound caller: given a resolved Agent Card,
// the core submits an A2A Task, streams the shim's sequenced SSE progress into
// the Run's live event surface (run_events, §10.1 — SSE live per ADR-040, not a
// Postgres table), collects artifact-refs, and settles on the task's terminal
// state — using ONLY the A2A wire (pkg/a2a) + the Agent Card, no lateral
// protocol (FR-D1, I4/NFR-EXT2).
//
// The concrete transport is pluggable behind Transport: the v1 shim set speaks
// stdio-framed JSONL (StdioTransport — the `shim run` sidecar contract), while
// EngineTransport drives an in-process shim for single-binary / dev /
// conformance use. External-spec revisions are pinned in internal/protocol and
// are never inlined here (story 5.3): a wire-rev bump changes an adapter, never
// this client.
package a2a

import (
	"context"
	"encoding/json"
	"fmt"

	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// EventSink receives every SSE event the shim emits for a dispatched task, in
// gap-free seq order (C4). The core's implementation fans it into the Run's
// live SSE surface (run_events) and the opt-in OTel export (§17.2). Delivery is
// at-least-once, so a sink MUST be idempotent on Event.Seq: the core dedups on
// (a2a_task_id, seq). A sink error aborts the dispatch (the live task is
// canceled) — a sink that wants to tolerate its own transient failures should
// swallow them internally rather than returning.
type EventSink interface {
	Event(ctx context.Context, ev wire.Event) error
}

// SinkFunc adapts a plain function to EventSink.
type SinkFunc func(ctx context.Context, ev wire.Event) error

// Event implements EventSink.
func (f SinkFunc) Event(ctx context.Context, ev wire.Event) error { return f(ctx, ev) }

// DiscardSink drops every event. It is the ledger-only / test sink and the
// default a nil sink resolves to.
var DiscardSink EventSink = SinkFunc(func(context.Context, wire.Event) error { return nil })

// Session is one submitted task's live connection: a gap-free SSE stream that
// closes when the task settles, the settled Status (valid once Events() is
// drained/closed), and an idempotent Cancel (C8). Close releases transport
// resources (a subprocess, a socket) and is safe to call more than once.
type Session interface {
	// Events is the task's sequenced SSE stream; it is closed exactly once,
	// when the task reaches a terminal state or the transport tears down.
	Events() <-chan wire.Event
	// Status is the task's current status; after Events() closes it is the
	// terminal status.
	Status() wire.Status
	// Cancel drains the live task to canceled; it is a no-op success on an
	// already-terminal task (C8).
	Cancel(ctx context.Context, reason string) error
	// Close releases transport resources; idempotent.
	Close() error
}

// Transport opens a Session for a submitted Task. Submit MUST be idempotent on
// Task.A2ATaskID: a re-submit reattaches to the in-flight task and never starts
// a second execution (C1).
type Transport interface {
	Submit(ctx context.Context, t wire.Task) (Session, error)
}

// Result is the outcome of following a dispatched task to terminal: its final
// status, the artifact-refs it emitted (already committed to coord.artifact by
// the shim — these are pointers, §5), and the last SSE seq delivered (the
// resume anchor, C4).
type Result struct {
	Status    wire.Status
	Artifacts []wire.ArtifactRef
	LastSeq   uint64
}

// Client is the core-side A2A caller over a Transport. It is stateless beyond
// the transport and safe for concurrent use if the transport is.
type Client struct {
	tr Transport
}

// New returns a Client that dispatches over tr.
func New(tr Transport) *Client { return &Client{tr: tr} }

// Begin submits t and returns its live Session without draining it. Callers
// that want the synchronous drive-to-terminal form should use Dispatch; Begin
// is the seam the async Dispatcher uses to return from the reconcile step once
// the task is accepted, then follow the stream in the background.
func (c *Client) Begin(ctx context.Context, t wire.Task) (Session, error) {
	if t.A2ATaskID == "" {
		return nil, fmt.Errorf("a2a: Begin requires a non-empty a2a_task_id")
	}
	sess, err := c.tr.Submit(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("a2a: submit task %s: %w", t.A2ATaskID, err)
	}
	return sess, nil
}

// Follow drains sess to terminal, forwarding every event to sink in gap-free
// seq order and collecting artifact-refs. It returns when the stream closes
// (terminal) or ctx is canceled (best-effort canceling the live task first so a
// dropped follow does not orphan a running shim). A nil sink discards events.
// Follow always Closes the session.
func (c *Client) Follow(ctx context.Context, sess Session, sink EventSink) (Result, error) {
	defer sess.Close()
	if sink == nil {
		sink = DiscardSink
	}
	var res Result
	for {
		select {
		case <-ctx.Done():
			// Detach the cancel so we still reach the shim after ctx died.
			_ = sess.Cancel(context.WithoutCancel(ctx), "context canceled")
			return res, ctx.Err()
		case ev, ok := <-sess.Events():
			if !ok {
				// A canceled context wins over a coincidental stream close: when
				// the caller drops the follow, the shim's stream may close in the
				// same scheduling window that ctx is canceled, and the select above
				// then picks either branch at random. Surface the cancellation
				// (and reach the shim to cancel the live task) rather than
				// reporting a clean terminal completion.
				if err := ctx.Err(); err != nil {
					_ = sess.Cancel(context.WithoutCancel(ctx), "context canceled")
					return res, err
				}
				res.Status = sess.Status()
				return res, nil
			}
			res.LastSeq = ev.Seq
			if ev.Type == wire.EventArtifactRef {
				if ar, ok := artifactRef(ev.Payload); ok {
					res.Artifacts = append(res.Artifacts, ar)
				}
			}
			if err := sink.Event(ctx, ev); err != nil {
				_ = sess.Cancel(context.WithoutCancel(ctx), "sink error")
				return res, fmt.Errorf("a2a: sink rejected event seq %d for task %s: %w", ev.Seq, ev.A2ATaskID, err)
			}
		}
	}
}

// Dispatch submits t, forwards its SSE progress to sink, collects artifact-refs,
// and returns once the task reaches a terminal state (C4). It is the synchronous
// drive-to-terminal form of story 5.1 (submit → stream → collect); the async
// reconcile-facing form is Dispatcher. A nil sink discards events.
func (c *Client) Dispatch(ctx context.Context, t wire.Task, sink EventSink) (Result, error) {
	sess, err := c.Begin(ctx, t)
	if err != nil {
		return Result{}, err
	}
	return c.Follow(ctx, sess, sink)
}

// artifactRef normalizes an EventArtifactRef payload into the typed ref. The
// in-process transport carries the concrete wire.ArtifactRef; the stdio
// transport decodes payloads as generic JSON, so we round-trip through JSON to
// recover the typed shape. A payload that carries no locating fields is ignored.
func artifactRef(payload any) (wire.ArtifactRef, bool) {
	switch v := payload.(type) {
	case wire.ArtifactRef:
		return v, true
	case *wire.ArtifactRef:
		if v == nil {
			return wire.ArtifactRef{}, false
		}
		return *v, true
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return wire.ArtifactRef{}, false
		}
		var ar wire.ArtifactRef
		if err := json.Unmarshal(b, &ar); err != nil {
			return wire.ArtifactRef{}, false
		}
		if ar.URI == "" && ar.SHA256 == "" && ar.Kind == "" {
			return wire.ArtifactRef{}, false
		}
		return ar, true
	}
}

// statusPayload normalizes an EventStatus payload into the typed StatusPayload,
// tolerating both the concrete in-process value and the JSON-decoded map form.
func statusPayload(payload any) (wire.StatusPayload, bool) {
	switch v := payload.(type) {
	case wire.StatusPayload:
		return v, true
	case *wire.StatusPayload:
		if v == nil {
			return wire.StatusPayload{}, false
		}
		return *v, true
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return wire.StatusPayload{}, false
		}
		var sp wire.StatusPayload
		if err := json.Unmarshal(b, &sp); err != nil {
			return wire.StatusPayload{}, false
		}
		if sp.State == "" {
			return wire.StatusPayload{}, false
		}
		return sp, true
	}
}
