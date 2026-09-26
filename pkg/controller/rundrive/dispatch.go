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
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/a2a"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/contextasm"
	"github.com/K8squad/K8squad/pkg/controller/contextsource"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
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
	// LLMStatus, when set, is chained AHEAD of RunEvents as the
	// TelemetrySink's inner sink (ISI-4238): it projects EventUsage /
	// EventStatus.TraceID onto Run.Status (llmInteractions,
	// totalTokenUsage, traceID) before any other consumer sees the event.
	// It swallows its own failures, so telemetry/run-event forwarding is
	// unaffected. Nil = projection off (tests, ledger-only lanes).
	LLMStatus *RunLLMStatusWriter
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
	if d.now == nil {
		d.now = time.Now
	}
	return &a2a.Dispatcher{
		Client:  a2a.New(sandboxTransport{d}),
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
	// now is the clock supervisorURL reads to age the bound sandbox pod against
	// podIPReadyDeadline (nil = time.Now; the seam tests pin it).
	now func() time.Time
}

// errSandboxPending marks the benign bind/readiness race: the sandbox pod is
// bound but has not yet been scheduled+networked+readied, so it has no PodIP or
// has not passed its readiness probe yet. It is the EXPECTED path on the first
// reconcile pass(es) after bind, cleared on requeue once the CNI assigns an IP
// AND the supervisor's :8080 probe goes Ready (ISI-4441; not-Ready gate added
// for ISI-5028). The driver requeues quietly on it instead of recording a span
// exception — only a pod that stays unready past podIPReadyDeadline (a genuine
// scheduling/CNI/supervisor failure) escalates to a loud, recorded error so it
// stands out from the normal race.
var errSandboxPending = errors.New("rundrive: sandbox pod not ready to dispatch yet")

// podIPReadyDeadline bounds how long the pre-IP / not-yet-Ready case stays
// benign. A bound pod normally gets its IP and passes readiness within seconds;
// past this the unready state is no longer a race but a scheduling/CNI fault or
// a dead supervisor worth surfacing on the reconcile span.
const podIPReadyDeadline = 2 * time.Minute

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

	// Resolve the EFFECTIVE model endpoint across the Model-Per-Role tiers
	// (ISI-4430 S4: agent → role → system-default ModelConfig). An Agent with
	// no BYO modelEndpointRef resolves to an empty ModelRoute (the runtime's
	// own provider default), but the winning tier's MODEL still rides through:
	// a role- or default-tier model reaches the shim exactly as an agent-tier
	// one does. Resolution is fail-closed: a dangling endpoint Secret, a
	// malformed URL, OR no model in any tier (ErrNoModel — never even the
	// system-default) aborts the dispatch rather than silently routing the Run
	// to a paid provider default (weak local models must never fail silently
	// mid-Run — story 5.7 + D3 fail-closed).
	var modelRoute wire.ModelRoute
	var modelTier string
	if len(run.Spec.Agents) > 0 {
		endpoint, tier, err := d.resolveEffectiveEndpoint(ctx, run)
		if err != nil {
			return wire.Task{}, err
		}
		// The winning tier is the resolved model-origin (agent|role|default);
		// it rides the submit payload so the shim stamps ksquad.model.tier on
		// the run.start span (ISI-4430 S5). Known even when the route is empty
		// (a role- or default-tier model with no BYO endpoint), which is why it
		// is captured off the resolution rather than off modelRoute.
		modelTier = string(tier)
		if endpoint.BaseURL != "" {
			modelRoute = wire.ModelRoute{
				Endpoint: endpoint.BaseURL,
				Model:    endpoint.Model,
				Token:    endpoint.Token,
			}
		}
		// Persist the winning tier into Run provenance (Run.status.modelSegments
		// tier origin). Best-effort: a lost provenance stamp is an observability
		// gap, not a correctness failure — the model has already resolved and
		// will route correctly, so a status-write hiccup must not abort the
		// dispatch. Idempotent, so a re-drive (C1) never duplicates the segment.
		d.recordModelProvenance(ctx, run, endpoint, tier)
	}

	// Per-run identity (ISI-4439): carry agent/team/project on the submit
	// payload so the warm-pool sandbox shim — whose pod env is generic (no
	// KSQUAD_AGENT_NAME/SQUAD/PROJECT) — stamps run/llm/tool spans with the
	// real identity. Same sources shimCommand uses for the stdio env: the Run's
	// TeamRef/ProjectRef and the dispatch agent (first spec.agents entry).
	identity := wire.AgentIdentity{
		Squad:   run.Spec.TeamRef.Name,
		Project: run.Spec.ProjectRef.Name,
	}
	if len(run.Spec.Agents) > 0 {
		identity.Name = run.Spec.Agents[0].Name
	}

	// Epic C / ADR-044 step 6 (ISI-5017): deliver the resolved MCP IR + the
	// per-run credential VALUES on the task envelope. A warm-pool sandbox pod
	// boots GENERIC and is immutable after Bind — volumes/env cannot be added
	// per run — so the pod-side supervisor (cmd/shim) can never receive the
	// IR via the projected ConfigMap mount. Shipping it on the envelope is the
	// same per-run-identity trick Task.Identity already uses. Fail-closed: a
	// Run whose recorded manifest demands MCP servers must never dispatch a
	// generic pod without its IR/credentials.
	mcpEndpoints, mcpTokens, err := d.mcpEnvelope(ctx, run)
	if err != nil {
		return wire.Task{}, err
	}

	return wire.Task{
		A2ATaskID:  a2aTaskID,
		WorkItemID: run.Spec.WorkItemRef,
		FenceToken: fence,
		Envelope:   env,
		Identity:   identity,
		// MCPEndpoints/MCPTokenEnv deliver the capability envelope to the
		// pod per run (ISI-5017). MCPTokenEnv is secret material — never
		// logged.
		MCPEndpoints: mcpEndpoints,
		MCPTokenEnv:  mcpTokens,
		// CredentialsMounted is the §7.3 contract: the reconciler env-injects
		// the credential Secret into the runtime container. The v1
		// operator-spawned topology mounts no per-user credential into the
		// operator pod, so this is honestly false — the pod-side supervisor
		// topology (follow-up ADR) is where it flips true.
		CredentialsMounted: false,
		// ModelRoute carries the resolved BYO endpoint (§11, §10.3). Empty
		// means the runtime's own provider default (fixed-vendor).
		ModelRoute: modelRoute,
		// ModelTier is the resolved model-origin (agent|role|default) the shim
		// stamps as ksquad.model.tier on the run.start span (ISI-4430 S5).
		ModelTier: modelTier,
	}, nil
}

// runByUID resolves the Run CR whose uid is runID — the same read shape the
// SpecClassifier uses (List + match; the drive loop already cached the Run
// before dispatch, so this is a cache hit in practice). A Run deleted
// mid-dispatch is an error: dispatch must fail loudly, never drive a ghost.
func (d *operatorDispatch) runByUID(ctx context.Context, runID string) (*api.Run, error) {
	return runByUIDFrom(ctx, d.cfg.Client, runID)
}

// runByUIDFrom resolves the Run CR whose UID matches runID over the given client
// (the Run drive loop keys on the Run's real uid, not name). Shared by the shim
// dispatch path and the warm-pool Bind-path credential writer, which is handed
// only the run-id string from the coord bind frame.
func runByUIDFrom(ctx context.Context, c client.Client, runID string) (*api.Run, error) {
	var runs api.RunList
	if err := c.List(ctx, &runs); err != nil {
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
// sandboxTransport picks the dispatch transport per task (ISI-4188 gap 6):
// when the Run holds a bound sandbox (status.sandboxRef, stamped by the
// claiming_sandbox step's warm-pool Bind), the task is POSTed to the pod's
// in-pod supervisor (`POST /task`, cmd/shim supervisor) — the topology where
// the runtime CLI actually exists, since the sandbox image ships shim+CLI
// while the operator image ships the shim alone. With no sandbox bound it
// falls back to the operator-spawned `shim run` stdio path (the §10.1 v1
// topology) so the shim-only conformance lanes keep working.
type sandboxTransport struct {
	d *operatorDispatch
}

func (st sandboxTransport) Submit(ctx context.Context, t wire.Task) (a2a.Session, error) {
	url, err := st.d.supervisorURL(ctx, t.A2ATaskID)
	if err != nil {
		return nil, err
	}
	if url != "" {
		ht := &a2a.HTTPTransport{
			URL: func(context.Context, wire.Task) (string, error) { return url, nil },
		}
		return ht.Submit(ctx, t)
	}
	std := &a2a.StdioTransport{Command: st.d.shimCommand, Stderr: st.d.cfg.Stderr}
	return std.Submit(ctx, t)
}

// supervisorURL resolves the bound sandbox pod's supervisor task endpoint
// (http://<podIP>:8080/task — the port cmd/shim's supervisorAddr and the
// warm-pool Boot probes agree on). Returns "" when the Run has no sandboxRef
// (pre-bind ordering or a sandbox-less lane); a bound sandbox whose pod is
// missing is a loud error so the re-drive retries instead of silently degrading
// to the stdio path (which cannot exec the runtime CLI). A bound pod that is not
// yet networked or not yet Ready returns errSandboxPending (a benign requeue
// signal) until it ages past podIPReadyDeadline, after which it is a loud error
// (ISI-4441; not-Ready gate added for ISI-5028).
func (d *operatorDispatch) supervisorURL(ctx context.Context, a2aTaskID string) (string, error) {
	run, err := d.runByUID(ctx, cleanRunID(a2aTaskID))
	if err != nil {
		return "", err
	}
	ref := run.Status.SandboxRef
	if ref == nil || ref.Name == "" {
		return "", nil
	}
	ns := ref.Namespace
	if ns == "" {
		ns = run.Namespace
	}
	var pod corev1.Pod
	if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &pod); err != nil {
		return "", fmt.Errorf("rundrive: resolve sandbox pod %s/%s: %w", ns, ref.Name, err)
	}
	// Dispatchable only once the pod is BOTH networked AND Ready. PodIP alone is
	// not sufficient: the supervisor serves :8080 only after the container
	// boots, so a fresh pod can carry an IP while :8080 still refuses
	// connections. Posting to it races the supervisor's boot and fails the run
	// fast with an opaque connection-refused ("readiness refused" / silent
	// engine settle, ISI-5028) instead of requeuing until the probe passes.
	// Treat the not-yet-Ready window exactly like the pre-IP window.
	if pod.Status.PodIP == "" || !sandboxPodReady(&pod) {
		if age := d.now().Sub(pod.CreationTimestamp.Time); age > podIPReadyDeadline {
			// Past the readiness deadline: no longer a race but a stuck pod
			// (scheduling/CNI fault, or a supervisor that never came up). Loud,
			// so it stands out on the span.
			if pod.Status.PodIP == "" {
				return "", fmt.Errorf("rundrive: sandbox pod %s/%s has no IP after %s", ns, ref.Name, age.Round(time.Second))
			}
			return "", fmt.Errorf("rundrive: sandbox pod %s/%s not Ready after %s (supervisor :8080 not serving)", ns, ref.Name, age.Round(time.Second))
		}
		// Benign bind/readiness race: requeue quietly, no span exception.
		return "", fmt.Errorf("resolve sandbox %s/%s: %w", ns, ref.Name, errSandboxPending)
	}
	return "http://" + pod.Status.PodIP + ":8080/task", nil
}

// sandboxPodReady reports the pod's Ready condition. The warm-pool supervisor
// serves its readiness probe on :8080 the moment it boots (cmd/shim
// supervisorAddr), so Ready means the in-pod /task endpoint is accepting
// connections — the precondition for dispatching a task to it.
func sandboxPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

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
	// The per-skill body+envelope projection (ADR-0004 S-B): the reconciler
	// projects one ConfigMap per granted INLINE skill
	// (ksquad-run-<name>-skill-<skill>); the sandbox topology mounts them as
	// volumes. Operator-spawned we materialize the whole
	// ${KSQUAD_SKILLS_DIR}/<name>/{SKILL.md,permissions.json} tree into a
	// temp dir, exactly the materializeMCPConfig discipline. Fail-closed: a
	// Run whose manifest granted inline skills MUST see its skill tree.
	if dir, err := d.materializeSkills(ctx, run); err != nil {
		return nil, err
	} else if dir != "" {
		env = append(env, capability.SkillsDirEnvVar+"="+dir)
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
		// Render the credential content through the shared taskio.RunCredential so
		// the env carrier here and the warmpool/sandbox Secret carrier (topology 2,
		// ADR-0007) stay byte-identical — same fields, same order. The trace carrier
		// is left off here because the shim emits TRACEPARENT/TRACESTATE
		// unconditionally above (out of band of task-io); the Secret carrier folds it
		// into the same struct when it wires the Bind path.
		cred := taskio.RunCredential{
			CoordURL:   d.cfg.TaskIOCoordURL,
			Token:      token,
			WorkItemID: run.Spec.WorkItemRef,
			RunID:      runID,
		}
		env = append(env, cred.EnvKV()...)
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

	// M1.2 (ISI-4128): TeamID is the team's Postgres uuid (scoped memory
	// recall keys on coord.work_item.team_id = Team CR uid), never the CR
	// name — resolve the Team like the run controller's snapshot path.
	teamNS := run.Spec.TeamRef.Namespace
	if teamNS == "" {
		teamNS = run.Namespace
	}
	var team api.Team
	if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: teamNS, Name: run.Spec.TeamRef.Name}, &team); err != nil {
		return "", fmt.Errorf("rundrive: read Team %s/%s for run %s/%s: %w", teamNS, run.Spec.TeamRef.Name, run.Namespace, run.Name, err)
	}
	// The Source resolves the Project CRD in projNS (which honors a
	// cross-namespace projectRef), not the Run's own namespace.
	res, err := d.cfg.ContextAssemblers.For(projNS).Assemble(ctx, contextasm.AssembleRequest{
		Run:           run,
		Agent:         &agent,
		Project:       &project,
		TeamID:        string(team.UID),
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
	// Model-Per-Role (ISI-4430 S4): the KSQUAD_MODEL the shim runs on is the
	// EFFECTIVE model across tiers (agent → role → default), not the agent
	// tier alone — so an Agent with no spec.model but a Role that sets one
	// dispatches on the Role's model. Best-effort on THIS seam: any resolution
	// gap yields "" and the shim falls back to its own provider default (the
	// hard fail-closed guarantee lives at admission and in buildTask's
	// ErrNoModel abort, so an empty here is a degraded-not-wrong shim env).
	endpoint, _, err := d.resolveEffectiveEndpoint(ctx, run)
	if err != nil {
		return ""
	}
	return endpoint.Model
}

// roleFor resolves the Agent's Role via spec.roleRef (Model-Per-Role S4). An
// empty roleRef, or a role that no longer exists, contributes no role tier
// (nil) rather than failing the dispatch: a dangling roleRef is admission's
// rejection to own (GuardAgentRole), and a role deleted after admission simply
// falls the resolver through to the system-default tier — never empty. Only a
// TRANSIENT read error fails closed, so a lookup glitch can never silently
// widen or drop the model.
func (d *operatorDispatch) roleFor(ctx context.Context, agent *api.Agent) (*api.Role, error) {
	if agent.Spec.RoleRef.Name == "" {
		return nil, nil
	}
	ns := agent.Spec.RoleRef.Namespace
	if ns == "" {
		ns = agent.Namespace
	}
	var role api.Role
	if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: agent.Spec.RoleRef.Name}, &role); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("rundrive: resolve Role %s/%s for Agent %s: %w", ns, agent.Spec.RoleRef.Name, agent.Name, err)
	}
	return &role, nil
}

// resolveEffectiveEndpoint walks the Model-Per-Role tiers (agent → role →
// system-default ModelConfig) for the Run's dispatch Agent (first spec.agents
// entry) and returns the effective PRIMARY endpoint plus the tier that supplied
// it (ISI-4430 S4, plan rev v3 §4d). It is the SINGLE effective-model seam both
// dispatch topologies read — the a2a wire.Task (buildTask) and the
// operator-spawned KSQUAD_MODEL env (agentModel) — so the model the shim runs on
// is identical whichever topology drives the Run.
//
// Fail-closed (D3): ErrNoModel (no tier supplies a model) and every endpoint
// resolution failure (dangling Secret, malformed URL) propagate as errors — the
// caller must abort rather than dispatch an empty model.
//
// ponytail: SystemNamespace is left at the resolver default ("k8squad-system",
// matching the operator's own default namespace and the Helm chart) rather than
// threaded from POD_NAMESPACE — a known ceiling if the operator ever runs in a
// non-default namespace; wire cfg.SystemNamespace when that lands.
func (d *operatorDispatch) resolveEffectiveEndpoint(ctx context.Context, run *api.Run) (modelendpoint.Endpoint, modelendpoint.Tier, error) {
	ref := run.Spec.Agents[0]
	ns := ref.Namespace
	if ns == "" {
		ns = run.Namespace
	}
	var agent api.Agent
	if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &agent); err != nil {
		return modelendpoint.Endpoint{}, "", fmt.Errorf("rundrive: resolve dispatch Agent %s/%s: %w", ns, ref.Name, err)
	}
	role, err := d.roleFor(ctx, &agent)
	if err != nil {
		return modelendpoint.Endpoint{}, "", err
	}
	resolver := modelendpoint.Resolver{Reader: d.cfg.Client}
	primary, _, tier, _, err := resolver.ResolveEffective(ctx, &agent, role)
	if err != nil {
		return modelendpoint.Endpoint{}, "", fmt.Errorf("rundrive: resolve effective model for Agent %s/%s: %w", ns, agent.Name, err)
	}
	return primary, tier, nil
}

// recordModelProvenance stamps the winning tier's model onto
// Run.status.modelSegments (ISI-4430 S4 provenance origin). Idempotent: if the
// latest open segment already serves this model+tier it is a no-op, so a
// re-drive of the deterministic builder (C1) never appends a duplicate. A
// provider-default run with no resolved model name (ep.Model == "") records
// nothing — there is no model to attribute. Best-effort: a status-write failure
// is logged, never returned, because provenance is observability and must not
// abort a Run whose model already resolved.
func (d *operatorDispatch) recordModelProvenance(ctx context.Context, run *api.Run, ep modelendpoint.Endpoint, tier modelendpoint.Tier) {
	if ep.Model == "" {
		return
	}
	for _, s := range run.Status.ModelSegments {
		if s.EndedAt == nil && s.Model == ep.Model && s.Tier == string(tier) {
			return // already recorded for this dispatch — idempotent
		}
	}
	patched := run.DeepCopy()
	patched.Status.ModelSegments = modelendpoint.OpenSegment(run.Status.ModelSegments, ep, tier, metav1.Now())
	if err := d.cfg.Client.Status().Patch(ctx, patched, client.MergeFrom(run)); err != nil {
		slog.WarnContext(ctx, "rundrive: could not record model provenance segment (best-effort)",
			"run.name", run.Name, "run.namespace", run.Namespace,
			"model", ep.Model, "tier", string(tier), "err", err)
	}
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
	return runScopesFor(ctx, d.cfg.Client, run)
}

// runScopesFor derives the ISI-3626 role scopes (org:write/project:write) for a
// Run from its first Agent's Role, over the given client. It is the SINGLE scope
// source both dispatch topologies mint against — the operator-spawned shim
// (operatorDispatch.deriveRunScopes) and the warm-pool Bind-path credential
// writer — so a warm-pool agent gets byte-identical scopes to a shim agent
// (scope parity). Fail-closed: any resolution gap returns nil (IC/no-scope).
func runScopesFor(ctx context.Context, c client.Client, run *api.Run) []string {
	if len(run.Spec.Agents) == 0 {
		return nil
	}
	ref := run.Spec.Agents[0]
	ns := ref.Namespace
	if ns == "" {
		ns = run.Namespace
	}
	var agent api.Agent
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &agent); err != nil {
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
	if err := c.List(ctx, &roles, client.InNamespace(roleNS)); err != nil {
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

// mcpEnvelope resolves the per-run MCP IR + credential VALUES to ship on the
// task envelope (ISI-5017). The IR is rebuilt from the Run's IMMUTABLE
// capability manifest — the same audit truth the projected ConfigMap follows
// (capability.EndpointsFromManifest) — so the envelope can never drift from
// what Run assembly recorded. For each endpoint carrying a CredentialSecretRef
// the referenced per-run Secret is read and its value mapped under the env
// NAME the IR references (capability.CredentialEnvName); that is the value the
// shim layers onto the runtime subprocess env, because a warm-pool pod cannot
// gain the SecretKeyRef projection after Bind.
//
// Fail-closed (ADR-044): a manifest that demands MCP servers but whose IR or
// credential Secret cannot be resolved aborts the dispatch rather than
// launching a generic pod with a silently missing capability. Returns nil,nil
// for a bare manifest (no MCP demand).
func (d *operatorDispatch) mcpEnvelope(ctx context.Context, run *api.Run) ([]capability.Endpoint, map[string]string, error) {
	endpoints := capability.EndpointsFromManifest(run.Status.CapabilityManifest)
	if len(endpoints) == 0 {
		return nil, nil, nil
	}
	tokens := map[string]string{}
	for _, ep := range endpoints {
		ref := ep.CredentialSecretRef
		if ref == nil || ref.Name == "" {
			continue
		}
		var sec corev1.Secret
		if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: ref.Name}, &sec); err != nil {
			return nil, nil, fmt.Errorf("rundrive: read MCP credential secret %s/%s for server %s (fail-closed): %w", run.Namespace, ref.Name, ep.Name, err)
		}
		key := ref.Key
		if key == "" {
			// Mirrors pkg/capability's defaultCredentialKey (the catalog
			// convention the pod-seam SecretKeyRef reads when the ref names no
			// key). Kept as a literal to avoid exporting an internal const.
			key = "token"
		}
		raw, ok := sec.Data[key]
		if !ok || len(raw) == 0 {
			return nil, nil, fmt.Errorf("rundrive: MCP credential secret %s/%s lacks key %q for server %s (fail-closed)", run.Namespace, ref.Name, key, ep.Name)
		}
		names := ep.EnvNames
		if len(names) == 0 {
			names = []string{capability.CredentialEnvName(ep.Name)}
		}
		for _, name := range names {
			tokens[name] = string(raw)
		}
	}
	if len(tokens) == 0 {
		return endpoints, nil, nil
	}
	return endpoints, tokens, nil
}

// materializeSkills copies the Run's per-skill projection ConfigMaps into
// the on-disk tree the shim reads ("" when the Run granted no inline
// skills): ${dir}/<name>/SKILL.md + ${dir}/<name>/permissions.json at mode
// 0o600, KSQUAD_SKILLS_DIR=<dir>. v1 rough edge mirroring
// materializeMCPConfig, deliberately bounded: the tree lives in os.TempDir
// for the task's lifetime; the pod-side topology replaces this with the
// real volume mounts. Fail-closed: a projected skill whose ConfigMap or
// file keys are missing aborts the dispatch, never a silent partial tree.
func (d *operatorDispatch) materializeSkills(ctx context.Context, run *api.Run) (string, error) {
	if run.Status.CapabilityManifest == nil {
		return "", nil
	}
	var inline []api.GrantedSkill
	for _, s := range run.Status.CapabilityManifest.Skills {
		if s.SourceType == api.SkillSourceInline {
			inline = append(inline, s)
		}
	}
	if len(inline) == 0 {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "ksquad-skills-*")
	if err != nil {
		return "", fmt.Errorf("rundrive: temp dir for skills tree: %w", err)
	}
	for _, s := range inline {
		var cm corev1.ConfigMap
		if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: capability.SkillsConfigMapName(run, s.Name)}, &cm); err != nil {
			return "", fmt.Errorf("rundrive: read skill configmap for run %s/%s skill %s: %w", run.Namespace, run.Name, s.Name, err)
		}
		body, ok := cm.Data[capability.SkillBodyFile]
		if !ok {
			return "", fmt.Errorf("rundrive: skill configmap %s lacks key %s", cm.Name, capability.SkillBodyFile)
		}
		perms, ok := cm.Data[capability.SkillPermissionsFile]
		if !ok {
			return "", fmt.Errorf("rundrive: skill configmap %s lacks key %s", cm.Name, capability.SkillPermissionsFile)
		}
		skillDir := filepath.Join(dir, s.Name)
		if err := os.MkdirAll(skillDir, 0o700); err != nil {
			return "", fmt.Errorf("rundrive: mkdir for skill %s: %w", s.Name, err)
		}
		// Two files, two sources, one dir (D8): the body is data copied
		// verbatim; the envelope is the reconciler-authored authority the
		// ConfigMap already carries — neither is derived from the other.
		if err := os.WriteFile(filepath.Join(skillDir, capability.SkillBodyFile), []byte(body), 0o600); err != nil {
			return "", fmt.Errorf("rundrive: write body for skill %s: %w", s.Name, err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, capability.SkillPermissionsFile), []byte(perms), 0o600); err != nil {
			return "", fmt.Errorf("rundrive: write permissions for skill %s: %w", s.Name, err)
		}
	}
	return dir, nil
}

// sinkFor wraps the run-event sink with the operator's mapper — the exact
// TelemetrySink wiring the ISI-3348 review demanded as the production caller:
// map first (ksquad_* series onto the operator registry), forward verbatim
// second, telemetry failures never abort the dispatch. The ISI-4238 LLM
// status projection (when configured) chains as the innermost sink: the CR
// projection sees the raw event before any other inner consumer, and its
// own failures are internal (never abort the dispatch).
func (d *operatorDispatch) sinkFor(runID string) a2a.EventSink {
	inner := d.cfg.RunEvents
	if d.cfg.LLMStatus != nil {
		inner = chainSinks(d.cfg.LLMStatus, inner)
	}
	return a2a.NewTelemetrySink(inner, d.cfg.Mapper, d.telemetryLabels(cleanRunID(runID)))
}

// telemetryLabels resolves the WS-A run-trace identity set (ISI-4382,
// ADR-0021 D1) for a run's spans from its CR: run id + agent + team +
// project + ticket (workItemRef) + sandbox pod. A single CR lookup feeds
// every field (replacing the former agentName-only lookup). A lookup miss
// degrades to run.id alone — telemetry is observational, never load-bearing,
// so a missing CR must never abort the dispatch.
func (d *operatorDispatch) telemetryLabels(runID string) toolusage.Labels {
	l := toolusage.Labels{RunID: runID}
	run, err := d.runByUID(context.Background(), runID)
	if err != nil {
		return l
	}
	if len(run.Spec.Agents) > 0 {
		l.Agent = run.Spec.Agents[0].Name
	}
	l.Team = run.Spec.TeamRef.Name
	l.Project = run.Spec.ProjectRef.Name
	l.WorkItemRef = run.Spec.WorkItemRef
	if run.Status.SandboxRef != nil {
		l.SandboxPod = run.Status.SandboxRef.Name
	}
	return l
}

// chainSinks fans one event out to a then b (a first: the projection must
// observe the raw event even when the downstream sink is the discard nil).
// Errors: a's failures are already internal (RunLLMStatusWriter never
// errors); b's error wins so the sink contract stays honest for the real
// inner sink.
func chainSinks(a, b a2a.EventSink) a2a.EventSink {
	return a2a.SinkFunc(func(ctx context.Context, ev wire.Event) error {
		if err := a.Event(ctx, ev); err != nil {
			return err
		}
		if b == nil {
			return nil
		}
		return b.Event(ctx, ev)
	})
}
