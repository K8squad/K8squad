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

// Package shim is the runtime-agnostic engine every KSquad agent-runtime shim
// runs (arch §7.1/§7.5, stories 5.5 + 5.8). It implements the internal stable
// a2a.Shim southbound interface — the six MUST-verbs, the §3.1 task state
// machine, gap-free SSE sequencing (C4), submit-reattach dedup (C1) and
// idempotent cancel (C8) — once, for all runtimes. A concrete runtime plugs in
// as a runtimes.Runtime adapter (capabilities + credential mapping + launch
// command); the engine owns everything conformance asserts, so a new shim is
// zero engine change.
package shim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/K8squad/K8squad/internal/protocol"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// Identity is the Agent identity block the shim stamps onto every Agent Card
// (spec §6.1), sourced from the Agent CR by the reconciler at launch.
type Identity struct {
	Name    string
	Squad   string
	Project string
}

// Config is the per-shim launch configuration the reconciler supplies via env
// (arch §7.2/§7.3). It is fixed for the shim's lifetime: one shim process
// serves one Agent of one runtime flavor.
type Config struct {
	Identity Identity
	// Skills are the Skill refs granted to the Agent (Agent.spec.skillRefs).
	Skills []string
	// SkillSHAs maps granted skill names to their pinned git source commit
	// (arch §5.3.6), when the reconciler knows one — it rides the skill.load
	// telemetry as the source SHA (Epic D, plan §2.4). Inline bodies have no
	// entry. +optional
	SkillSHAs map[string]string
	// Model overrides the runtime's default model id (Agent.spec.model). The
	// context window remains the runtime's declared authority (spec §6.2).
	Model string
	// Credential is the raw per-user secret the reconciler env-injected (arch
	// §7.3). Held only in memory, mapped to native env per Run, never logged.
	Credential string
	// CredentialSecretRef is the Secret name advertised on the Agent Card auth
	// block (metadata only — never the value).
	CredentialSecretRef string
	// ShimVersion is this shim binary's version, advertised on the card.
	ShimVersion string
	// Experimental marks a non-conformant vendor runtime (FR-D3) on the card.
	Experimental bool
	// WorkDir is the sandbox working directory Runs execute in.
	WorkDir string
	// MCPEndpoints is the Run's resolved MCP IR (Epic C, ADR-044): parsed
	// once at shim startup from the projected K8SQUAD_MCP_CONFIG document
	// and handed to the runtime adapters, which render their native config
	// from it. Empty when the Run demanded no MCP servers.
	MCPEndpoints []capability.Endpoint
	// Nower is an injectable clock for deterministic tests; nil uses time.Now.
	Nower func() time.Time
}

// Engine is the runtime-agnostic shim implementing a2a.Shim for one runtime.
type Engine struct {
	rt     runtimes.Runtime
	runner Runner
	cfg    Config
	now    func() time.Time
	// telemetry is the optional Epic D tool-usage hook: when set, tool and
	// skill activity flowing through the engine's event funnel is mapped
	// onto OTel GenAI-semconv spans + ksquad_* metrics in-process (plan
	// §2.4) — the shim is where tool events are born, so it is the first
	// telemetry hop. Nil = no telemetry (tests, card-only invocations).
	telemetry *toolusage.Mapper

	mu    sync.Mutex
	tasks map[string]*task
}

var _ a2a.Shim = (*Engine)(nil)

// New builds an Engine for the given runtime, driving Runs through runner.
func New(rt runtimes.Runtime, runner Runner, cfg Config) *Engine {
	now := cfg.Nower
	if now == nil {
		now = time.Now
	}
	return &Engine{
		rt:     rt,
		runner: runner,
		cfg:    cfg,
		now:    now,
		tasks:  map[string]*task{},
	}
}

// SetTelemetry attaches the Epic D tool-usage mapper (plan §2.4). The
// engine's labels come from its Identity (agent/project) — run correlation
// rides the task id, which IS the run id (spec §3 V1). Calling it after
// tasks started is legal: events emitted before the call were simply not
// mapped (telemetry is strictly observational, never load-bearing).
func (e *Engine) SetTelemetry(m *toolusage.Mapper) { e.telemetry = m }

func (e *Engine) labels() toolusage.Labels {
	return toolusage.Labels{Agent: e.cfg.Identity.Name}
}

// Runtime returns the runtime this engine serves.
func (e *Engine) Runtime() runtimes.Runtime { return e.rt }

// SubmitTask (V1) drives a task. A second submit with a known a2a_task_id
// reattaches and returns the current status without starting a second
// execution (C1); a submit on a terminal task returns the terminal status.
func (e *Engine) SubmitTask(ctx context.Context, t a2a.Task) (a2a.Status, error) {
	if t.A2ATaskID == "" {
		return a2a.Status{}, fmt.Errorf("shim: SubmitTask requires a non-empty a2a_task_id")
	}

	e.mu.Lock()
	if existing, ok := e.tasks[t.A2ATaskID]; ok {
		e.mu.Unlock()
		return existing.status(), nil // reattach / terminal dedup (C1)
	}
	spec, err := e.rt.Command(runtimes.LaunchContext{
		Envelope:     t.Envelope,
		ModelRoute:   t.ModelRoute,
		Model:        e.cfg.Model,
		Credential:   e.cfg.Credential,
		WorkDir:      e.cfg.WorkDir,
		MCPEndpoints: e.cfg.MCPEndpoints,
	})
	if err != nil {
		e.mu.Unlock()
		return a2a.Status{}, fmt.Errorf("shim: build command for %s: %w", e.rt.Type(), err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	tk := &task{
		id:       t.A2ATaskID,
		workItem: t.WorkItemID,
		state:    a2a.TaskSubmitted,
		stream:   newTaskStream(),
		cancel:   cancel,
		now:      e.now,
	}
	e.tasks[t.A2ATaskID] = tk
	e.mu.Unlock()

	// Seed the SSE log with the submitted state before returning so a resume
	// from seq 0 always sees the full lifecycle (C4).
	tk.setState(a2a.TaskSubmitted, "")

	go e.drive(runCtx, tk, spec)

	return tk.status(), nil
}

// drive runs the runtime to completion, funneling progress into the task's
// sequenced SSE log and settling on a terminal state exactly once. Tool and
// skill activity is additionally mapped onto the Epic D telemetry spine
// (when attached) — in the same funnel, so the wire events and the spans
// agree by construction.
func (e *Engine) drive(ctx context.Context, tk *task, spec runtimes.ExecSpec) {
	tk.setState(a2a.TaskWorking, "")

	// Epic D (plan §2.4): each granted skill entering the session is a
	// skill.load activity. The engine knows the granted refs at launch; the
	// source SHA travels on the payload when the reconciler pinned one
	// (git-sourced skills, arch §5.3.6). Outcome is deliberately absent
	// (unknown): "granted" is not "loaded successfully" — no runtime has
	// actually loaded anything yet, so the span never claims success it
	// cannot know (review ISI-3348, non-blocking note).
	if e.telemetry != nil {
		for _, s := range e.cfg.Skills {
			e.telemetry.SkillEvent(ctx, e.labels(), a2a.SkillLoadPayload{
				Name:   s,
				SHA256: e.cfg.SkillSHAs[s],
			})
		}
	}

	outcome, err := e.runner.Run(ctx, spec, func(p Progress) {
		hashToolArgs(p)
		tk.emitProgress(p)
		if e.telemetry != nil {
			switch p.Kind {
			case a2a.EventTool:
				if p.Tool != nil {
					e.telemetry.ToolEvent(ctx, e.labels(), tk.id, *p.Tool)
				}
			case a2a.EventSkillLoad:
				if p.SkillLoad != nil {
					e.telemetry.SkillEvent(ctx, e.labels(), *p.SkillLoad)
				}
			}
		}
	})

	switch {
	case ctx.Err() != nil:
		// Canceled via CancelTask; the cancel path owns the terminal state.
		tk.terminate(a2a.TaskCanceled, "canceled")
	case err != nil:
		tk.terminate(a2a.TaskFailed, err.Error())
	default:
		tk.terminate(outcome.State, outcome.Reason)
	}
	if e.telemetry != nil {
		e.telemetry.FinishTask(ctx, tk.id)
	}
}

// hashToolArgs stamps the tool-call arguments hash at the shim's tool-call
// boundary — BEFORE the event leaves the process (Epic D, plan §2.4: args are
// hashed, never transported raw — they may carry secrets). Raw args ride the
// internal Progress.ToolArgs seam only; by the time the event is funneled
// into the SSE log and the telemetry mapper it carries ArgsSHA256 and no raw
// argument text. An emitter that already computed the hash is respected
// (idempotent); no args means no hash (the acceptance attribute is optional).
func hashToolArgs(p Progress) {
	if p.Tool == nil || p.Tool.ArgsSHA256 != "" || p.ToolArgs == "" {
		return
	}
	sum := sha256.Sum256([]byte(p.ToolArgs))
	p.Tool.ArgsSHA256 = hex.EncodeToString(sum[:])
}

// StreamEvents (V2) returns an SSE-style channel of this task's events with
// seq > fromSeq, replaying the buffered log then tailing live events until the
// task is terminal (resume is gap-free, C4). Unknown task is an error.
func (e *Engine) StreamEvents(ctx context.Context, taskID string, fromSeq uint64) (<-chan a2a.Event, error) {
	e.mu.Lock()
	tk, ok := e.tasks[taskID]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("shim: StreamEvents: unknown task %q", taskID)
	}
	return tk.stream.subscribe(ctx, fromSeq), nil
}

// GetStatus (V3) is a pure read of the current task status. Unknown task is an
// error.
func (e *Engine) GetStatus(ctx context.Context, taskID string) (a2a.Status, error) {
	e.mu.Lock()
	tk, ok := e.tasks[taskID]
	e.mu.Unlock()
	if !ok {
		return a2a.Status{}, fmt.Errorf("shim: GetStatus: unknown task %q", taskID)
	}
	return tk.status(), nil
}

// CancelTask (V4) is idempotent: it drains a live task to canceled and is a
// no-op success on an already-terminal or unknown task (C8).
func (e *Engine) CancelTask(ctx context.Context, taskID, reason string) error {
	e.mu.Lock()
	tk, ok := e.tasks[taskID]
	e.mu.Unlock()
	if !ok {
		return nil // unknown task: idempotent no-op (C8)
	}
	if reason == "" {
		reason = "canceled"
	}
	// Signal the runtime process to stop; drive() observes ctx.Err() and the
	// terminate below is the authoritative canceled transition (whichever wins
	// the terminate guard, the state is canceled).
	tk.cancel()
	tk.terminate(a2a.TaskCanceled, reason)
	return nil
}

// GetAgentCard (V6) returns the capability contract the core negotiates
// against (spec §6), generated from the runtime adapter + launch config +
// pinned protocol revisions.
func (e *Engine) GetAgentCard(ctx context.Context) (a2a.AgentCard, error) {
	return e.AgentCard(), nil
}

// AgentCard builds the Agent Card without a context, for cmd/shim startup and
// tests.
func (e *Engine) AgentCard() a2a.AgentCard {
	model := e.rt.DefaultModel()
	if e.cfg.Model != "" {
		model.ID = e.cfg.Model // context window stays the runtime's authority
	}
	skills := e.cfg.Skills
	if skills == nil {
		skills = []string{}
	}
	return a2a.AgentCard{
		SchemaVersion: a2a.SchemaVersion,
		Agent: a2a.AgentIdentity{
			Name:    e.cfg.Identity.Name,
			Squad:   e.cfg.Identity.Squad,
			Project: e.cfg.Identity.Project,
		},
		Runtime: a2a.RuntimeInfo{
			Type:         e.rt.Type(),
			CLIVersion:   e.rt.CLIVersion(),
			ShimVersion:  e.cfg.ShimVersion,
			Experimental: e.cfg.Experimental,
		},
		Model:  model,
		Skills: skills,
		Auth: a2a.AuthInfo{
			Type:      string(e.rt.CredentialShape()),
			SecretRef: e.cfg.CredentialSecretRef,
		},
		Capabilities: e.rt.Capabilities(),
		Protocol:     protocol.Pinned(),
	}
}
