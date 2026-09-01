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

// ksquad-operator is the epic-2/3 controller-manager: it hosts the Run
// reconciler that projects the durable coord reconcile_step onto Run.status
// (ISI-2655, Story 3.1), the Run DRIVE loop that advances that durable
// machine — warm-pool bind, A2A dispatch marker, artifact collect, the §5.3
// death/retry lap, and the 3.7 rate-limit pause/resume (ISI-2883) — the
// repo-sync mirror (11.1) and the Team tenancy reconciler (4.1).
//
// Leader election is ON by default (arch §5.2, AC4 availability): exactly one
// replica reconciles at a time, so a rolling restart or node loss fails over
// without two managers racing to patch the same Run.status. The fenced coord
// store (pkg/coord) is the durable guard against a zombie writer, but leader
// election keeps the steady state single-writer and avoids needless patch churn.
//
// The coord DSN gates the Run controllers (projector + driver) and repo-sync:
// without one the operator still elects and serves probes (e.g. a probe-only
// smoke deploy before the DB is provisioned).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the coord pool

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	clienta2a "github.com/K8squad/K8squad/internal/a2a"
	credentialctrl "github.com/K8squad/K8squad/pkg/controller/credential"
	mcpserverctrl "github.com/K8squad/K8squad/pkg/controller/mcpserver"
	otelgate "github.com/K8squad/K8squad/pkg/controller/otelgate"
	reposync "github.com/K8squad/K8squad/pkg/controller/reposync"
	runctrl "github.com/K8squad/K8squad/pkg/controller/run"
	rundrive "github.com/K8squad/K8squad/pkg/controller/rundrive"
	teamctrl "github.com/K8squad/K8squad/pkg/controller/team"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/issuesync"
	networkpkg "github.com/K8squad/K8squad/pkg/networkpolicy"
	"github.com/K8squad/K8squad/pkg/scm"
	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
	"github.com/K8squad/K8squad/pkg/toolchain"
	kubepool "github.com/K8squad/K8squad/pkg/warmpool"
	workspacepkg "github.com/K8squad/K8squad/pkg/workspace"
)

// leaderElectionID is the ConfigMap/Lease name the manager coordinates on. It is
// binary-specific so the operator never contends with another ksquad manager.
const leaderElectionID = "ksquad-operator.ksquad.io"

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ksquadv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr, coordDSN string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health/readiness probes bind to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. Ensures a single active reconciler (AC4).")
	flag.StringVar(&coordDSN, "coord-dsn", os.Getenv("DATABASE_URL"),
		"Coordination Postgres DSN (defaults to $DATABASE_URL). When empty, the Run reconciler is not registered.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// One signal context drives the whole process: the OTel spine flushes on it,
	// and the manager stops on it. (ctrl.SetupSignalHandler must be called once.)
	ctx := ctrl.SetupSignalHandler()

	// Install the OpenTelemetry spine (ISI-2915/ISI-3103): W3C propagation, a
	// TracerProvider so every Run drive pass is one span, and the otelslog bridge
	// so structured logs carry trace_id/span_id. Exports to stdout for now.
	_, otelShutdown, err := telemetry.Setup(ctx, telemetry.Options{ServiceName: "ksquad-operator"})
	if err != nil {
		ctrl.Log.Error(err, "unable to initialize OpenTelemetry spine")
		os.Exit(1)
	}
	defer func() {
		// Flush on a fresh bounded context: the signal ctx is already cancelled
		// by the time the manager returns, and Shutdown must still drain buffers.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(flushCtx); err != nil {
			ctrl.Log.Error(err, "OpenTelemetry shutdown")
		}
	}()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// The Run reconciler projects the durable coord reconcile_step onto Run.status
	// (ISI-2655 slice-3). Its StepSource is the read-only coord.ReconcileStepReader
	// over the coordination Postgres. Registration is gated on a DSN: without one
	// the reconciler has no source and would panic on the first Run event, so the
	// operator still elects and serves probes but does not register the controller
	// (e.g. a probe-only smoke deploy before the DB is provisioned).
	// The A2A dispatch feed (ISI-3352) is built inside the coord-DSN branch
	// below; the shutdown drain at the tail needs it in this scope.
	var a2aDispatcher *clienta2a.Dispatcher
	if coordDSN == "" {
		ctrl.Log.Info("Run reconciler not registered: no coord DSN (set --coord-dsn or $DATABASE_URL)")
	} else {
		db, err := sql.Open("pgx", coordDSN)
		if err != nil {
			ctrl.Log.Error(err, "unable to open coord Postgres pool")
			os.Exit(1)
		}
		// sql.Open is lazy; per-reconcile read failures surface through the
		// StepSource error and requeue rather than crashing the manager.
		// Epic B (ISI-3286): the Run reconciler also converges the per-Run
		// toolchain RBAC union (per-Run Role bound to the managed
		// ksquad-agent SA, released at terminal phase) and records the
		// grant on Run.status. Platform config (cluster-catalog namespace,
		// cluster-scope opt-in) comes from the deployment env the Helm
		// chart sets.
		// Epic C (ISI-3287): the Run reconciler also resolves the Run's
		// capability envelope pre-dispatch, stamps the immutable
		// Run.status.capabilityManifest and projects the MCP IR ConfigMap
		// the runtime adapters consume (ADR-044).
		if err := (&runctrl.Reconciler{
			Source:    coord.NewReconcileStepReader(db),
			RBAC:      runctrl.NewRBACRenderer(mgr.GetClient(), toolchain.PlatformConfigFromEnv()),
			Assembler: runctrl.NewAssembler(mgr.GetClient(), toolchain.PlatformConfigFromEnv()),
		}).SetupWithManager(mgr); err != nil {
			ctrl.Log.Error(err, "unable to set up Run reconciler")
			os.Exit(1)
		}

		// The Run DRIVE loop (Story 3.1/3.2/3.7, ISI-2883): advances the
		// durable reconcile machine for every Run CR — level-triggered, every
		// pass re-derived from Postgres (the §6.4 crash-safe contract). Its
		// pieces: the per-Run Store/Effects bindings over this coord pool,
		// the warm-pool SandboxBinder with real kube Provisioner for pod creation,
		// the 3.7 resume timer (one per leader; a single durable wake per pause
		// episode — never a poll), and the §5.3 death/retry lap (expired lease →
		// fence-first reclaim, checkout release, bounded-backoff retry within
		// spec.retryPolicy).
		resumeStore, err := coord.NewProdResumeStore(db, coord.DefaultProdResumeConfig(), nil)
		if err != nil {
			ctrl.Log.Error(err, "unable to bind resume store")
			os.Exit(1)
		}
		// Real kube provisioner for actual pod creation (enables cluster-testable agent execution)
		kubeProvisioner := kubepool.NewKubeProvisioner(mgr.GetClient(), "1", "512Mi")
		pool := kubepool.NewPool(kubeProvisioner) // real kube provisioner enables actual agent work

		// Epic D follow-up (ISI-3352): the physical A2A dispatch feed — the
		// production caller the ISI-3348 review demanded. The mapper (Epic D,
		// plan §2.4) registers the ksquad_* instruments on the
		// controller-runtime metrics registry (the operator's /metrics
		// endpoint); the dispatcher is what FEEDS it: TaskBuilder (coord
		// schema + Run CR → wire.Task) → StdioTransport (`shim run`
		// subprocess — operator-spawned, the §10.1 v1 topology) → per-Run
		// TelemetrySink mapping tool/skill events onto the mapper. With the
		// shim binary absent the constructor errors and the drive loop keeps
		// its ledger-only dispatcher — an honest, loudly-logged degraded
		// state, never a silently broken dispatch.
		mapper := toolusage.NewMapper(telemetry.Tracer(), metrics.Registry)
		var a2aErr error
		a2aDispatcher, a2aErr = rundrive.NewOperatorDispatcher(rundrive.OperatorDispatchConfig{
			DB:     db,
			Client: mgr.GetClient(),
			Mapper: mapper,
			ShimBin: func() string {
				if v := os.Getenv("KSQUAD_SHIM_BIN"); v != "" {
					return v
				}
				return "shim" // the operator image ships it at /usr/local/bin/shim
			}(),
			RuntimeType: os.Getenv("KSQUAD_RUNTIME_TYPE"),
			Stderr:      shimLogWriter{},
		})
		if a2aErr != nil {
			ctrl.Log.Error(a2aErr, "A2A dispatch unavailable: Run drive loop stays ledger-only (operator ksquad_* series will stay empty)")
		}

		driver := rundrive.NewDriver(mgr.GetClient(),
			rundrive.NewProdClaims(db, rundrive.OperatorPrincipal),
			rundrive.NewProdPauses(resumeStore),
			rundrive.NewProdRunner(db, rundrive.OperatorPrincipal,
				kubepool.NewBinder(pool, rundrive.SpecClassifier(mgr.GetClient())), a2aDispatcher))
		driver.Sandbox = pool // dead-run sandbox teardown on the retry path (§9.3)
		timer := coord.NewProdTimer(resumeStore, driver.OnResumeDue)
		driver.Notify = timer.Notify
		if err := driver.SetupWithManager(mgr); err != nil {
			ctrl.Log.Error(err, "unable to set up Run drive loop")
			os.Exit(1)
		}
		if err := mgr.Add(timerRunnable{t: timer}); err != nil {
			ctrl.Log.Error(err, "unable to register resume timer")
			os.Exit(1)
		}

		// The repo-sync reconciler (story 11.1, §5.4) mirrors a Project's
		// upstream into the untrusted-external scm schema on the SAME
		// Postgres (ADR-001 — one more schema, not a new datastore). Its
		// triggers are the webhook-ingress annotation bump (cmd/scm-webhook,
		// HMAC-verified before parse) and the spec's poll-interval requeue;
		// every pass is the same idempotent provider-snapshot upsert.
		//
		// The story-11.2 issue⇄work-item engine rides the SAME pass: for
		// every scm.issue_link of the Project it drives status/labels
		// across the provider seam (LWW, audited conflicts) — one loop,
		// two triggers, no third path (ISI-2738).
		issueLinkStore, err := issuesync.NewSQLStore(db)
		if err != nil {
			ctrl.Log.Error(err, "unable to bind issue-link store")
			os.Exit(1)
		}
		if err := (&reposync.Reconciler{
			Store:     scm.NewSQLMirrorStore(db),
			Providers: scm.NewProviderRegistry(),
			IssueSync: issuesync.NewSyncer(issueLinkStore),
		}).SetupWithManager(mgr); err != nil {
			ctrl.Log.Error(err, "unable to set up repo-sync reconciler")
			os.Exit(1)
		}

		// The 3.3 kill sweep (ISI-2884): a kill issued while the Run was
		// healthy has no death-detection requeue pending, so this bounded
		// sweep kicks cancelling Runs back into the drive loop, which tears
		// the sandbox down and finishes → cancelled. Latency sugar for the
		// kick; correctness is level-triggered off the durable step.
		if err := mgr.Add(&rundrive.CancelSweeper{
			Claims: rundrive.NewProdClaims(db, rundrive.OperatorPrincipal),
			OnDue:  driver.OnCancelDue,
			Log:    func(f string, a ...any) { ctrl.Log.Info(fmt.Sprintf(f, a...)) },
		}); err != nil {
			ctrl.Log.Error(err, "unable to register cancel sweep")
			os.Exit(1)
		}
	}

	// The Team reconciler provisions the squad tenancy scaffold (story 4.1,
	// arch §12.1: a squad IS a namespace) — namespace, least-privilege
	// SA/Role/RoleBinding, ResourceQuota, LimitRange, and the default-deny +
	// allow-DNS + allow-control-plane NetworkPolicy baseline — and tears the
	// namespace down finalizer-driven on Team delete. Unlike the Run
	// projector it needs no coordination DB, so it registers unconditionally.
	if err := (&teamctrl.Reconciler{}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up Team reconciler")
		os.Exit(1)
	}

	// The credential controller (story 7.7, arch §5.2/§11.1, ADR-032) is the
	// zero-touch Claude OAuth lifecycle: it watches per-user HUMAN-SEAT OAuth
	// Secrets and refreshes the ~8h access token in place before it expires, so
	// the many agent pods that MOUNT that Secret share one login and never
	// handle token strings. Registering it on the manager (whose leader
	// election is enabled above) makes it the SINGLE leader-elected refresher —
	// no per-pod refresh, no thundering-refresh race. Its Watch predicate
	// selects only `ksquad.io/credential-class: human-seat`, so a
	// service-account credential is never given the OAuth lifecycle (ADR-041).
	if err := (&credentialctrl.Reconciler{
		Refresher: credentialctrl.NewDefaultAnthropicRefresher(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up credential reconciler")
		os.Exit(1)
	}

	// The MCPServer discovery controller (story A3 / ADR-042): control-plane
	// tool discovery. streamable-http servers are probed directly
	// (initialize → tools/list, credentials in-memory only); stdio servers
	// via a short-lived probe Job in the server's namespace writing a
	// well-known ConfigMap. status.observedTools + the Ready/Credentials/
	// Egress/ToolsDiscovered conditions are what Run assembly's fail-closed
	// dangling-tool checks consume. Needs no coordination DB.
	if err := (&mcpserverctrl.Reconciler{}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up MCPServer discovery reconciler")
		os.Exit(1)
	}

	// Epic D (ISI-3288, plan §2.4): the tool-usage instrumentation spine.
	// The mapper itself is constructed at the Run drive loop (above) — the
	// dispatch feed and the instruments register in one place, so the
	// ksquad_tool_calls_total / ksquad_skill_loads_total /
	// ksquad_mcp_call_duration_seconds series are scrapeable on the
	// controller-runtime registry (the operator's /metrics endpoint) AND fed
	// by live dispatches. The otelgate reconciler watches OTelConfig and
	// applies spec.toolUsage onto the process-wide gate: flipping the CRD
	// field stops and resumes tool-usage spans + metrics mid-process, no
	// restart (story D2).
	if err := (&otelgate.Reconciler{}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up otelgate reconciler")
		os.Exit(1)
	}

	// Initialize workspace manager for PVC-based agent workspaces (ISI-2880)
	workspaceManager := workspacepkg.NewWorkspaceManager(mgr.GetClient())

	// Initialize network policy manager for team isolation (ISI-2884)
	networkPolicyManager := networkpkg.NewNetworkPolicyManager(mgr.GetClient())

	// Register custom controllers for workspace and network management.
	// Workspaces are per-Run (the manager keys off Run and owns the PVC);
	// network policies are per-Team.
	// A dedicated controller name is required: controller-runtime derives the
	// name from the primary Kind (lowercased) unless overridden, so a bare
	// For(&Run{}) here collides with the Run drive-loop controller ("run") and
	// For(&Team{}) collides with the Team reconciler ("team"), tripping the
	// "controller with name X already exists" uniqueness check at manager start
	// (surfaced deploying ISI-3488/ISI-3490). Name them for their concern.
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("run-workspace").
		For(&ksquadv1alpha1.Run{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(workspaceManager); err != nil {
		ctrl.Log.Error(err, "unable to set up workspace manager")
		os.Exit(1)
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("team-networkpolicy").
		For(&ksquadv1alpha1.Team{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(networkPolicyManager); err != nil {
		ctrl.Log.Error(err, "unable to set up network policy manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting ksquad-operator", "leaderElection", enableLeaderElection, "controllers", []string{"team", "run", "run-drive", "reposync", "credential", "mcpserver-discovery", "workspace", "networkpolicy"})
	if err := mgr.Start(ctx); err != nil {
		ctrl.Log.Error(err, "manager exited with error")
		os.Exit(1)
	}
	// Drain in-flight A2A follows so a Run's SSE stream is fully flushed to
	// the run-event sink + telemetry mapper before the process exits (the
	// Dispatcher's graceful-shutdown barrier, ISI-3352).
	if a2aDispatcher != nil {
		a2aDispatcher.Wait()
	}
}

// shimLogWriter routes the shim subprocess's diagnostic stream (the
// StdioTransport's Stderr — NEVER the SSE channel) into the operator's
// structured log.
type shimLogWriter struct{}

func (shimLogWriter) Write(p []byte) (int, error) {
	ctrl.Log.Info(strings.TrimRight(string(p), "\n"), "src", "shim")
	return len(p), nil
}

// timerRunnable adapts the 3.7 resume timer to the manager's Runnable surface:
// the wake loop runs (and exits) under the manager context, leader-gated with
// every other controller.
type timerRunnable struct{ t *coord.ProdTimer }

// Start implements manager.Runnable.
func (r timerRunnable) Start(ctx context.Context) error { return r.t.Run(ctx) }
