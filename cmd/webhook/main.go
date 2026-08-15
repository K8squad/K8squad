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

// ksquad-webhook serves the squad-composition admission webhooks:
//
//   - story 1.6 attribution (Team, Project, Agent, Run): createdBy stamped
//     and enforced immutable at admission, spec.ownedBy defaulted from the
//     authenticated principal;
//   - story 1.3 cross-object reference guards (Team→Project/Agent,
//     Agent→AgentRuntime/Role/Skill/Secret, Run→Team/Project/Agent),
//     chained after the attribution checks on the same validating paths.
//
// Same-object rules (FR-D3 runtime discipline, Skill source one-of,
// sandbox defaults) are compiled into the CRD schemas themselves and
// enforced by the API server without this binary.
//
// It is deliberately webhook-only: no controllers, no metrics, no leader
// election. The operator binary (epic 2) may host these same webhooks via
// webhook.SetupAttributionWebhookWithManager when the manager grows
// controllers.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	attrwebhook "github.com/K8squad/K8squad/internal/webhook"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ksquadv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		"Address the metrics endpoint binds to. \"0\" disables metrics (webhook-only binary).")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: metricsAddr},
		WebhookServer: webhook.NewServer(webhook.Options{
			// Port 9443 + /tmp/k8s-webhook-server/serving-certs are the
			// kubebuilder conventions the deployment (story 1.4 Helm)
			// mounts certs into.
			Port:    9443,
			CertDir: "/tmp/k8s-webhook-server/serving-certs",
		}),
		// Webhook-only: probes/metrics land with the operator binary.
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := attrwebhook.SetupAttributionWebhookWithManager(mgr,
		&ksquadv1alpha1.Team{}, &ksquadv1alpha1.Project{}, &ksquadv1alpha1.Agent{}, &ksquadv1alpha1.Run{}); err != nil {
		ctrl.Log.Error(err, "unable to set up webhooks")
		os.Exit(1)
	}

	ctrl.Log.Info("starting ksquad-webhook", "webhooks", []string{"teams", "projects", "agents", "runs"})
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "webhook server exited with error")
		os.Exit(1)
	}
}
