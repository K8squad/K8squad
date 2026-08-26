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

package a2a_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	clienta2a "github.com/K8squad/K8squad/internal/a2a"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// pauseCall records one Pauser invocation.
type pauseCall struct {
	item       int
	retryAfter *time.Duration
}

// switchCall records one SwitchExecutor invocation.
type switchCall struct {
	runID string
	plan  modelendpoint.SwitchPlan
}

// rlFixture assembles a RateLimitBridge over recording seams. agent drives the
// SwitchModel vs PauseRun verdict: a nil FallbackModel → PauseRun; a set one
// (with no BYO endpoint ref) → SwitchModel, resolved with no client.Reader.
type rlFixture struct {
	pauses   []pauseCall
	switches []switchCall
	bridge   *clienta2a.RateLimitBridge
}

func newRLFixture(agent *api.Agent, workItem int, resolveOK bool, resolveErr error) *rlFixture {
	f := &rlFixture{}
	f.bridge = &clienta2a.RateLimitBridge{
		Switcher: &modelendpoint.Switcher{Resolver: &modelendpoint.Resolver{}},
		Resolve: func(_ context.Context, ev wire.Event) (clienta2a.RunContext, bool, error) {
			if resolveErr != nil {
				return clienta2a.RunContext{}, false, resolveErr
			}
			return clienta2a.RunContext{Agent: agent, WorkItem: workItem, RunID: "run-" + ev.A2ATaskID}, resolveOK, nil
		},
		Pause: func(_ context.Context, item int, ra *time.Duration) error {
			f.pauses = append(f.pauses, pauseCall{item: item, retryAfter: ra})
			return nil
		},
		OnSwitch: func(_ context.Context, runID string, plan modelendpoint.SwitchPlan) error {
			f.switches = append(f.switches, switchCall{runID: runID, plan: plan})
			return nil
		},
	}
	return f
}

// agentNoFallback → PauseRun path. agentWithFallback → SwitchModel path.
func agentNoFallback() *api.Agent {
	return &api.Agent{Spec: api.AgentSpec{Model: "claude-primary"}}
}

func agentWithFallback() *api.Agent {
	return &api.Agent{Spec: api.AgentSpec{
		Model:         "claude-primary",
		FallbackModel: &api.FallbackModel{Model: "claude-fallback"},
	}}
}

func rlEvent(payload wire.RateLimitedPayload, ts time.Time) wire.Event {
	return wire.Event{
		Seq:       7,
		A2ATaskID: "task-1",
		TS:        ts,
		Type:      wire.EventRateLimited,
		Payload:   payload,
	}
}

func TestRateLimitBridge_PauseWithProviderWindow(t *testing.T) {
	f := newRLFixture(agentNoFallback(), 42, true, nil)
	ev := rlEvent(wire.RateLimitedPayload{Model: "claude-primary", RetryAfter: "120"}, time.Now())

	if err := f.bridge.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.pauses) != 1 {
		t.Fatalf("want 1 pause, got %d", len(f.pauses))
	}
	got := f.pauses[0]
	if got.item != 42 {
		t.Errorf("pause item = %d, want 42", got.item)
	}
	if got.retryAfter == nil || *got.retryAfter != 120*time.Second {
		t.Errorf("pause retryAfter = %v, want 120s", got.retryAfter)
	}
	if len(f.switches) != 0 {
		t.Errorf("want no switch, got %d", len(f.switches))
	}
}

func TestRateLimitBridge_PauseNoWindowIsBackoff(t *testing.T) {
	f := newRLFixture(agentNoFallback(), 9, true, nil)
	// Empty Retry-After → 0 window → nil retryAfter → coord.Pause backoff path.
	ev := rlEvent(wire.RateLimitedPayload{Model: "claude-primary"}, time.Now())

	if err := f.bridge.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.pauses) != 1 {
		t.Fatalf("want 1 pause, got %d", len(f.pauses))
	}
	if f.pauses[0].retryAfter != nil {
		t.Errorf("pause retryAfter = %v, want nil (backoff)", *f.pauses[0].retryAfter)
	}
}

func TestRateLimitBridge_HTTPDateWindowAgainstEventClock(t *testing.T) {
	f := newRLFixture(agentNoFallback(), 1, true, nil)
	base := time.Date(2015, 10, 21, 7, 28, 0, 0, time.UTC)
	// Retry-After is an HTTP-date 90s after the event's TS; the window must be
	// resolved against ev.TS (consumer clock anchored to the event), = 90s.
	httpDate := base.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	ev := rlEvent(wire.RateLimitedPayload{Model: "claude-primary", RetryAfter: httpDate}, base)

	if err := f.bridge.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.pauses) != 1 || f.pauses[0].retryAfter == nil {
		t.Fatalf("want 1 pause with window, got %+v", f.pauses)
	}
	if *f.pauses[0].retryAfter != 90*time.Second {
		t.Errorf("window = %v, want 90s", *f.pauses[0].retryAfter)
	}
}

func TestRateLimitBridge_SwitchModel(t *testing.T) {
	f := newRLFixture(agentWithFallback(), 5, true, nil)
	ev := rlEvent(wire.RateLimitedPayload{Model: "claude-primary", RetryAfter: "120"}, time.Now())

	if err := f.bridge.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.switches) != 1 {
		t.Fatalf("want 1 switch, got %d", len(f.switches))
	}
	sw := f.switches[0]
	if sw.plan.Action != modelendpoint.ActionSwitchModel {
		t.Errorf("action = %q, want SwitchModel", sw.plan.Action)
	}
	if !sw.plan.KeepClaim {
		t.Error("SwitchModel must keep the claim")
	}
	if sw.plan.To.Model != "claude-fallback" {
		t.Errorf("switch To.Model = %q, want claude-fallback", sw.plan.To.Model)
	}
	if len(f.pauses) != 0 {
		t.Errorf("SwitchModel must not pause, got %d pauses", len(f.pauses))
	}
}

func TestRateLimitBridge_SwitchModelNilExecutorIsNoop(t *testing.T) {
	f := newRLFixture(agentWithFallback(), 5, true, nil)
	f.bridge.OnSwitch = nil // shim-driven switch elsewhere; bridge is a no-op.
	ev := rlEvent(wire.RateLimitedPayload{Model: "claude-primary"}, time.Now())

	if err := f.bridge.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.pauses) != 0 {
		t.Errorf("SwitchModel verdict must not pause, got %d", len(f.pauses))
	}
}

func TestRateLimitBridge_FromModelDefaultsToSpec(t *testing.T) {
	// Payload carries no model; OnRateLimited defaults FromModel to spec.model.
	f := newRLFixture(agentWithFallback(), 5, true, nil)
	ev := rlEvent(wire.RateLimitedPayload{RetryAfter: "5"}, time.Now())

	if err := f.bridge.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.switches) != 1 {
		t.Fatalf("want 1 switch, got %d", len(f.switches))
	}
	if f.switches[0].plan.From != "claude-primary" {
		t.Errorf("From = %q, want spec model claude-primary", f.switches[0].plan.From)
	}
}

func TestRateLimitBridge_NonRateLimitedIgnored(t *testing.T) {
	f := newRLFixture(agentNoFallback(), 1, true, nil)
	for _, typ := range []wire.EventType{wire.EventStatus, wire.EventMessage, wire.EventUsage, wire.EventArtifactRef} {
		ev := wire.Event{A2ATaskID: "task-1", Type: typ, TS: time.Now()}
		if err := f.bridge.Handle(context.Background(), ev); err != nil {
			t.Fatalf("Handle(%s): %v", typ, err)
		}
	}
	if len(f.pauses)+len(f.switches) != 0 {
		t.Errorf("non-rate-limited events acted: pauses=%d switches=%d", len(f.pauses), len(f.switches))
	}
}

func TestRateLimitBridge_UnresolvedRunDropped(t *testing.T) {
	f := newRLFixture(agentNoFallback(), 1, false /* resolveOK */, nil)
	ev := rlEvent(wire.RateLimitedPayload{RetryAfter: "5"}, time.Now())

	if err := f.bridge.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.pauses)+len(f.switches) != 0 {
		t.Errorf("unresolved run acted: pauses=%d switches=%d", len(f.pauses), len(f.switches))
	}
}

func TestRateLimitBridge_ResolverErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	f := newRLFixture(agentNoFallback(), 1, true, boom)
	ev := rlEvent(wire.RateLimitedPayload{RetryAfter: "5"}, time.Now())

	if err := f.bridge.Handle(context.Background(), ev); !errors.Is(err, boom) {
		t.Fatalf("Handle err = %v, want wrapping boom", err)
	}
}

func TestRateLimitBridge_PauseErrorPropagates(t *testing.T) {
	boom := errors.New("db down")
	f := newRLFixture(agentNoFallback(), 1, true, nil)
	f.bridge.Pause = func(context.Context, int, *time.Duration) error { return boom }
	ev := rlEvent(wire.RateLimitedPayload{RetryAfter: "5"}, time.Now())

	if err := f.bridge.Handle(context.Background(), ev); !errors.Is(err, boom) {
		t.Fatalf("Handle err = %v, want wrapping db down", err)
	}
}

// jsonRoundTrip mimics the stdio transport delivering the payload as generic
// decoded JSON rather than the concrete wire type.
func jsonRoundTrip(t *testing.T, p wire.RateLimitedPayload) any {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestRateLimitBridge_JSONDecodedPayload(t *testing.T) {
	f := newRLFixture(agentNoFallback(), 3, true, nil)
	ev := wire.Event{
		A2ATaskID: "task-1",
		Type:      wire.EventRateLimited,
		TS:        time.Now(),
		Payload:   jsonRoundTrip(t, wire.RateLimitedPayload{Model: "claude-primary", RetryAfter: "30"}),
	}
	if err := f.bridge.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.pauses) != 1 || f.pauses[0].retryAfter == nil || *f.pauses[0].retryAfter != 30*time.Second {
		t.Fatalf("want pause with 30s window, got %+v", f.pauses)
	}
}

func TestRateLimitBridge_SinkForwardsAndActs(t *testing.T) {
	f := newRLFixture(agentNoFallback(), 8, true, nil)
	var forwarded []wire.Event
	sink := f.bridge.Sink(clienta2a.SinkFunc(func(_ context.Context, ev wire.Event) error {
		forwarded = append(forwarded, ev)
		return nil
	}))

	// A rate-limited event and an unrelated one both reach the inner sink; only
	// the rate-limited one drives a pause.
	rl := rlEvent(wire.RateLimitedPayload{Model: "claude-primary", RetryAfter: "60"}, time.Now())
	msg := wire.Event{A2ATaskID: "task-1", Type: wire.EventMessage, TS: time.Now()}
	for _, ev := range []wire.Event{rl, msg} {
		if err := sink.Event(context.Background(), ev); err != nil {
			t.Fatalf("sink.Event: %v", err)
		}
	}
	if len(forwarded) != 2 {
		t.Errorf("forwarded %d events, want 2 (sink must not swallow)", len(forwarded))
	}
	if len(f.pauses) != 1 {
		t.Errorf("want 1 pause from the sink path, got %d", len(f.pauses))
	}
}

func TestRateLimitBridge_SinkHandleErrorAborts(t *testing.T) {
	boom := errors.New("pause failed")
	f := newRLFixture(agentNoFallback(), 1, true, nil)
	f.bridge.Pause = func(context.Context, int, *time.Duration) error { return boom }
	var forwarded int
	sink := f.bridge.Sink(clienta2a.SinkFunc(func(context.Context, wire.Event) error {
		forwarded++
		return nil
	}))
	rl := rlEvent(wire.RateLimitedPayload{RetryAfter: "5"}, time.Now())

	if err := sink.Event(context.Background(), rl); !errors.Is(err, boom) {
		t.Fatalf("sink.Event err = %v, want wrapping pause failure", err)
	}
	if forwarded != 0 {
		t.Errorf("inner sink ran %d times despite Handle error, want 0", forwarded)
	}
}
