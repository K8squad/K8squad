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

package rundrive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/a2a"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/contextasm"
	"github.com/K8squad/K8squad/pkg/controller/contextsource"
	"github.com/K8squad/K8squad/pkg/orgops"
	"github.com/K8squad/K8squad/pkg/taskio"
	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// dispatch.go — the operator-side physical A2A dispatch (ISI-3352, the last
// hop the ISI-3348 review flagged): the concrete TaskBuilder (coord schema +
// Run CR → wire.Task), the StdioTransport Command builder for `shim run`, and
// the Dispatcher assembly whose TelemetrySink feeds the operator-registered
// toolusage mapper — the production feed path that makes ksquad_* series
// appear on the operator /metrics endpoint the D3 read model scrapes.
//
// Topology decision (v1, operator-spawned): the StdioTransport spawns `shim
// run` as a child process of the operator, the transport's documented
// deployment shape (§10.1 stories 3.4/3.5 — one Task on stdin, JSONL SSE on
// stdout). The shim in turn drives the runtime CLI, so a fully in-cluster
// driven Run additionally needs a runtime reachable in the operator image;
// the pod-side-supervisor alternative (shim + runtime in the sandbox pod, the
// operator bridging stdio over the kube API) is the follow-up topology and
// requires its own ADR — tracked as a follow-up issue, not silently assumed
// here. When the shim binary is not present NewOperatorDispatcher returns
// nil and the drive loop stays in its ledger-only mode (an honest degraded
// state, loudly logged by the caller — never a silently broken dispatch).

// OperatorDispatchConfig assembles the operator-side A2A dispatcher.
type OperatorDispatchConfig struct {
	// DB is the coordination Postgres (coord schema: work item content +
	// claim fence). Required.
	DB *sql.DB
	// Client reads the Run/Agent/ConfigMap CRs (the manager's client).
	// Required.
	Client client.Client
	// Mapper is the operator-registered toolusage mapper (Epic D: the one
	// main.go constructs on the controller-runtime metrics registry). Nil
	// keeps the dispatcher honest — events flow, telemetry degrades to
	// pass-through.
	Mapper *toolusage.Mapper
	// ShimBin is the `shim` binary path (default "shim"; the operator image
	// ships it at /usr/local/bin/shim).
	ShimBin string
	// RuntimeType selects the shim runtime flavor (KSQUAD_RUNTIME_TYPE,
	// §7.2). Empty defers to the shim's own error — a dispatch with no
	// runtime selected must fail loudly, not silently no-op.
	RuntimeType string
	// Stderr receives the shim's diagnostic stream. NEVER the SSE channel;
	// nil discards.
	Stderr io.Writer
	// RunEvents is the inner run-event sink the TelemetrySink decorates
	// (nil = discard: v1 maps telemetry first, forwards verbatim second).
	RunEvents a2a.EventSink
	// ExtraEnv is appended verbatim to the shim environment — a
	// diagnostics/test hook (e.g. proxy vars, the Go helper-process marker).
	// Never secrets: the §7.3 credential has its own mount seam.
	ExtraEnv []string
	// Source overrides the coord read-side (nil = sqlDispatchSource over
	// DB) — the seam tests bind a fake against instead of a live Postgres.
	Source dispatchSource
	// ContextAssemblers builds the §8.5 context assembler used to re-read
	// the Run's pinned context snapshot and inject it as
	// wire.Envelope.SystemContext (story S1, ISI-3600, seam A —
	// recompute-from-snapshot). Nil ships title+body only, the pre-S1
	// behavior, so the field is opt-in and non-regressing.
	ContextAssemblers ContextAssemblers
	// TaskIOMinter, when set, mints the run-scoped task-io token (ISI-3601 S2)
	// injected into the shim env as KSQUAD_COORD_TOKEN. Nil disables task-io
	// env injection entirely — the agent simply gets no token (fail-safe: an
	// absent token makes the coord API refuse the call, never fail-open).
	TaskIOMinter *taskio.Minter
	// TaskIOCoordURL is the in-cluster coord/apiserver base URL injected as
	// KSQUAD_COORD_URL (§AC7: an in-cluster Service, not a public surface).
	// Empty disables task-io injection — both the minter and the URL are
	// needed together for the seam to be usable.
	TaskIOCoordURL string
}

// ContextAssemblers builds a per-namespace §8.5 context assembler over the
// production Sources (pkg/controller/contextsource.Deps implements it). The
// dispatcher re-reads the Run's pinned snapshot through it (Existing set) so
// the injected SystemContext is byte-identical to what the reconciler pinned
// (deterministic resume, AC3).
type ContextAssemblers interface {
	For(namespace string) *contextasm.Assembler
}

// NewOperatorDispatcher resolves the config, verifies the shim binary is
// actually spawnable (exec.LookPath), and returns the assembled Dispatcher —
// or nil when the shim is missing, the caller's signal to stay ledger-only.
func NewOperatorDispatcher(cfg OperatorDispatchConfig) (*a2a.Dispatcher, error) {
	if cfg.DB == nil {
		return nil, errors.New("rundrive.NewOperatorDispatcher: nil DB")
	}
	if cfg.Client == nil {
		return nil, errors.New("rundrive.NewOperatorDispatcher: nil Client")
	}
	shimBin := cfg.ShimBin
	if shimBin == "" {
		shimBin = "shim"
	}
	if _, err := exec.LookPath(shimBin); err != nil {
		return nil, fmt.Errorf("rundrive.NewOperatorDispatcher: shim binary %q not found: %w", shimBin, err)
	}
	d := &operatorDispatch{
		cfg:     cfg,
		shimBin: shimBin,
		source:  cfg.Source,
	}
	if d.source == nil {
		d.source = sqlDispatchSource{db: cfg.DB}
	}
	return &a2a.Dispatcher{
		Client:  a2a.New(&a2a.StdioTransport{Command: d.shimCommand, Stderr: cfg.Stderr}),
		Builder: d.buildTask,
		// Per-Run sink: the TelemetrySink's labels (Run/Agent) are per-Run —
		// one process-wide sink would freeze the agent label across Runs.
		SinkFor: d.sinkFor,
	}, nil
}

// operatorDispatch carries the resolved config + the coord read-side.
type operatorDispatch struct {
	cfg     OperatorDispatchConfig
	shimBin string
	source  dispatchSource
}

// dispatchSource is the coord read-side the TaskBuilder needs, kept minimal
// so tests bind a fake instead of a live Postgres. The prod binding is over
// *sql.DB (sqlDispatchSource below).
type dispatchSource interface {
	// WorkItem returns the work item's title and body (the §8.5 envelope's
	// concrete work instruction).
	WorkItem(ctx context.Context, id string) (title, body string, err error)
	// FenceToken returns the work item's current claim fence (§6.2): the
	// token every artifact write is checked against.
	FenceToken(ctx context.Context, workItemID string) (string, error)
}

// sqlDispatchSource binds dispatchSource to the coord schema.
type sqlDispatchSource struct{ db *sql.DB }

func (s sqlDispatchSource) WorkItem(ctx context.Context, id string) (string, string, error) {
	var title, body sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT title, body FROM coord.work_item WHERE id = $1::uuid`, id).
		Scan(&title, &body)
	if err != nil {
		return "", "", fmt.Errorf("rundrive: read work item %s: %w", id, err)
	}
	return title.String, body.String, nil
}

func (s sqlDispatchSource) FenceToken(ctx context.Context, workItemID string) (string, error) {
	var fence int64
	err := s.db.QueryRowContext(ctx,
		`SELECT fence_token FROM coord.claim WHERE work_item_id = $1::uuid`, workItemID).
		Scan(&fence)
	if err != nil {
		return "", fmt.Errorf("rundrive: read claim fence for %s: %w", workItemID, err)
	}
	return strconv.FormatInt(fence, 10), nil
}

// buildTask is the concrete a2a.TaskBuilder: deterministic on (a2aTaskID,
// runID) — the same inputs rebuild the same Task so a re-drive reattaches
// (C1). The coord schema supplies the durable seams (work item id + content,
// fence token); the Run CR supplies the envelope metadata and the agent
// selection; Epic C's assembler owns the deeper capability seams (the
// immutable status.capabilityManifest and the projected MCP IR — read here,
// never recomputed, ADR-044).
func (d *operatorDispatch) buildTask(ctx context.Context, a2aTaskID, runID string) (wire.Task, error) {
	run, err := d.runByUID(ctx, runID)
	if err != nil {
		return wire.Task{}, fmt.Errorf("rundrive: resolve Run %s: %w", runID, err)
	}
	if run.Spec.WorkItemRef == "" {
		return wire.Task{}, fmt.Errorf("rundrive: Run %s/%s has no workItemRef", run.Namespace, run.Name)
	}
	title, body, err := d.source.WorkItem(ctx, run.Spec.WorkItemRef)
	if err != nil {
		return wire.Task{}, err
	}
	fence, err := d.source.FenceToken(ctx, run.Spec.WorkItemRef)
	if err != nil {
		return wire.Task{}, err
	}

	env := wire.Envelope{
		// v1 envelope: the work item IS the concrete work instruction. The
		// full §8.5 context assembly (repo/memory/project sources) rides the
		// context assembler's own seam — folded in when that surface lands,
		// never half-faked here.
		Input: body,
		Metadata: map[string]string{
			"work_item.title": title,
			"work_item.id":    run.Spec.WorkItemRef,
			"team":            run.Spec.TeamRef.Name,
			"project":         run.Spec.ProjectRef.Name,
			"run.namespace":   run.Namespace,
			"run.name":        run.Name,
		},
	}
	if env.Input == "" {
		env.Input = title // a titled-but-bodiless item still carries its instruction
	}

	// §8.5 context injection (story S1, ISI-3600, seam A): whenever the
	// context side-channel is wired, assemble the tier-framed system/context
	// string. With a pinned snapshot (the normal case — the reconciler pins at
	// Claiming) it re-reads the PINNED revisions/doc-ids + budget/window for a
	// byte-identical resume; WITHOUT one (the status reconciler has not pinned
	// yet — the two controllers race) it assembles fresh rather than silently
	// shipping title+body only. This makes a configured assembler a hard
	// prerequisite of a fully-contextualised dispatch, not a best-effort
	// add-on. SystemContext is ADDITIVE — env.Input still carries the concrete
	// work instruction (AC1). Fail-closed on assembly error (AC4). With the
	// side-channel OFF, SystemContext stays empty: the bare title+body
	// dispatch is unchanged (AC6).
	if d.cfg.ContextAssemblers != nil {
		sysCtx, err := d.assembleSystemContext(ctx, run)
		if err != nil {
			return wire.Task{}, fmt.Errorf("rundrive: assemble system context for run %s/%s: %w", run.Namespace, run.Name, err)
		}
		env.SystemContext = sysCtx
	}

	return wire.Task{
		A2ATaskID:  a2aTaskID,
		WorkItemID: run.Spec.WorkItemRef,
		FenceToken: fence,
		Envelope:   env,
		// CredentialsMounted is the §7.3 contract: the reconciler env-injects
		// the credential Secret into the runtime container. The v1
		// operator-spawned topology mounts no per-user credential into the
		// operator pod, so this is honestly false — the pod-side supervisor
		// topology (follow-up ADR) is where it flips true.
		CredentialsMounted: false,
		// ModelRoute stays zero for fixed-vendor runtimes (spec §11); the
		// BYO-endpoint resolution rides the same follow-up seam.
		ModelRoute: wire.ModelRoute{},
	}, nil
}

// runByUID resolves the Run CR whose uid is runID — the same read shape the
// SpecClassifier uses (List + match; the drive loop already cached the Run
// before dispatch, so this is a cache hit in practice). A Run deleted
// mid-dispatch is an error: dispatch must fail loudly, never drive a ghost.
func (d *operatorDispatch) runByUID(ctx context.Context, runID string) (*api.Run, error) {
	var runs api.RunList
	if err := d.cfg.Client.List(ctx, &runs); err != nil {
		return nil, fmt.Errorf("list Runs: %w", err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) == runID {
			r := runs.Items[i]
			return &r, nil
		}
	}
	return nil, fmt.Errorf("no Run CR with uid %s", runID)
}

// agentName resolves the Run's dispatch agent (first spec.agents entry) for
// the telemetry labels — the D3 panel aggregates per agent.
func (d *operatorDispatch) agentName(ctx context.Context, runID string) string {
	run, err := d.runByUID(ctx, runID)
	if err != nil || len(run.Spec.Agents) == 0 {
		return ""
	}
	return run.Spec.Agents[0].Name
}

// cleanRunID strips the drive loop's lap/disambiguation suffix from an
// a2aTaskID (boundTaskID: runID, runID#lapN or runID/fixture) to recover the
// raw Run uid for CR lookups.
func cleanRunID(taskOrRunID string) string {
	if i := strings.IndexAny(taskOrRunID, "#/"); i >= 0 {
		return taskOrRunID[:i]
	}
	return taskOrRunID
}

// shimCommand builds the `shim run` exec.Cmd (the StdioTransport Command
// seam): minimal environment — PATH plus the §7.2 KSQUAD_* set resolved from
// the Run/Agent CRs — never os.Environ, so no operator secret (DATABASE_URL
// and friends) leaks into a task subprocess. The W3C carrier (D1, finding 3)
// joins the shim's spans onto the Run's trace exactly like the sandbox Boot
// env does.
func (d *operatorDispatch) shimCommand(ctx context.Context, t wire.Task) (*exec.Cmd, error) {
	runID := cleanRunID(t.A2ATaskID)
	run, err := d.runByUID(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("rundrive: resolve Run %s for shim env: %w", runID, err)
	}

	env := []string{"PATH=" + os.Getenv("PATH")}
	if d.cfg.RuntimeType != "" {
		env = append(env, "KSQUAD_RUNTIME_TYPE="+d.cfg.RuntimeType)
	}
	if agent := d.agentName(ctx, runID); agent != "" {
		env = append(env, "KSQUAD_AGENT_NAME="+agent)
	}
	env = append(env,
		"KSQUAD_SQUAD="+run.Spec.TeamRef.Name,
		"KSQUAD_PROJECT="+run.Spec.ProjectRef.Name,
		"KSQUAD_TOOL_USAGE_ENABLED="+strconv.FormatBool(toolusage.Enabled()),
	)
	if model := d.agentModel(ctx, run); model != "" {
		env = append(env, "KSQUAD_MODEL="+model)
	}
	// The MCP IR: Epic C projects it into the per-Run ConfigMap
	// (ksquad-run-<name>-mcp). The sandbox topology mounts it as a volume;
	// operator-spawned we materialize it as a temp file at the env path the
	// shim already reads (K8SQUAD_MCP_CONFIG). Fail-closed per ADR-044: a
	// Run whose envelope demands MCP servers MUST see its IR.
	if path, err := d.materializeMCPConfig(ctx, run); err != nil {
		return nil, err
	} else if path != "" {
		env = append(env, capability.MCPConfigEnvVar+"="+path)
	}
	// D1: the W3C trace carrier — same convention as warmpool's sandbox env.
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	for envName, carrierKey := range map[string]string{"TRACEPARENT": "traceparent", "TRACESTATE": "tracestate"} {
		if v := carrier[carrierKey]; v != "" {
			env = append(env, envName+"="+v)
		}
	}
	// Task-io seam (ISI-3601 S2, AC6): the run-scoped bootstrap vars mirror
	// Paperclip's PAPERCLIP_API_URL / PAPERCLIP_API_KEY / PAPERCLIP_TASK_ID
	// injection so an agent can re-read its task, comment, update status, and
	// check out mid-run. The token is minted per task, bound to (RUN_ID,
	// WORK_ITEM_ID) — own-run only. It joins THIS curated env, never os.Environ,
	// so the minimal-env invariant holds (no DATABASE_URL / operator secret
	// reaches the subprocess). Injection is skipped wholesale unless both a
	// minter and a coord URL are configured, and the Run names a work item —
	// fail-safe (an absent token makes the coord API refuse the call, never
	// fail-open).
	if d.cfg.TaskIOMinter != nil && d.cfg.TaskIOCoordURL != "" && run.Spec.WorkItemRef != "" {
		// The ONE coord token also carries the ISI-3626 role-derived scope
		// (org:write/project:write) the org-ops seam enforces; IC runs mint an
		// empty scope so only the own-task task-io verbs work.
		scopes := d.deriveRunScopes(ctx, run)
		token, err := d.cfg.TaskIOMinter.MintWithScopes(runID, run.Spec.WorkItemRef, d.agentName(ctx, runID), scopes)
		if err != nil {
			return nil, fmt.Errorf("rundrive: mint task-io token for Run %s: %w", runID, err)
		}
		env = append(env,
			taskio.EnvCoordURL+"="+d.cfg.TaskIOCoordURL,
			taskio.EnvCoordToken+"="+token,
			taskio.EnvWorkItemID+"="+run.Spec.WorkItemRef,
			taskio.EnvRunID+"="+runID,
		)
	}

	env = append(env, d.cfg.ExtraEnv...)

	// #nosec G204 -- d.shimBin is the operator's own pod-spec env/config
	// (cfg.ShimBin, default "shim"), validated via exec.LookPath at
	// construction and never derived from Run or request input; argv is the
	// constant "run". Spawning the shim is this dispatcher's job.
	cmd := exec.CommandContext(ctx, d.shimBin, "run")
	cmd.Env = env
	return cmd, nil
}

// assembleSystemContext renders the tier-framed system/context string (seam
// A, story S1). With a pinned snapshot it runs Assemble with Existing set, so
// the same revisions/doc-ids/window/budget the reconciler pinned are re-read —
// the render is byte-identical to the first drive (deterministic resume, AC3),
// and a pinned revision that no longer resolves errors loudly (Sources
// contract). Without a snapshot (the reconciler has not pinned yet) it
// assembles fresh so the dispatch is still fully contextualised, never
// title+body only.
func (d *operatorDispatch) assembleSystemContext(ctx context.Context, run *api.Run) (string, error) {
	if len(run.Spec.Agents) == 0 {
		return "", fmt.Errorf("run %s/%s: context side-channel is configured but the Run has no dispatch agent to resolve the model window", run.Namespace, run.Name)
	}
	agentRef := run.Spec.Agents[0]
	agentNS := agentRef.Namespace
	if agentNS == "" {
		agentNS = run.Namespace
	}
	var agent api.Agent
	if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: agentNS, Name: agentRef.Name}, &agent); err != nil {
		return "", fmt.Errorf("read Agent %s/%s: %w", agentNS, agentRef.Name, err)
	}

	projNS := run.Spec.ProjectRef.Namespace
	if projNS == "" {
		projNS = run.Namespace
	}
	var project api.Project
	if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: projNS, Name: run.Spec.ProjectRef.Name}, &project); err != nil {
		return "", fmt.Errorf("read Project %s/%s: %w", projNS, run.Spec.ProjectRef.Name, err)
	}

	// On resume the window comes from the pinned snapshot, not the live Agent:
	// a spec.model / contextBudgetOverride change after the snapshot was
	// stored must not silently re-budget the resumed envelope (the assembler
	// pins the budget off Existing too). Fresh dispatch resolves from the
	// live model.
	window := contextsource.WindowForModel(agent.Spec.Model)
	if snap := run.Status.ContextSnapshot; snap != nil && snap.ContextWindow != nil {
		window = *snap.ContextWindow
	}

	// The Source resolves the Project CRD in projNS (which honors a
	// cross-namespace projectRef), not the Run's own namespace.
	res, err := d.cfg.ContextAssemblers.For(projNS).Assemble(ctx, contextasm.AssembleRequest{
		Run:           run,
		Agent:         &agent,
		Project:       &project,
		TeamID:        run.Spec.TeamRef.Name,
		ContextWindow: window,
		Existing:      run.Status.ContextSnapshot,
	})
	if err != nil {
		return "", err
	}
	return res.Injection.SystemPrompt(), nil
}

// agentModel resolves the dispatch agent's spec.model (empty = runtime
// default).
func (d *operatorDispatch) agentModel(ctx context.Context, run *api.Run) string {
	if len(run.Spec.Agents) == 0 {
		return ""
	}
	ref := run.Spec.Agents[0]
	ns := ref.Namespace
	if ns == "" {
		ns = run.Namespace
	}
	var agent api.Agent
	if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &agent); err != nil {
		return "" // model resolution is best-effort on this seam; the shim defaults
	}
	return agent.Spec.Model
}

// deriveRunScopes computes the ISI-3626 role-derived privilege scopes stamped
// into the Run's coord token (ADR-0005 D2): org:write for CEO + manager roles,
// project:write for the CEO role, neither for IC roles. It resolves the dispatch
// Agent's Role, lists every Role in that namespace to decide manager/CEO
// structurally, and applies orgops.DeriveScopes — so the grant follows the Role
// graph and NEVER Agent.spec.skillRefs (closing the skill-union loophole). It is
// fail-closed to least privilege: any read failure, or a Role that is not among
// the listed namespace's Roles, yields no scope, so a lookup glitch can never
// widen a token.
func (d *operatorDispatch) deriveRunScopes(ctx context.Context, run *api.Run) []string {
	if len(run.Spec.Agents) == 0 {
		return nil
	}
	ref := run.Spec.Agents[0]
	ns := ref.Namespace
	if ns == "" {
		ns = run.Namespace
	}
	var agent api.Agent
	if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &agent); err != nil {
		return nil
	}
	roleName := agent.Spec.RoleRef.Name
	if roleName == "" {
		return nil
	}
	roleNS := agent.Spec.RoleRef.Namespace
	if roleNS == "" {
		roleNS = ns
	}
	var roles api.RoleList
	if err := d.cfg.Client.List(ctx, &roles, client.InNamespace(roleNS)); err != nil {
		return nil
	}
	views := make([]orgops.RoleView, 0, len(roles.Items))
	var target orgops.RoleView
	found := false
	for i := range roles.Items {
		rv := orgops.RoleView{
			Name:      roles.Items[i].Name,
			ReportsTo: roles.Items[i].Labels[orgops.LabelReportsTo],
		}
		views = append(views, rv)
		if roles.Items[i].Name == roleName {
			target, found = rv, true
		}
	}
	if !found {
		return nil // cross-namespace roleRef we could not situate in the graph — fail closed.
	}
	return orgops.DeriveScopes(target, views)
}

// materializeMCPConfig copies the Run's projected MCP IR ConfigMap to a temp
// file and returns its path ("" when the Run demanded no MCP servers). v1
// rough edge, deliberately bounded: the file lives in os.TempDir for the
// task's lifetime; the pod-side topology replaces this with the volume mount.
func (d *operatorDispatch) materializeMCPConfig(ctx context.Context, run *api.Run) (string, error) {
	if run.Status.CapabilityManifest == nil || len(run.Status.CapabilityManifest.MCPEndpoints) == 0 {
		return "", nil
	}
	var cm corev1.ConfigMap
	if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: capability.MCPConfigMapName(run)}, &cm); err != nil {
		return "", fmt.Errorf("rundrive: read MCP IR configmap for run %s/%s: %w", run.Namespace, run.Name, err)
	}
	ir, ok := cm.Data[capability.MCPConfigFile]
	if !ok {
		return "", fmt.Errorf("rundrive: MCP IR configmap %s lacks key %s", cm.Name, capability.MCPConfigFile)
	}
	dir, err := os.MkdirTemp("", "ksquad-mcp-*")
	if err != nil {
		return "", fmt.Errorf("rundrive: temp dir for MCP IR: %w", err)
	}
	path := filepath.Join(dir, capability.MCPConfigFile)
	if err := os.WriteFile(path, []byte(ir), 0o600); err != nil {
		return "", fmt.Errorf("rundrive: write MCP IR: %w", err)
	}
	return path, nil
}

// sinkFor wraps the run-event sink with the operator's mapper — the exact
// TelemetrySink wiring the ISI-3348 review demanded as the production caller:
// map first (ksquad_* series onto the operator registry), forward verbatim
// second, telemetry failures never abort the dispatch.
func (d *operatorDispatch) sinkFor(runID string) a2a.EventSink {
	return a2a.NewTelemetrySink(d.cfg.RunEvents, d.cfg.Mapper, toolusage.Labels{
		RunID: cleanRunID(runID),
		Agent: d.agentName(context.Background(), cleanRunID(runID)),
	})
}
