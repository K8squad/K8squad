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

package modelendpoint

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rateLimitedSignal(from string, at time.Time) RateLimitedSignal {
	return RateLimitedSignal{FromModel: from, At: at, RetryAfter: 90 * time.Second}
}

// TestOnRateLimitedSwitchesToConfiguredFallback (5.11 core AC): a
// configured fallback yields a SwitchModel plan that keeps the
// coordination claim (no re-dispatch — the 2.10 squad re-route is a
// different story) and targets the resolved fallback endpoint.
func TestOnRateLimitedSwitchesToConfiguredFallback(t *testing.T) {
	r := newResolver(t, endpointSecret("amelia-ollama", map[string][]byte{
		"endpointURL": []byte("http://ollama.svc:11434"),
	}))
	s := &Switcher{Resolver: r}
	a := byoAgent(func(spec *api.AgentSpec) {
		spec.ModelEndpointRef = &api.SecretRef{Name: "amelia-ollama"}
		spec.FallbackModel = &api.FallbackModel{Model: "llama3:8b"}
	})
	sig := rateLimitedSignal("qwen3:14b", time.Now())

	plan, err := s.OnRateLimited(context.Background(), a, sig)
	require.NoError(t, err)
	assert.Equal(t, ActionSwitchModel, plan.Action)
	assert.Equal(t, "qwen3:14b", plan.From)
	assert.Equal(t, "llama3:8b", plan.To.Model)
	assert.Equal(t, "http://ollama.svc:11434", plan.To.BaseURL)
	assert.True(t, plan.KeepClaim, "mid-Run model switch KEEPS the coordination claim (5.11)")
}

// TestOnRateLimitedNoFallsBackToPausePath (5.11 AC): with NO fallback
// configured the decision is the 2.11 scheduled-timer pause — the seam
// delegates to the existing resume machinery instead of inventing a
// second wait.
func TestOnRateLimitedNoFallsBackToPausePath(t *testing.T) {
	s := &Switcher{Resolver: newResolver(t)}
	a := byoAgent(nil)

	plan, err := s.OnRateLimited(context.Background(), a, rateLimitedSignal("qwen3:14b", time.Now()))
	require.NoError(t, err)
	assert.Equal(t, ActionPauseRun, plan.Action)
	assert.False(t, plan.KeepClaim, "pause path: the claim discipline belongs to pkg/coord, not the switch")
}

// TestOnRateLimitedUnresolvableFallbackFailsClosed: a fallback that was
// configured but does not resolve is an error — the reconciler surfaces it
// rather than silently hammering the throttled primary or silently
// pausing as if no fallback existed.
func TestOnRateLimitedUnresolvableFallbackFailsClosed(t *testing.T) {
	s := &Switcher{Resolver: newResolver(t)}
	a := byoAgent(func(spec *api.AgentSpec) {
		spec.FallbackModel = &api.FallbackModel{Model: "llama3:8b", ModelEndpointRef: &api.SecretRef{Name: "ghost"}}
	})
	_, err := s.OnRateLimited(context.Background(), a, rateLimitedSignal("qwen3:14b", time.Now()))
	require.Error(t, err)
	var ue *ErrUnresolved
	assert.ErrorAs(t, err, &ue)
}

// TestOnRateLimitedIdempotentByInput: the decision is pure — the same
// (agent, signal) yields the same plan, so a level-triggered reconciler
// re-drive cannot double-switch.
func TestOnRateLimitedIdempotentByInput(t *testing.T) {
	r := newResolver(t, endpointSecret("amelia-ollama", map[string][]byte{
		"endpointURL": []byte("http://ollama.svc:11434"),
	}))
	s := &Switcher{Resolver: r}
	a := byoAgent(func(spec *api.AgentSpec) {
		spec.FallbackModel = &api.FallbackModel{Model: "llama3:8b"}
	})
	sig := rateLimitedSignal("qwen3:14b", time.Now())

	p1, err := s.OnRateLimited(context.Background(), a, sig)
	require.NoError(t, err)
	p2, err := s.OnRateLimited(context.Background(), a, sig)
	require.NoError(t, err)
	assert.Equal(t, p1, p2)
}

// TestShouldSkipSwitchAlreadyOnFallback: a repeat rate_limited signal
// while ALREADY serving the fallback is a no-op — no re-switch, no
// re-meter.
func TestShouldSkipSwitchAlreadyOnFallback(t *testing.T) {
	plan := SwitchPlan{
		Action: ActionSwitchModel,
		To:     Endpoint{Model: "llama3:8b", BaseURL: "http://ollama.svc:11434"},
	}
	assert.True(t, ShouldSkipSwitch(plan, Endpoint{Model: "llama3:8b", BaseURL: "http://ollama.svc:11434"}),
		"already on the fallback → skip")
	assert.False(t, ShouldSkipSwitch(plan, Endpoint{Model: "qwen3:14b", BaseURL: "http://ollama.svc:11434"}),
		"still on the primary → switch")
	assert.False(t, ShouldSkipSwitch(SwitchPlan{Action: ActionPauseRun}, Endpoint{}),
		"pause plans are never switch-skips")
}

// TestProvenanceSegments (5.11 provenance AC): open → close → open draws
// the "which model served which portion" ledger the status subresource
// carries; the close stamps why the portion ended.
func TestProvenanceSegments(t *testing.T) {
	t0 := metav1.NewTime(time.Now().Add(-time.Minute))
	t1 := metav1.NewTime(time.Now())

	primary := Endpoint{Model: "qwen3:14b", BaseURL: "http://ollama.svc:11434", SecretName: "amelia-ollama"}
	fallback := Endpoint{Model: "llama3:8b", BaseURL: "http://ollama.svc:11434", SecretName: "amelia-ollama"}

	segs := OpenSegment(nil, primary, t0)
	require.Len(t, segs, 1)
	assert.Equal(t, "qwen3:14b", segs[0].Model)
	assert.Equal(t, "amelia-ollama", segs[0].SecretName, "Secret NAME rides as provenance, never contents")
	require.NotNil(t, segs[0].StartedAt)
	assert.Nil(t, segs[0].EndedAt, "segment is open while serving")

	segs = CloseSegment(segs, "qwen3:14b", t1, ReasonRateLimited)
	require.Len(t, segs, 1)
	require.NotNil(t, segs[0].EndedAt, "portion closed at the switch boundary")
	assert.Equal(t, ReasonRateLimited, segs[0].Reason)

	segs = OpenSegment(segs, fallback, t1)
	require.Len(t, segs, 2)
	assert.Equal(t, "llama3:8b", segs[1].Model)
	assert.Nil(t, segs[1].EndedAt)

	// The original slice is untouched (pure): the caller's status is never
	// mutated behind its back.
	assert.Nil(t, OpenSegment(nil, primary, t0)[0].EndedAt)
}

// TestCloseSegmentOnlyClosesMatchingOpenSegment: a close for a model with
// no open segment (bookkeeping drift, terminal close after a switch) is a
// no-op rather than corrupting the ledger.
func TestCloseSegmentOnlyClosesMatchingOpenSegment(t *testing.T) {
	t0 := metav1.NewTime(time.Now())
	closed := api.ModelSegment{Model: "qwen3:14b", StartedAt: &t0, EndedAt: &t0}
	segs := CloseSegment([]api.ModelSegment{closed}, "qwen3:14b", t0, ReasonRateLimited)
	require.Len(t, segs, 1)
	assert.Equal(t, t0, *segs[0].EndedAt, "already-closed segment stays as-is")
}
