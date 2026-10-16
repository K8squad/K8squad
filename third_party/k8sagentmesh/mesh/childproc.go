/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh

import (
	"context"
	"fmt"
	"syscall"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// ChildProcessAdapter is the base RuntimeAdapter for any runtime that runs as a
// standing child process (chapter §1B.1 base-vs-override): opencode, Claude
// Code, Codex and goose all inherit it, because for a child process the OS
// itself supplies Hibernate/Wake as SIGSTOP/SIGCONT. The runtime-specific bits
// — how to build the launch spec and the agent card from an Envelope — are
// injected (card/launch), so this type stays free of any concrete runtime: it
// is the proof that the gateway's lifecycle is written against the interface,
// not against opencode.
//
// An in-process framework (e.g. CrewAI embedded in the gateway) cannot be
// SIGSTOP'd independently; such a runtime provides its own RuntimeAdapter (or
// embeds this one and overrides Hibernate/Wake with a cooperative pause/resume).
// The interface is identical; only the mechanism differs.
type ChildProcessAdapter struct {
	host   PlatformHost
	router Router
	// runtimeType is the flavor this adapter spawns (labels + card).
	runtimeType string
	// card builds the agent card a spawned process serves from its envelope.
	card func(env Envelope) a2a.AgentCard
	// launch builds the host LaunchSpec (runtime-specific process env) from the
	// envelope. It may fail (e.g. a malformed model route) before any process
	// starts.
	launch func(env Envelope) (LaunchSpec, error)
}

// Option configures a ChildProcessAdapter.
type Option func(*ChildProcessAdapter)

// WithRouter overrides the task router (default: the HTTP /task router). Tests
// inject a fake; a k8squad deployment may inject its pod-proxy transport.
func WithRouter(r Router) Option {
	return func(a *ChildProcessAdapter) { a.router = r }
}

// newChildProcessAdapter wires the base adapter. host is required; card and
// launch are the runtime-specific builders a concrete adapter (opencode)
// supplies. The router defaults to the HTTP /task router.
func newChildProcessAdapter(host PlatformHost, runtimeType string, card func(Envelope) a2a.AgentCard, launch func(Envelope) (LaunchSpec, error), opts ...Option) *ChildProcessAdapter {
	a := &ChildProcessAdapter{
		host:        host,
		router:      NewHTTPRouter(nil),
		runtimeType: runtimeType,
		card:        card,
		launch:      launch,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// childHandle is the concrete Handle a ChildProcessAdapter hands out. It binds a
// running host Instance to the agent card it serves and the envelope it was
// spawned from (so Recreate can rebuild it). The gateway sees only the Handle
// interface; Route/Hibernate/Wake/Stop/Recreate recover this concrete type.
type childHandle struct {
	inst Instance
	card a2a.AgentCard
	env  Envelope
}

func (h *childHandle) ID() string       { return h.inst.ID() }
func (h *childHandle) Endpoint() string { return h.inst.Endpoint() }

// recover narrows a Handle to this adapter's concrete *childHandle, failing
// closed on a nil or foreign handle rather than panicking — passing another
// adapter's handle is a gateway bug that must surface as an error, not a crash.
func (a *ChildProcessAdapter) recover(h Handle) (*childHandle, error) {
	ch, ok := h.(*childHandle)
	if !ok || ch == nil {
		return nil, fmt.Errorf("mesh: handle is not a %s child-process handle (got %T)", a.runtimeType, h)
	}
	return ch, nil
}

// sink returns the host's telemetry sink, never nil.
func (a *ChildProcessAdapter) sink() TelemetrySink {
	if s := a.host.Telemetry(); s != nil {
		return s
	}
	return NopSink{}
}

// Spawn launches a standing process pre-loaded with the envelope and returns a
// handle plus the agent card the process serves.
func (a *ChildProcessAdapter) Spawn(ctx context.Context, env Envelope) (Handle, a2a.AgentCard, error) {
	if env.RuntimeType == "" {
		env.RuntimeType = a.runtimeType
	}
	spec, err := a.launch(env)
	if err != nil {
		a.sink().OnLifecycle("spawn", "", "", err)
		return nil, a2a.AgentCard{}, fmt.Errorf("mesh: build launch spec for %s: %w", a.runtimeType, err)
	}
	inst, err := a.host.Launch(ctx, spec)
	if err != nil {
		a.sink().OnLifecycle("spawn", "", "", err)
		return nil, a2a.AgentCard{}, fmt.Errorf("mesh: launch %s: %w", a.runtimeType, err)
	}
	card := a.card(env)
	h := &childHandle{inst: inst, card: card, env: env}
	a.sink().OnLifecycle("spawn", h.ID(), h.Endpoint(), nil)
	return h, card, nil
}

// Register surfaces the agent card the spawned process serves. In the full
// gateway this is where the supervisor learns the card and persists the
// endpoint (coord.mesh_endpoint); at this seam it returns the card the process
// announced at Spawn, which is the authoritative capability contract.
func (a *ChildProcessAdapter) Register(ctx context.Context, h Handle) (a2a.AgentCard, error) {
	ch, err := a.recover(h)
	if err != nil {
		return a2a.AgentCard{}, err
	}
	a.sink().OnLifecycle("register", ch.ID(), ch.Endpoint(), nil)
	return ch.card, nil
}

// Route delivers one A2A task to the standing process behind h. Idempotency on
// task.A2ATaskID (C1) is enforced server-side by the shim engine the process
// runs — a re-route reattaches to the in-flight task, never starts a second
// execution — so this method only has to deliver the task to the right endpoint.
func (a *ChildProcessAdapter) Route(ctx context.Context, h Handle, task a2a.Task) (Stream, error) {
	ch, err := a.recover(h)
	if err != nil {
		return nil, err
	}
	if task.A2ATaskID == "" {
		return nil, fmt.Errorf("mesh: Route requires a non-empty a2a_task_id")
	}
	stream, err := a.router.Route(ctx, ch.Endpoint(), task)
	a.sink().OnLifecycle("route", ch.ID(), ch.Endpoint(), err)
	if err != nil {
		return nil, fmt.Errorf("mesh: route task %s to %s: %w", task.A2ATaskID, ch.Endpoint(), err)
	}
	return stream, nil
}

// Hibernate freezes the process with SIGSTOP (base mechanism, chapter §6.1): the
// process, its warmed runtime and its context stay resident, so Wake is
// near-zero latency with no re-init. It does NOT free RAM — an LLM-idle process
// already burns no CPU; hibernate's value is instant wake and a clean idleness
// signal, not a memory win.
func (a *ChildProcessAdapter) Hibernate(ctx context.Context, h Handle) error {
	ch, err := a.recover(h)
	if err != nil {
		return err
	}
	err = ch.inst.Signal(syscall.SIGSTOP)
	a.sink().OnLifecycle("hibernate", ch.ID(), ch.Endpoint(), err)
	if err != nil {
		return fmt.Errorf("mesh: hibernate (SIGSTOP) %s: %w", ch.ID(), err)
	}
	return nil
}

// Wake resumes a hibernated process with SIGCONT. It is the inverse of
// Hibernate; a never-hibernated process is unaffected by SIGCONT, so Wake is
// safe to call defensively.
func (a *ChildProcessAdapter) Wake(ctx context.Context, h Handle) error {
	ch, err := a.recover(h)
	if err != nil {
		return err
	}
	err = ch.inst.Signal(syscall.SIGCONT)
	a.sink().OnLifecycle("wake", ch.ID(), ch.Endpoint(), err)
	if err != nil {
		return fmt.Errorf("mesh: wake (SIGCONT) %s: %w", ch.ID(), err)
	}
	return nil
}

// Stop terminates the process and releases its RAM. The host's Close does the
// graceful SIGTERM→SIGKILL sequence over the process group.
func (a *ChildProcessAdapter) Stop(ctx context.Context, h Handle) error {
	ch, err := a.recover(h)
	if err != nil {
		return err
	}
	err = ch.inst.Close()
	a.sink().OnLifecycle("stop", ch.ID(), ch.Endpoint(), err)
	if err != nil {
		return fmt.Errorf("mesh: stop %s: %w", ch.ID(), err)
	}
	return nil
}

// Recreate rebuilds the process from an envelope — a near-bare-exec into the
// already-staged runtime, not a cold boot (chapter §6.1). It stops the old
// process first (best-effort: a teardown failure must not block the rebuild,
// because a wedged process is exactly the case Recreate exists to recover from)
// then spawns anew. The prior handle is invalid after a successful Recreate.
func (a *ChildProcessAdapter) Recreate(ctx context.Context, h Handle, env Envelope) (Handle, error) {
	ch, err := a.recover(h)
	if err != nil {
		return nil, err
	}
	if cerr := ch.inst.Close(); cerr != nil {
		// Report but do not abort: the old process may already be dead, which is
		// the common Recreate trigger.
		a.sink().OnLifecycle("recreate.stop", ch.ID(), ch.Endpoint(), cerr)
	}
	nh, _, err := a.Spawn(ctx, env)
	a.sink().OnLifecycle("recreate", "", "", err)
	if err != nil {
		return nil, fmt.Errorf("mesh: recreate %s: %w", a.runtimeType, err)
	}
	return nh, nil
}

// compile-time assertion that the base adapter satisfies the seam.
var _ RuntimeAdapter = (*ChildProcessAdapter)(nil)
