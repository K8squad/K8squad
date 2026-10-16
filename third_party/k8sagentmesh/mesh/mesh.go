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

// Package mesh is the AgentMesh runtime-agnostic seam (AgentMesh chapter §1B,
// decision D8.1). It defines the one contract the gateway/supervisor speak —
// RuntimeAdapter — so the mesh lifecycle logic is written against an interface,
// never against a concrete coding-agent runtime (opencode, Claude Code, Codex,
// goose, CrewAI). opencode is adapter #1 (opencode.go), codex is adapter #2
// (codex.go); both are ChildProcessAdapters built from an injected RuntimeSpec
// (leak L1, ISI-5827) — a host supplies the runtime-type names, card/launch
// builder inputs and env contracts, so adding a runtime is a new spec plus a
// constructor with ZERO control-plane change, which is the test that proves
// the seam generalizes.
//
// Two seams are pluggable and orthogonal (chapter §1B):
//
//   - RuntimeAdapter — the MECHANISM: how one runtime's lifecycle (Spawn,
//     Register, Route, Hibernate, Wake, Stop, Recreate) maps onto the concrete
//     runtime. opencode supplies one.
//   - PlatformHost — the SUBSTRATE: where/how a runtime process physically runs
//     (a local child process, a Kubernetes sandbox pod) plus the telemetry sink.
//     k8squad (host #1) supplies one; LocalHost (localhost.go) is the generic
//     POSIX child-process substrate the prototype runs against.
//
// This package is [generic] — it stays extraction-clean by construction
// (roadmap P3 lifted it out of the k8squad host): it depends only on the A2A
// wire types (../a2a), never on any host-internal control-plane package; the
// host-specific bits arrive as injected inputs (RuntimeSpec, PlatformHost,
// Router, TelemetrySink).
package mesh

import (
	"context"
	"syscall"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// Envelope is the role envelope a standing process is pre-loaded with: the
// resolved model, tools and skills that define what the process CAN do before
// any task arrives. It is the generic input to Spawn and Recreate.
//
// It is deliberately distinct from a2a.Envelope (the per-task system-context +
// instruction carried on a Task, delivered through Route): this is the standing
// identity of the process, that is one unit of work handed to it.
type Envelope struct {
	// RuntimeType is the coding-agent flavor to spawn (api/v1alpha1
	// RuntimeType*, e.g. "opencode"). The adapter that owns the runtime fixes
	// this; it rides the envelope so a host/telemetry sink can label the process.
	RuntimeType string
	// Model is the resolved model id the process serves with (Agent.spec.model).
	// Empty uses the runtime's default. The context window stays the runtime's
	// authority — this overrides only the id.
	Model string
	// ModelRoute is the resolved model-provider seam (a BYO OpenAI-compatible
	// endpoint, or empty for the runtime's fixed vendor wire).
	ModelRoute a2a.ModelRoute
	// Skills is the announced skill set surfaced on the agent card (Register).
	Skills []string
	// Identity is the standing agent identity (name/squad/project) the process
	// serves under; stamped on the agent card and the process's telemetry.
	Identity a2a.AgentIdentity
	// WorkDir is the sandbox working directory the runtime runs in; empty uses
	// the host's default.
	WorkDir string
	// Env is extra process environment the host layers onto the launched
	// runtime (e.g. a resolved credential). Values may be secret material and
	// MUST NOT be logged.
	Env map[string]string

	// Source is the per-process source-checkout layout (ISI-6009 write
	// discipline, ADR-0024 §1): when set, the process runs on Source.Branch in
	// Source.WorktreeDir — its exclusive write surface — with Source.TeamMainDir
	// read-only to it. nil keeps the pre-P1.4a shape (plain WorkDir, no source
	// isolation) for runtimes with no source output.
	Source *SourceSpec
}

// Handle is an opaque reference to one standing process spawned by a
// RuntimeAdapter. The gateway treats it opaquely; only the adapter that
// produced it knows its concrete shape. A Handle from one adapter MUST NOT be
// passed to another.
type Handle interface {
	// ID is a stable identifier for the running process (adapter-assigned).
	ID() string
	// Endpoint is the A2A base URL the process serves (e.g.
	// http://127.0.0.1:8080). Route targets its /task surface.
	Endpoint() string
}

// Stream is one routed task's live connection: a gap-free SSE event stream that
// closes when the task settles, the settled status, and an idempotent Close
// that releases the transport resources. It mirrors the core-side a2a.Session
// contract but is named at the mesh seam so the generic lifecycle logic does not
// reach into a k8squad-internal transport package.
type Stream interface {
	// Events is the task's sequenced SSE stream; it is closed exactly once,
	// when the task reaches a terminal state or the transport tears down.
	Events() <-chan a2a.Event
	// Status is the task's current status; after Events() closes it is the
	// terminal status.
	Status() a2a.Status
	// Close releases transport resources; it is idempotent.
	Close() error
}

// RuntimeAdapter — the one seam that makes AgentMesh runtime-agnostic (chapter
// §1B, D8.1). The gateway speaks ONLY this interface; opencode / Claude Code /
// Codex / goose / CrewAI each provide an implementation. Every lifecycle op
// (chapter §6) is expressed here.
type RuntimeAdapter interface {
	// Spawn a standing process pre-loaded with the role envelope (model + tools
	// + skills). It returns a handle and the agent card the process will serve.
	Spawn(ctx context.Context, env Envelope) (Handle, a2a.AgentCard, error)
	// Register surfaces the agent card (announced skills) to the supervisor.
	Register(ctx context.Context, h Handle) (a2a.AgentCard, error)
	// Route delivers ONE A2A task to a specific standing process. It MUST be
	// idempotent on task.A2ATaskID (a re-route reattaches, never double-executes).
	Route(ctx context.Context, h Handle, task a2a.Task) (Stream, error)
	// Hibernate freezes an idle process cheaply; Wake restores it with NO
	// re-init. The base child-process adapter implements these as SIGSTOP /
	// SIGCONT (chapter §1B.1 base-vs-override); an in-process runtime overrides
	// with a cooperative pause/resume.
	Hibernate(ctx context.Context, h Handle) error
	Wake(ctx context.Context, h Handle) error
	// Stop terminates the process, releasing RAM.
	Stop(ctx context.Context, h Handle) error
	// Recreate rebuilds from the envelope — near-bare-exec, not a cold boot. The
	// prior handle is invalid after a successful Recreate.
	Recreate(ctx context.Context, h Handle, env Envelope) (Handle, error)
}

// PlatformHost is the substrate seam (chapter §1B): it owns WHERE a runtime
// process physically runs and the telemetry sink the mesh emits to. The adapter
// (mechanism) and host (substrate) are both pluggable; k8squad supplies one of
// each. LocalHost (localhost.go) is the generic POSIX child-process host the
// prototype runs against; a Kubernetes sandbox-pod host is the k8squad-specific
// substrate that arrives with the gateway/warm-pool integration.
type PlatformHost interface {
	// Launch starts a standing runtime process for the given spec and returns a
	// running Instance. For a local host this execs the shim as a child process;
	// a kube host provisions/execs the shim supervisor in a sandbox pod.
	Launch(ctx context.Context, spec LaunchSpec) (Instance, error)
	// Telemetry is the sink lifecycle events are reported to. It is never nil —
	// a host with no telemetry returns NopSink.
	Telemetry() TelemetrySink
}

// LaunchSpec is the resolved, substrate-level input PlatformHost.Launch turns
// into a running process. The adapter builds it from an Envelope; the host does
// not interpret the runtime semantics, only runs the process.
type LaunchSpec struct {
	// RuntimeType is the flavor being launched (labels/telemetry only).
	RuntimeType string
	// Env is the full process environment (KSQUAD_RUNTIME_TYPE, OPENCODE_*,
	// a resolved credential, …) in the os/exec "KEY=VALUE" form. It MAY carry
	// secret material and MUST NOT be logged.
	Env []string
	// WorkDir is the process working directory; empty uses the host default.
	WorkDir string
	// Source, when non-nil, is the per-process source layout (ADR-0024 §1) the
	// host provisions before exec: the worktree is created idempotently and the
	// process lands in it (Source overrides WorkDir as the launch directory).
	Source *SourceSpec
	// Identity is the standing agent identity (telemetry labels).
	Identity a2a.AgentIdentity
}

// Instance is one running runtime process managed by a PlatformHost. The
// adapter drives its lifecycle through this seam, which is why the generic
// adapter never imports os/exec or kube: SIGSTOP/SIGCONT (Hibernate/Wake) and
// teardown (Stop) are expressed as Signal and Close here, and the concrete host
// decides whether that is a process-group kill or a pod exec.
type Instance interface {
	// ID is a stable identifier for the running instance.
	ID() string
	// Endpoint is the A2A base URL the instance serves.
	Endpoint() string
	// Signal delivers an OS signal to the instance (its whole process group for
	// a child-process host). SIGSTOP/SIGCONT are the base Hibernate/Wake; a
	// graceful Stop uses SIGTERM then SIGKILL via Close.
	Signal(sig syscall.Signal) error
	// Close terminates the instance and releases its resources. It is idempotent.
	Close() error
}

// TelemetrySink receives lifecycle events from the mesh. It is the generic
// hook the PlatformHost wires to its concrete telemetry spine (k8squad: the
// OTLP exporter). The default NopSink discards everything.
type TelemetrySink interface {
	// OnLifecycle reports that op (one of the RuntimeAdapter verbs) ran against
	// the instance identified by handleID/endpoint, with err non-nil on failure.
	OnLifecycle(op string, handleID, endpoint string, err error)
}

// NopSink is a TelemetrySink that discards every event. Hosts with no telemetry
// return it so the adapter never needs a nil check.
type NopSink struct{}

// OnLifecycle discards the event.
func (NopSink) OnLifecycle(string, string, string, error) {}
