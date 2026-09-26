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
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/K8squad/K8squad/internal/protocol"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
	"go.opentelemetry.io/otel/trace"
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
	// SandboxPod is the name of the sandbox pod this shim runs in (one run
	// per pod). It rides every run-trace span as ksquad.sandbox.pod (WS-A,
	// ISI-4382); empty in the operator-spawned stdio path where the shim is
	// not pod-hosted. Sourced from env (KSQUAD_SANDBOX_POD / HOSTNAME).
	SandboxPod string
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

// labels builds the run-trace correlation set for tk (WS-A, ISI-4382): the
// static per-shim identity (agent/team/project/sandbox pod, fixed for the
// shim's lifetime — one shim serves one Agent of one Team/Project in one
// pod) plus the per-task run identity (run id + ticket). On the warm-pool
// sandbox path the shim boots GENERIC — the static identity is empty — so the
// per-task fields carried on the submit payload (ISI-4439) take precedence
// when set. A nil task yields the static set only (there is no run-scoped call
// site today, but the method stays total). Team maps from the Agent Card's
// Squad.
func (e *Engine) labels(tk *task) toolusage.Labels {
	l := toolusage.Labels{
		Agent:      e.cfg.Identity.Name,
		Team:       e.cfg.Identity.Squad,
		Project:    e.cfg.Identity.Project,
		SandboxPod: e.cfg.SandboxPod,
	}
	if tk != nil {
		l.RunID = tk.id
		l.WorkItemRef = tk.workItem
		// ModelTier is per-task only: the operator resolves it at dispatch and
		// there is no process-static launch equivalent, so it rides straight
		// from the submit payload (ISI-4430 S5).
		l.ModelTier = tk.modelTier
		// ISI-4439: per-task run identity wins over the process-static launch
		// config. On the warm-pool sandbox path the pod boots generic, so
		// cfg.Identity is empty and the real agent/team/project arrive ONLY on
		// the submit payload; per-field so a partially-populated payload still
		// falls back to whatever the launch env did carry (the stdio path).
		if tk.agent != "" {
			l.Agent = tk.agent
		}
		if tk.team != "" {
			l.Team = tk.team
		}
		if tk.project != "" {
			l.Project = tk.project
		}
		// ISI-4973: the run's resolved BYO/Ollama route endpoint rides the
		// llm.call span's network attribution (server.address / url.full). It
		// is a per-task fact (ModelRoute.Endpoint on the submit payload), so it
		// travels via Labels like the identity fields — never on the wire.
		l.Endpoint = tk.endpoint
	}
	return l
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
	// Epic C / ADR-044 step 6 (ISI-5017): the per-task envelope's MCP IR wins
	// over the process-static launch config. On the warm-pool sandbox path the
	// pod boots GENERIC (K8SQUAD_MCP_CONFIG unset), so cfg.MCPEndpoints is
	// empty and the IR arrives ONLY on the submit payload; the operator-spawned
	// stdio path still carries it in the launch env and leaves the task field
	// empty. Fail-closed: a task that demands MCP servers but whose credential
	// VALUES are missing must never launch with a half-wired envelope.
	endpoints := t.MCPEndpoints
	if len(endpoints) == 0 {
		endpoints = e.cfg.MCPEndpoints
	}
	if err := validateMCPEnvelope(t.MCPEndpoints, t.MCPTokenEnv); err != nil {
		e.mu.Unlock()
		return a2a.Status{}, err
	}
	spec, err := e.rt.Command(runtimes.LaunchContext{
		Envelope:     t.Envelope,
		ModelRoute:   t.ModelRoute,
		Model:        e.cfg.Model,
		Credential:   e.cfg.Credential,
		WorkDir:      e.cfg.WorkDir,
		MCPEndpoints: endpoints,
	})
	if err != nil {
		e.mu.Unlock()
		return a2a.Status{}, fmt.Errorf("shim: build command for %s: %w", e.rt.Type(), err)
	}
	// Layer the resolved MCP credential VALUES onto the runtime subprocess env
	// (ISI-5017): the rendered native config references the env NAME
	// (Bearer {env:KSQUAD_MCP_*_TOKEN}), and the CLI resolves it from its own
	// process env. These values are secret material and are never logged.
	spec.Env = append(spec.Env, mcpCredentialEnv(t.MCPTokenEnv)...)
	runCtx, cancel := context.WithCancel(context.Background())
	resolvedModel := e.runModel(t)
	tk := &task{
		id:        t.A2ATaskID,
		workItem:  t.WorkItemID,
		agent:     t.Identity.Name,
		team:      t.Identity.Squad,
		project:   t.Identity.Project,
		model:     resolvedModel,
		modelTier: t.ModelTier,
		endpoint:  t.ModelRoute.Endpoint,
		provider:  providerForRoute(t.ModelRoute, resolvedModel),
		state:     a2a.TaskSubmitted,
		stream:    newTaskStream(),
		cancel:    cancel,
		now:       e.now,
	}
	// ISI-4238: a valid trace context on the submit ctx (the W3C carrier
	// the dispatcher injected — extracted by `shim run`/supervisor before
	// this call) becomes the telemetry parent, so the run's spans join the
	// Run's distributed trace rather than forking an orphan root.
	if trace.SpanContextFromContext(ctx).IsValid() {
		tk.traceCtx = ctx
	}
	e.tasks[t.A2ATaskID] = tk
	e.mu.Unlock()

	// Seed the SSE log with the submitted state before returning so a resume
	// from seq 0 always sees the full lifecycle (C4).
	tk.setState(a2a.TaskSubmitted, "")

	// Snapshot the submitted status BEFORE launching drive: the goroutine
	// advances tk.state to working→terminal, so reading tk.status() after the
	// go statement races with a fast runner and can return a non-submitted
	// state (flaky TestSubmitLifecycle). The submit call always reports the
	// submitted state; callers observe later transitions over the SSE stream.
	submitted := tk.status()

	go e.drive(runCtx, tk, spec)

	return submitted, nil
}

// validateMCPEnvelope fails closed (ADR-044) when a task-supplied MCP IR
// references a credential env NAME with no resolved value on the same
// envelope (ISI-5017). Only task-supplied endpoints are validated: the
// operator-spawned stdio path carries the IR in the launch env and no per-task
// tokens, and must keep its prior behavior. An endpoint whose credential rides
// a staged stdio sidecar (transport=stdio with an image) needs no agent-env
// value by construction, so it is exempt — mirroring capability.AssemblePod.
func validateMCPEnvelope(endpoints []capability.Endpoint, tokens map[string]string) error {
	for _, ep := range endpoints {
		if ep.Transport == "stdio" && ep.Image != "" {
			continue
		}
		for _, name := range ep.EnvNames {
			if _, ok := tokens[name]; !ok {
				return fmt.Errorf("shim: MCP endpoint %q requires credential env %s but the task envelope carries no value (fail-closed, ADR-044)", ep.Name, name)
			}
		}
	}
	return nil
}

// mcpCredentialEnv renders the task envelope's resolved MCP credential env
// pairs, name-sorted so the runtime launch env is deterministic across drives
// (a re-drive reattaches byte-identically, C1). Values are secret material and
// are never logged.
func mcpCredentialEnv(tokens map[string]string) []string {
	if len(tokens) == 0 {
		return nil
	}
	names := make([]string, 0, len(tokens))
	for name := range tokens {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+tokens[name])
	}
	return out
}

// drive runs the runtime to completion, funneling progress into the task's
// sequenced SSE log and settling on a terminal state exactly once. Tool and
// skill activity is additionally mapped onto the Epic D telemetry spine
// (when attached) — in the same funnel, so the wire events and the spans
// agree by construction. ISI-4238: the funnel also owns the run's root
// trace (run.start … run.end) and maps EventUsage onto llm.call spans +
// token counters, so a run is debuggable end-to-end instead of being a
// black box between dispatch and exit.
func (e *Engine) drive(ctx context.Context, tk *task, spec runtimes.ExecSpec) {
	// ISI-4238: telemetry parents from the submit-side trace context when
	// one rode the W3C carrier (joining the Run's distributed trace), else
	// from this ctx; the RUNNER below keeps the cancelable ctx either way —
	// the trace context is for spans only, never cancellation.
	telCtx := ctx
	if tk.traceCtx != nil {
		telCtx = tk.traceCtx
	}

	// ISI-4238: open the run's root trace BEFORE the first status event so
	// every span emitted below joins it AND the working event already
	// carries the trace id (StatusPayload.TraceID → Run.Status.TraceID
	// live, not only post-terminal).
	if e.telemetry != nil {
		var runSpan trace.Span
		telCtx, runSpan = e.telemetry.RunStart(telCtx, e.labels(tk), tk.id)
		if sc := runSpan.SpanContext(); sc.HasTraceID() {
			tk.setTraceID(sc.TraceID().String())
		}
	}

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
			e.telemetry.SkillEvent(telCtx, e.labels(tk), a2a.SkillLoadPayload{
				Name:   s,
				SHA256: e.cfg.SkillSHAs[s],
			})
		}
	}

	outcome, err := e.runner.Run(ctx, spec, func(p Progress) {
		hashToolArgs(p)
		// ISI-4238: usage payloads without a model (runtimes that report
		// tokens but not which model served them — opencode v1.18.27's
		// step-finish omits modelID) are attributed to the run's RESOLVED
		// model (tk.model = ModelRoute.Model → launch override → runtime
		// default) BEFORE the event leaves the process, so the wire event
		// and the llm.call span carry the same attribution. This is the
		// model the run actually launched against (runtimes.resolveModel);
		// the prior code used the runtime's generic default and so mislabeled
		// every BYO/routed run (e.g. an Ollama qwen run reported as the
		// opencode default claude-sonnet-4).
		if p.Kind == a2a.EventUsage && p.Usage != nil && p.Usage.Model == "" && tk.model != "" {
			p.Usage.Model = tk.model
		}
		// GH #634: usage payloads without a provider (runtimes whose wire omits
		// the serving provider — opencode v1.18.27's step-finish carries no
		// providerID) are attributed to the run's RESOLVED gen_ai.system before
		// the event leaves the process, so the llm.call span carries a non-empty
		// gen_ai.system. Derived from the ModelRoute/launch model at submit
		// (tk.provider); empty when neither yields a confident provider (the span
		// then omits the attribute rather than fabricating one — GH #634 AC).
		if p.Kind == a2a.EventUsage && p.Usage != nil && p.Usage.Provider == "" && tk.provider != "" {
			p.Usage.Provider = tk.provider
		}
		tk.emitProgress(p)
		if e.telemetry != nil {
			switch p.Kind {
			case a2a.EventTool:
				if p.Tool != nil {
					e.telemetry.ToolEvent(telCtx, e.labels(tk), tk.id, *p.Tool)
				}
			case a2a.EventSkillLoad:
				if p.SkillLoad != nil {
					e.telemetry.SkillEvent(telCtx, e.labels(tk), *p.SkillLoad)
				}
			case a2a.EventUsage:
				if p.Usage != nil {
					e.telemetry.UsageEvent(telCtx, e.labels(tk), tk.id, *p.Usage)
				}
			}
		}
	})

	var terminalState a2a.TaskState
	var terminalReason string
	switch {
	case ctx.Err() != nil:
		// Canceled via CancelTask; the cancel path owns the terminal state.
		terminalState, terminalReason = a2a.TaskCanceled, "canceled"
	case err != nil:
		terminalState, terminalReason = a2a.TaskFailed, err.Error()
	default:
		terminalState, terminalReason = outcome.State, outcome.Reason
	}
	// End the run trace BEFORE terminate closes the event stream. terminate
	// unblocks StreamEvents/drain consumers, and the terminal status carries
	// the run TraceID — so the run.start/run.end spans must be finished before
	// a consumer can drain the terminal event and race the still-open span
	// End() calls (ISI-4370). terminalState/terminalReason are computed above,
	// so this has no data dependency on terminate.
	if e.telemetry != nil {
		e.telemetry.RunEnd(telCtx, tk.id, string(terminalState), terminalReason)
		e.telemetry.FinishTask(telCtx, tk.id)
	}
	tk.terminate(terminalState, terminalReason)
}

// modelID resolves the model this engine serves (launch config override
// over the runtime default) — the attribution fallback for usage events
// whose runtime payload omits the model id (ISI-4238).
func (e *Engine) modelID() string {
	if e.cfg.Model != "" {
		return e.cfg.Model
	}
	return e.rt.DefaultModel().ID
}

// runModel resolves the model a specific task launches against, mirroring
// runtimes.resolveModel's precedence: the per-task route (ModelRoute.Model,
// the BYO/routed model — e.g. an Ollama-served qwen) wins over the engine's
// launch override, which wins over the runtime default. It is the truthful
// llm.call attribution for a runtime whose usage wire omits the served model
// (ISI-4238): the warm-pool sandbox path never sets KSQUAD_MODEL, so
// e.cfg.Model is empty and modelID() alone would collapse every routed run to
// the runtime default (opencode's claude-sonnet-4) — the mislabel Henrik saw.
func (e *Engine) runModel(t a2a.Task) string {
	if t.ModelRoute.Model != "" {
		return t.ModelRoute.Model
	}
	return e.modelID()
}

// providerForRoute resolves gen_ai.system (the serving provider) for a run
// whose usage wire omits it (GH #634), mirroring how Model is backfilled. It
// is derived from the resolved ModelRoute + launch model:
//
//   - BYO/Ollama route (Endpoint != ""): "ollama" when the endpoint host
//     suggests Ollama (host contains "ollama" or the local :11434
//     OpenAI-compat port), else the fixed BYO provider id
//     (capability.OpenCodeBYOProviderID, "ksquad-byo").
//   - vendor run (Endpoint == ""): inferred from the resolved launch model
//     (e.g. claude-sonnet-4 -> "anthropic"); "" when there is no confident
//     inference — the span then omits the attribute rather than fabricating
//     one (never fabricate, GH #634).
func providerForRoute(route a2a.ModelRoute, model string) string {
	if route.Endpoint != "" {
		if ollamaEndpoint(route.Endpoint) {
			return "ollama"
		}
		return capability.OpenCodeBYOProviderID
	}
	return providerFromModel(model)
}

// ollamaEndpoint reports whether a BYO endpoint host suggests an Ollama
// server: the hostname contains "ollama", or it binds the conventional
// local :11434 OpenAI-compat port (the zero-credential CI lane, spec §11/C9).
func ollamaEndpoint(endpoint string) bool {
	if u, err := url.Parse(endpoint); err == nil {
		if strings.Contains(strings.ToLower(u.Hostname()), "ollama") {
			return true
		}
		return u.Port() == "11434"
	}
	// Unparseable endpoint: fall back to raw substring checks so a BYO route
	// never silently mislabels an Ollama host as a generic BYO provider.
	lower := strings.ToLower(endpoint)
	return strings.Contains(lower, "ollama") || strings.Contains(lower, ":11434")
}

// providerFromModel maps a resolved launch model id onto a vendor gen_ai.system
// for a fixed-vendor run (GH #634). It is deliberately conservative: only the
// confident, well-known model families map, and anything unrecognized yields
// "" so the span omits the attribute instead of fabricating a provider.
func providerFromModel(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "claude"):
		return "anthropic"
	case strings.Contains(lower, "gpt"), strings.Contains(lower, "openai"):
		return "openai"
	case strings.Contains(lower, "gemini"):
		return "google"
	default:
		return ""
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
