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

// kube.go — the Story 3.4 kube Provisioner adapter: creates/destroys sandbox
// pods with the pool key's RuntimeClass and AgentRuntime image. This is the
// production drop-in for the Provisioner seam (pool.go) that makes the
// warm-pool system actually create real cluster pods for cluster testing.
package warmpool

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/pkg/taskio"
	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// sandboxNamespace is the namespace sandbox pods are created in. It is a
// package default until Team-scoped namespacing lands (Epic 4).
const sandboxNamespace = "default"

// KubeProvisioner implements the Provisioner interface using a
// controller-runtime client to create and delete sandbox pods. It is the
// production adapter that makes the warm-pool system boot real pods.
type KubeProvisioner struct {
	client client.Client
	// Default resource limits for sandbox pods.
	cpuLimit    string
	memoryLimit string
}

// NewKubeProvisioner creates a new kube Provisioner with the given
// controller-runtime client and optional resource limits. If a limit is
// empty, a reasonable default is used.
func NewKubeProvisioner(kubeClient client.Client, cpuLimit, memoryLimit string) *KubeProvisioner {
	if cpuLimit == "" {
		cpuLimit = "1"
	}
	if memoryLimit == "" {
		memoryLimit = "512Mi"
	}

	return &KubeProvisioner{
		client:      kubeClient,
		cpuLimit:    cpuLimit,
		memoryLimit: memoryLimit,
	}
}

// Boot creates a fresh sandbox pod for key under the pool-assigned sandboxID.
// The pod carries the key's RuntimeClass and AgentRuntime image. It returns
// WITHOUT waiting for readiness — readiness is reported to the pool via the
// pod watch (Provisioner contract, pool.go).
//
// The pod boots in the key's Namespace when set (ADR-044 step 9: sandbox
// tenancy — per-Run RBAC, NetworkPolicy and quota are namespace-scoped,
// §12.1); the sandboxNamespace default remains only for callers that have
// not migrated to classified keys.
func (k *KubeProvisioner) Boot(ctx context.Context, key PoolKey, sandboxID string) error {
	runtimeClass := key.RuntimeClass
	namespace := key.Namespace
	if namespace == "" {
		namespace = sandboxNamespace
	}
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(k.cpuLimit),
		corev1.ResourceMemory: resource.MustParse(k.memoryLimit),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxID,
			Namespace: namespace,
			Labels: map[string]string{
				"app":     "k8squad-sandbox",
				"sandbox": sandboxID,
				"pool":    key.RuntimeClass,
			},
			Annotations: map[string]string{
				"k8squad.io/sandbox-id": sandboxID,
				"k8squad.io/pool-key":   fmt.Sprintf("%s/%s", key.RuntimeClass, key.Image),
			},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName:              &runtimeClass,
			TerminationGracePeriodSeconds: ptrTo[int64](30),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  ptrTo[int64](1000),
				RunAsGroup: ptrTo[int64](1000),
			},
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: key.Image,
					// Epic D (ISI-3288, plan §2.4): the tool-usage gate as of
					// pod boot. The operator's otelgate reconciler keeps
					// toolusage.Enabled() synced with OTelConfig.spec.toolUsage;
					// stamping it into the sandbox env carries the toggle to
					// the shim process (cmd/shim reads it, default-on when
					// absent — plan §5.4 opt-out).
					//
					// Epic D / D1 (ISI-3348 finding 3): the W3C trace
					// carrier. telemetry.Inject writes the current span's
					// traceparent/tracestate into the carrier when Boot rides
					// a traced context (the run-drive pass opens run.reconcile
					// per pass), and those env vars join the shim's spans onto
					// the Run's distributed trace (cmd/shim Extracts them at
					// startup). A carrier-less context (pool warm-boot, no
					// live Run) stamps nothing — the next span roots a fresh
					// trace, honestly.
					Env: sandboxEnv(ctx, toolusage.Enabled()),
					// Topology 2 (ADR-0007 channel A): mount the per-sandbox
					// task-io Secret at the coord path. The mount is OPTIONAL
					// (see the Volume below) because the Secret does not exist
					// at Boot — it is written by the operator at Bind, once
					// RUN_ID/WORK_ITEM_ID exist to mint against. The supervisor
					// reads the run-scoped credential from files here (env→path
					// contract); no operator secret rides the container env, so
					// the minimal-env invariant holds.
					VolumeMounts: []corev1.VolumeMount{{
						Name:      taskio.CoordVolumeName,
						MountPath: taskio.CoordMountPath,
						ReadOnly:  true,
					}},
					Resources: corev1.ResourceRequirements{
						// Requests match limits for guaranteed QoS.
						Limits:   limits,
						Requests: limits,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/health",
								Port:   intstr.FromInt(8080),
								Scheme: corev1.URISchemeHTTP,
							},
						},
						InitialDelaySeconds: 30,
						TimeoutSeconds:      5,
						PeriodSeconds:       10,
						SuccessThreshold:    1,
						FailureThreshold:    3,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/ready",
								Port:   intstr.FromInt(8080),
								Scheme: corev1.URISchemeHTTP,
							},
						},
						InitialDelaySeconds: 10,
						TimeoutSeconds:      5,
						PeriodSeconds:       5,
						SuccessThreshold:    1,
						FailureThreshold:    3,
					},
				},
			},
			// The projected Secret for the mount above. Optional so Boot never
			// blocks on a Secret that only exists after Bind (and a warm pod
			// that is never bound simply has no file — fail-safe: an absent
			// token makes the coord API refuse the call, never fail-open). The
			// Secret is named after the pod (== sandbox_ref) so the Bind-path
			// writer can address it from the run-id-only bind frame.
			Volumes: []corev1.Volume{{
				Name: taskio.CoordVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: sandboxID,
						Optional:   ptrTo(true),
					},
				},
			}},
		},
	}

	if err := k.client.Create(ctx, pod); err != nil {
		return fmt.Errorf("kubeProvisioner.Boot: failed to create sandbox pod %s: %w", sandboxID, err)
	}

	return nil
}

// sandboxEnv renders the sandbox container env: the Epic D tool-usage gate
// plus, when Boot rides a traced context, the W3C trace-context carrier the
// shim Extracts to continue the Run's distributed trace (D1). The carrier is
// stamped per pod boot, so every task the sandbox serves continues the trace
// the booting Run pass was in. Carrier keys are lowercase (W3C via
// propagation.MapCarrier); the env vars follow the TRACEPARENT convention.
func sandboxEnv(ctx context.Context, toolUsageEnabled bool) []corev1.EnvVar {
	env := []corev1.EnvVar{{
		Name:  "KSQUAD_TOOL_USAGE_ENABLED",
		Value: strconv.FormatBool(toolUsageEnabled),
	}}
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	for envName, carrierKey := range map[string]string{"TRACEPARENT": "traceparent", "TRACESTATE": "tracestate"} {
		if v := carrier[carrierKey]; v != "" {
			env = append(env, corev1.EnvVar{Name: envName, Value: v})
		}
	}
	return env
}

// TearDown deletes the sandbox pod with the given sandboxID (§9.3
// teardown-and-replace: the pod is the disposable unit; a sandbox is NEVER
// reused across Runs). Foreground deletion is used for graceful termination.
func (k *KubeProvisioner) TearDown(ctx context.Context, sandboxID string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxID,
			Namespace: sandboxNamespace,
		},
	}

	deletePolicy := metav1.DeletePropagationForeground
	if err := k.client.Delete(ctx, pod, client.PropagationPolicy(deletePolicy)); err != nil {
		return fmt.Errorf("kubeProvisioner.TearDown: failed to delete sandbox pod %s: %w", sandboxID, err)
	}

	return nil
}

// ptrTo returns a pointer to v — a helper for the optional pointer fields in
// the pod spec.
func ptrTo[T any](v T) *T {
	return &v
}
