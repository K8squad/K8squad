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

package sandbox

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// PodSpec is the desired-shape input to sandbox pod assembly (story 4.2 §4:
// the reconciler emits the pod under the resolved class, in the Team
// namespace, as the ksquad-agent SA).
type PodSpec struct {
	// Namespace is the Team's squad namespace (story 4.1 status.namespace).
	Namespace string
	// TeamName labels the pod back to its Team.
	TeamName string
	// RuntimeClass is the ADMITTED, existence-verified class from
	// SelectRuntimeClass — never empty, never silently substituted.
	RuntimeClass string
	// Image is the AgentRuntime image the pod runs.
	Image string
	// Command/Args are the container entrypoint (shim launch).
	Command []string
	Args    []string
	// Mounts are the per-principal workspace mounts (story 4.5). May be nil
	// for a warm pod (mounts attach at claim/bind time, not warm time).
	Mounts []corev1.VolumeMount
	// Volumes are the PVC volumes backing the mounts.
	Volumes []corev1.Volume
}

// BuildSandboxPod assembles the per-Run sandbox pod (story 4.2 AC1/AC3):
// non-empty runtimeClassName, in the squad namespace, running as the
// namespaced ksquad-agent ServiceAccount, one pod per Run. It fail-closes
// on an empty runtime class — a pod without runtimeClassName runs on the
// node default runtime (runc), which is the exact hole §9.1 closes.
func BuildSandboxPod(run *api.Run, spec PodSpec) (*corev1.Pod, error) {
	if spec.RuntimeClass == "" {
		return nil, &PolicyError{Reason: "sandbox pod assembly requires an admitted RuntimeClass; refusing to emit a pod on the node default runtime"}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PodNameFor(run),
			Namespace: spec.Namespace,
			Labels: map[string]string{
				LabelRun:   string(run.UID),
				LabelSquad: spec.TeamName,
			},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: &spec.RuntimeClass,
			ServiceAccountName: agentSA,
			Containers: []corev1.Container{{
				Name:         "agent",
				Image:        spec.Image,
				Command:      spec.Command,
				Args:         spec.Args,
				VolumeMounts: spec.Mounts,
			}},
			Volumes: spec.Volumes,
		},
	}
	return pod, nil
}

// agentSA mirrors pkg/controller/team.AgentServiceAccount without an import
// cycle. The reconciler-side constant stays authoritative; this one exists so
// the sandbox package can be consumed standalone by the run reconcile path.
const agentSA = "ksquad-agent"

// Teardown destroys the Run's sandbox pod (story 4.5 AC1: destroyed, not
// reset — there is NO in-place scrub-and-reuse path). Crash-safe: deleting an
// already-absent pod is a no-op (AC6) — a controller that crashed between
// delete and replenish converges on re-reconcile without orphaning anything.
func Teardown(ctx context.Context, c client.Client, run *api.Run) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: PodNameFor(run), Namespace: runNamespace(run)}}
	if err := c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("teardown sandbox pod: %w", err)
	}
	return nil
}

// runNamespace resolves the pod's namespace: the Run object's own namespace
// is the squad namespace (a Run lives inside its Team's tenancy).
func runNamespace(run *api.Run) string { return run.Namespace }

// BindingGuard enforces the §9.3 absolute (story 4.5 AC2): a sandbox pod is
// bound to exactly one Run over its lifetime. Handing a pod whose run-label
// names a DIFFERENT Run to this Run is a construction failure — fail closed.
func BindingGuard(pod *corev1.Pod, run *api.Run) error {
	if bound := pod.Labels[LabelRun]; bound != "" && bound != string(run.UID) {
		return &PolicyError{Reason: fmt.Sprintf("sandbox pod %s/%s is bound to run uid %s; refusing to rebind to %s (a sandbox is never reused across Runs or principals)", pod.Namespace, pod.Name, bound, run.UID)}
	}
	return nil
}

// Replenish mints a FRESH warm pod to restore the pool count after a
// teardown (story 4.5 AC1: replenishment is async and always a new pod —
// "warm" is a property of the pool, not of a reused pod). The name is
// collision-safe via a fresh hash input, so a replenished pod can never
// resurrect a torn-down pod's identity.
func Replenish(ctx context.Context, c client.Client, spec PodSpec) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: SandboxPodPrefix + "warm-",
			Namespace:    spec.Namespace,
			Labels: map[string]string{
				LabelSquad: spec.TeamName,
				// No LabelRun: an unbound warm pod. The claim path (story
				// 3.4) stamps LabelRun at bind time after BindingGuard.
			},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName:   &spec.RuntimeClass,
			ServiceAccountName: agentSA,
			Containers: []corev1.Container{{
				Name:    "agent",
				Image:   spec.Image,
				Command: spec.Command,
				Args:    spec.Args,
			}},
			Volumes: spec.Volumes,
		},
	}
	if spec.RuntimeClass == "" {
		return &PolicyError{Reason: "replenishment requires an admitted RuntimeClass; refusing to mint a pod on the node default runtime"}
	}
	if err := c.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("replenish warm pod: %w", err)
	}
	return nil
}
