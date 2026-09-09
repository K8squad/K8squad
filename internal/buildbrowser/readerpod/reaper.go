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
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultMaxLifetime is the wall-clock age past which the orphan sweep reaps a reader pod even if the
// apiserver never asked for a teardown. It is deliberately just UNDER the pod's 900s ActiveDeadline
// backstop (readerpod BuildPod), so a leaked reader is reclaimed by the control plane first and the
// kubelet deadline is only the last-ditch fallback. AC6: reap idle readers, keep ActiveDeadline as
// the backstop — never the primary mechanism.
const DefaultMaxLifetime = 12 * time.Minute

// Reaper is the AC6 idle-teardown controller. It is the ONE metering teardown path: both the
// apiserver's in-memory idle manager (ReapHandle, called when a browse session goes idle) and the
// periodic cluster-side orphan sweep (ReapIdle, called on a timer) funnel through it, so every reader
// pod removed before its ActiveDeadline increments the reap counter exactly once.
//
// It carries NO pods/exec and adds NO write verb beyond the launcher's existing pod+service
// create/delete — it lists reader pods (label-scoped) and delegates deletion to the Launcher.
type Reaper struct {
	client    client.Client
	launcher  Launcher
	namespace string
	maxLife   time.Duration
	now       func() time.Time // injectable clock for tests
}

// NewReaper builds a Reaper over the same client + launcher the KubeLauncher uses. A zero namespace
// defaults to DefaultNamespace and a zero maxLife to DefaultMaxLifetime, so the caller can construct
// it with just the collaborators.
func NewReaper(c client.Client, launcher Launcher, namespace string, maxLife time.Duration) *Reaper {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if maxLife <= 0 {
		maxLife = DefaultMaxLifetime
	}
	return &Reaper{client: c, launcher: launcher, namespace: namespace, maxLife: maxLife, now: time.Now}
}

// ReapHandle tears down a single reader (pod + paired Service) and records one reap on success. It is
// the seam the apiserver idle manager calls when a browse session has been idle past its threshold —
// the idle decision lives in the manager (which knows last-access wall time); the metering + teardown
// live here so there is exactly one reap path. A teardown error is returned and NOT counted.
func (r *Reaper) ReapHandle(ctx context.Context, h Handle) error {
	if h.PodName == "" && h.ServiceName == "" {
		return nil
	}
	if err := r.launcher.TearDown(ctx, h); err != nil {
		return err
	}
	recordReap()
	return nil
}

// ReapIdle is the periodic cluster-side orphan sweep: it lists every reader pod in the namespace and
// reaps those older than maxLife. This reclaims readers the in-memory idle manager lost track of
// (e.g. an apiserver restart) WITHOUT needing a pod-patch "last-access" annotation — age is read
// straight off CreationTimestamp. It returns the number of readers reaped so a caller can log a sweep
// summary. A per-pod teardown error is skipped (best-effort sweep) so one stuck pod cannot stall the
// rest; the next sweep retries it.
func (r *Reaper) ReapIdle(ctx context.Context) (int, error) {
	var pods corev1.PodList
	if err := r.client.List(ctx, &pods,
		client.InNamespace(r.namespace),
		client.MatchingLabels{"app": readerAppLabel},
	); err != nil {
		return 0, err
	}
	cutoff := r.now().Add(-r.maxLife)
	reaped := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.CreationTimestamp.Time.After(cutoff) {
			continue // still within its lifetime window — leave it to the idle manager
		}
		runID := p.Labels["k8squad.io/run"]
		h := Handle{
			PodName:     p.Name,
			ServiceName: ServiceName(runID),
			Namespace:   p.Namespace,
		}
		if err := r.ReapHandle(ctx, h); err != nil {
			continue // best-effort: skip a stuck reader, retry next sweep
		}
		reaped++
	}
	return reaped, nil
}
