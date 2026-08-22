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

// kube.go — the Story 3.4 kube Provisioner adapter: creates/destroys sandbox pods
// with the specified RuntimeClass and AgentRuntime image. This is the missing
// piece that makes the warm-pool system actually functional for cluster testing.
//
// This file lives in package warmpool (not a separate "kube" package): the
// operator consumes it as kubepool.NewKubeProvisioner (an alias for this
// package), so keeping it in warmpool is what makes the operator build.
package warmpool

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubeProvisioner implements the Provisioner interface using Kubernetes
// client-go to create and delete sandbox pods. It's the production adapter that
// makes the warm-pool system actually functional.
type KubeProvisioner struct {
	client client.Client
	// Default resource limits for sandbox pods
	cpuLimit    string
	memoryLimit string
}

// NewKubeProvisioner creates a new kube Provisioner with the given Kubernetes client
// and optional resource limits. If limits are empty, reasonable defaults are used.
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

// Boot creates a new sandbox pod with the specified pool key and sandbox ID.
// The pod will have the specified RuntimeClass and AgentRuntime image.
func (k *KubeProvisioner) Boot(ctx context.Context, key PoolKey, sandboxID string) error {
	// Create the sandbox pod spec
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sandboxID,
			Namespace:   "default", // Should be configurable based on Team
			Labels: map[string]string{
				"app":     "k8squad-sandbox",
				"sandbox": sandboxID,
				"pool":    key.RuntimeClass,
			},
			Annotations: map[string]string{
				"k8squad.io/sandbox-id": sandboxID,
				"k8squad.io/pool-key":  fmt.Sprintf("%s/%s", key.RuntimeClass, key.Image),
			},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: &key.RuntimeClass,
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: key.Image,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							"cpu":    resource.MustParse(k.cpuLimit),
							"memory": resource.MustParse(k.memoryLimit),
						},
						// Requests match limits for guaranteed QoS
						Requests: corev1.ResourceList{
							"cpu":    resource.MustParse(k.cpuLimit),
							"memory": resource.MustParse(k.memoryLimit),
						},
					},
					// Basic liveness probe for sandbox readiness
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/health",
								Port:   intstr.FromInt(8080),
								Scheme: "HTTP",
							},
						},
						InitialDelaySeconds: 30,
						TimeoutSeconds:      5,
						PeriodSeconds:       10,
						SuccessThreshold:    1,
						FailureThreshold:    3,
					},
					// Readiness probe for pool claiming
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/ready",
								Port:   intstr.FromInt(8080),
								Scheme: "HTTP",
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
			// Security context for sandbox isolation
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  ptrTo[int64](1000), // Non-root user
				RunAsGroup: ptrTo[int64](1000),
			},
			// Termination grace period for clean shutdown
			TerminationGracePeriodSeconds: ptrTo[int64](30),
		},
	}

	// Create the sandbox pod
	if err := k.client.Create(ctx, pod); err != nil {
		return fmt.Errorf("kubeProvisioner.Boot: failed to create sandbox pod %s: %w", sandboxID, err)
	}

	return nil
}

// TearDown deletes the sandbox pod with the given sandbox ID.
// This implements the teardown-and-replace semantics: the pod is destroyed
// and never reused across Runs.
func (k *KubeProvisioner) TearDown(ctx context.Context, sandboxID string) error {
	// Create a pod object for deletion
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxID,
			Namespace: "default", // Should match the namespace used in Boot
		},
	}

	// Delete the sandbox pod
	// Use foreground deletion for graceful termination
	deletePolicy := metav1.DeletePropagationForeground
	if err := k.client.Delete(ctx, pod, client.PropagationPolicy(deletePolicy)); err != nil {
		return fmt.Errorf("kubeProvisioner.TearDown: failed to delete sandbox pod %s: %w", sandboxID, err)
	}

	return nil
}

// ptrTo returns a pointer to the given value. This is a helper for
// creating pointer values for pod specification fields.
func ptrTo[T any](v T) *T {
	return &v
}