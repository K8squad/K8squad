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

// ksquad-operator is the controller-manager for the Run reconcile machine (arch
// §5.2, Story 3.1 production wiring, ISI-2655). Unlike the webhook binary — which
// is deliberately stateless and unelected — the operator hosts the reconcile
// controllers under LEADER ELECTION (§5.2 AC4): exactly one replica is active,
// so two pods never drive the same Run's durable step or race its status
// subresource on failover.
//
// This slice wires the read-only status-projection controller (durable
// reconcile_step → Run.status). The step-advancing Effects controller
// (warm-pool bind / A2A dispatch / artifact upsert) lands in the follow-up
// Effects slice; it plugs into this same manager.
package main

import (
	"database/sql"
	"flag"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the coord source

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	runctrl "github.com/K8squad/K8squad/internal/controller/run"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ksquadv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr, leaderElectionID string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"Address the metrics endpoint binds to. \"0\" disables metrics.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"Address the health/readiness probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election so exactly one replica reconciles (§5.2 AC4).")
	flag.StringVar(&leaderElectionID, "leader-election-id", "ksquad-operator.ksquad.io",
		"Holder identity name for the leader-election lease.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	// The coordination Postgres is the Run's durable source of truth (§6.4).
	// DATABASE_URL is the conventional deployment knob (matches ksquad-memory).
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		setupLog.Error(nil, "DATABASE_URL is required (coordination Postgres DSN)")
		os.Exit(1)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		setupLog.Error(err, "unable to open coordination Postgres")
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&runctrl.StatusReconciler{
		Client: mgr.GetClient(),
		DB:     db,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up Run status-projection controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting ksquad-operator",
		"leaderElection", enableLeaderElection, "controllers", []string{"run-status-projection"})
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
