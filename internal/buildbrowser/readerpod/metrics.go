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

package readerpod

import (
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// readerPodLaunches is the 8.7f cost signal (§7): every RO-reader-pod launch is alert-worthy because
// a reader mounts a Project PVC and burns cluster resources. It is a SINGLE global counter with NO
// per-run / per-project label, so the observability obeys the NFR-OBS3 cardinality firewall — a
// launch storm shows as a rate() spike on one series, and the per-launch record stays on the pod
// (labels/annotations), never in the metric.
var (
	metricsRegisterOnce sync.Once

	readerPodLaunches = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ksquad_buildbrowser_reader_pod_launches_total",
		Help: "On-demand full-tree RO reader pods launched (8.7f cost signal, §7). Alert on rate() spikes.",
	})
)

// RegisterMetrics registers the reader-pod launch counter with reg exactly once. It is safe to call
// from multiple hosts/tests; a re-register is a no-op. Mirrors the event-seam metrics registration.
func RegisterMetrics(reg prometheus.Registerer) {
	metricsRegisterOnce.Do(func() {
		if reg != nil {
			reg.MustRegister(readerPodLaunches)
		}
	})
}

// recordLaunch increments the launch counter. It is always safe: the counter exists whether or not
// RegisterMetrics ran, so a KubeLauncher used without a registry still counts (the value is just not
// scraped). Called on every successful pod create.
func recordLaunch() { readerPodLaunches.Inc() }

// launchCount reads the current counter value — test-only introspection.
func launchCount() float64 { return testutil.ToFloat64(readerPodLaunches) }

// apierrIsNotFound reports whether err is a Kubernetes "not found" status error, so an idempotent
// teardown of an already-gone reader is treated as success.
func apierrIsNotFound(err error) bool { return apierrors.IsNotFound(err) }
