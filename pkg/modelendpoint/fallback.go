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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// SwitchAction is the 5.11 verdict for a rate_limited Run.
type SwitchAction string

const (
	// ActionSwitchModel: a fallback is configured and resolved — the SAME
	// Run continues on the fallback model/endpoint, keeping the
	// coordination claim (no re-dispatch, §8 tier-1 recovery).
	ActionSwitchModel SwitchAction = "SwitchModel"

	// ActionPauseRun: NO fallback is configured — the Run takes the 2.11
	// scheduled-timer pause path (resume_at + single durable wake,
	// pkg/coord/resume.go), not a model switch.
	ActionPauseRun SwitchAction = "PauseRun"
)

// ReasonRateLimited is the §5.2/§8 sub-state that triggers the tier-1
// decision; carried onto provenance segments as why the portion ended.
const ReasonRateLimited = "rate_limited"

// SwitchPlan is the mid-Run switch decision (5.11): everything the
// reconciler/shim needs to act on one rate_limited signal, computed pure
// so it is unit-testable and idempotent by construction.
type SwitchPlan struct {
	// Action is the verdict: SwitchModel or PauseRun.
	Action SwitchAction

	// From is the model the Run was serving from when the limit hit (the
	// now-ending provenance segment).
	From string

	// To is the resolved fallback endpoint when Action is SwitchModel
	// (zero value otherwise). The shim rides the SAME seam to it — no new
	// AgentRuntime.type, no image change (ADR-026).
	To Endpoint

	// KeepClaim is true on SwitchModel: a model switch mid-Run keeps the
	// coordination claim — distinct from the 2.10 squad re-route, which is
	// a different agent/credential and DOES re-dispatch.
	KeepClaim bool
}

// Switcher is the 5.11 decision core over a Resolver. It decides; it does
// not dial — executing the switch against the runtime is the shim's
// (5.8/5.10) job, driven by this plan.
type Switcher struct {
	Resolver *Resolver
}

// RateLimitedSignal is one 5.10 shim signal: the Run's provider 429'd.
type RateLimitedSignal struct {
	// FromModel is the model the Run was serving from at the limit.
	FromModel string

	// At is when the limit was observed (provenance segment boundary).
	At time.Time

	// RetryAfter is the provider-advertised window, when sent. Only
	// consulted on the PauseRun path (2.11 resume_at); a fallback switch
	// does not wait for it.
	RetryAfter time.Duration
}

// OnRateLimited computes the 5.11 decision for a Run hitting its limit:
//
//   - fallback configured → SwitchModel: resolve the fallback endpoint
//     (fail-closed — an unresolvable fallback Secret is an ERROR, never a
//     silent stay-on-throttled-primary), keep the claim, continue.
//
//   - no fallback → PauseRun: hand back to the existing 2.11 scheduled
//     resume (resume_at = At + RetryAfter; pkg/coord/resume.go owns the
//     durable wake).
//
// Idempotent by input: the same (agent, signal) always yields the same
// plan, so a reconciler re-drive cannot double-switch.
func (s *Switcher) OnRateLimited(ctx context.Context, agent *api.Agent, sig RateLimitedSignal) (SwitchPlan, error) {
	from := sig.FromModel
	if from == "" {
		from = agent.Spec.Model
	}

	fallback, ok, err := s.Resolver.ResolveFallback(ctx, agent)
	if err != nil {
		// The fallback was CONFIGURED but does not resolve. Fail closed:
		// do not silently keep hammering the throttled primary. The
		// reconciler surfaces the error and the pause path remains the
		// operator's lever.
		return SwitchPlan{}, err
	}
	if !ok {
		// KeepClaim stays false: on pause there is no switch, and the
		// claim discipline (hold vs release while paused) belongs to
		// pkg/coord, not this decision.
		return SwitchPlan{
			Action: ActionPauseRun,
			From:   from,
		}, nil
	}

	return SwitchPlan{
		Action:    ActionSwitchModel,
		From:      from,
		To:        fallback,
		KeepClaim: true,
	}, nil
}

// ShouldSkipSwitch reports whether a switch plan is a no-op: the Run is
// already serving from the fallback's model+endpoint. A repeat
// rate_limited signal while already on fallback must not re-switch or
// re-meter (idempotency for the reconciler's level-triggered re-drive).
func ShouldSkipSwitch(plan SwitchPlan, currentlyServing Endpoint) bool {
	if plan.Action != ActionSwitchModel {
		return false
	}
	return currentlyServing.Model == plan.To.Model &&
		currentlyServing.BaseURL == plan.To.BaseURL
}

// CloseSegment ends the Run's current provenance portion: which model
// served until when, and why the portion ended. Pure — the reconciler
// stamps it onto Run.status.modelSegments (5.11 provenance: "which model
// served which portion").
func CloseSegment(segments []api.ModelSegment, model string, endedAt metav1.Time, reason string) []api.ModelSegment {
	out := make([]api.ModelSegment, len(segments))
	copy(out, segments)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Model == model && out[i].EndedAt == nil {
			ended := endedAt
			out[i].EndedAt = &ended
			out[i].Reason = reason
			return out
		}
	}
	return out
}

// OpenSegment begins a new provenance portion on the given model. The
// endpoint Secret NAME (never contents) rides along so attribution (7.6)
// and the 8.8 fallback indicators can key off it.
func OpenSegment(segments []api.ModelSegment, ep Endpoint, startedAt metav1.Time) []api.ModelSegment {
	seg := api.ModelSegment{
		Model:      ep.Model,
		StartedAt:  &startedAt,
		SecretName: ep.SecretName,
	}
	return append(segments, seg)
}
