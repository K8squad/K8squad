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

// ksquad-operator is the epic-2 controller-manager: it hosts the Run
// reconciler that projects the durable coord reconcile_step onto Run.status
// (ISI-2655, Story 3.1).
//
// Leader election is ON by default (arch §5.2, AC4 availability): exactly one
// replica reconciles at a time, so a rolling restart or node loss fails over
// without two managers racing to patch the same Run.status. The fenced coord
// store (pkg/coord) is the durable guard against a zombie writer, but leader
// election keeps the steady state single-writer and avoids needless patch churn.
//
// The Run reconciler's StepSource is left nil here until the coord Postgres pool
// wiring lands (its own slice, gated on the real-Postgres integration test): the
// binary compiles and elects, and run.StepSource is the seam that wiring fills in
// before the reconciler is registered.
package main

import (
	"database/sql"
	"flag"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the coord pool

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	runctrl "github.com/K8squad/K8squad/pkg/controller/run"
	teamctrl "github.com/K8squad/K8squad/pkg/controller/team"
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

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting ksquad-operator", "leaderElection", enableLeaderElection, "controllers", []string{"team", "run"})
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
