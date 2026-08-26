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
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	credentialctrl "github.com/K8squad/K8squad/pkg/controller/credential"
	reposync "github.com/K8squad/K8squad/pkg/controller/reposync"
	runctrl "github.com/K8squad/K8squad/pkg/controller/run"
	rundrive "github.com/K8squad/K8squad/pkg/controller/rundrive"
	teamctrl "github.com/K8squad/K8squad/pkg/controller/team"
	"github.com/K8squad/K8squad/pkg/coord"
	networkpkg "github.com/K8squad/K8squad/pkg/networkpolicy"
	"github.com/K8squad/K8squad/pkg/issuesync"
	"github.com/K8squad/K8squad/pkg/scm"
	"github.com/K8squad/K8squad/pkg/telemetry"
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
		if err := (&runctrl.Reconciler{Source: coord.NewReconcileStepReader(db)}).SetupWithManager(mgr); err != nil {
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
		driver := rundrive.NewDriver(mgr.GetClient(),
			rundrive.NewProdClaims(db, rundrive.OperatorPrincipal),
			rundrive.NewProdPauses(resumeStore),
			rundrive.NewProdRunner(db, rundrive.OperatorPrincipal,
				kubepool.NewBinder(pool, rundrive.SpecClassifier(mgr.GetClient())), nil))
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

	// Initialize workspace manager for PVC-based agent workspaces (ISI-2880)
	workspaceManager := workspacepkg.NewWorkspaceManager(mgr.GetClient())

	// Initialize network policy manager for team isolation (ISI-2884)
	networkPolicyManager := networkpkg.NewNetworkPolicyManager(mgr.GetClient())

	// Register custom controllers for workspace and network management.
	// Workspaces are per-Run (the manager keys off Run and owns the PVC);
	// network policies are per-Team.
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&ksquadv1alpha1.Run{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(workspaceManager); err != nil {
		ctrl.Log.Error(err, "unable to set up workspace manager")
		os.Exit(1)
	}

	if err := ctrl.NewControllerManagedBy(mgr).
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

	ctrl.Log.Info("starting ksquad-operator", "leaderElection", enableLeaderElection, "controllers", []string{"team", "run", "run-drive", "reposync", "credential", "workspace", "networkpolicy"})
	if err := mgr.Start(ctx); err != nil {
		ctrl.Log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// timerRunnable adapts the 3.7 resume timer to the manager's Runnable surface:
// the wake loop runs (and exits) under the manager context, leader-gated with
// every other controller.
type timerRunnable struct{ t *coord.ProdTimer }

// Start implements manager.Runnable.
func (r timerRunnable) Start(ctx context.Context) error { return r.t.Run(ctx) }
