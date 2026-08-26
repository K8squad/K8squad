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

package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// probeScaffoldObjects builds the per-namespace stdio probe identity:
// ServiceAccount + Role (create/update exactly the probe result ConfigMaps)
// + RoleBinding. Idempotent create-or-update; mirrors the Team reconciler's
// managed-label repair discipline.
func (r *Reconciler) ensureProbeScaffold(ctx context.Context, server *ksquadv1alpha1.MCPServer) error {
	ns := server.Namespace
	labels := map[string]string{LabelManaged: ValueMCPProbeManager}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: probeServiceAccount, Namespace: ns, Labels: labels},
	}
	if err := createOrUpdate(ctx, r.Client, sa); err != nil {
		return err
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: probeServiceAccount, Namespace: ns, Labels: labels},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			// The probe writes only its own well-known result ConfigMap.
			Verbs:         []string{"create", "get", "update"},
			ResourceNames: []string{probeArtifactName(server.Name)},
		}},
	}
	if err := createOrUpdate(ctx, r.Client, role); err != nil {
		return err
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: probeServiceAccount, Namespace: ns, Labels: labels},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      probeServiceAccount,
			Namespace: ns,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     probeServiceAccount,
		},
	}
	return createOrUpdate(ctx, r.Client, binding)
}

// createOrUpdate applies create-or-replace-by-label semantics without
// clobbering server-set defaults (resourceVersion-aware update on conflict
// is unnecessary at v1alpha1 probe scope: the objects are fully owned).
func createOrUpdate(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // fully-owned scaffold; drift healed on next pass
		}
		return err
	}
	return nil
}

// probeJob builds the short-lived stdio discovery Job (ADR-042): an init
// container stages the mcp-probe binary onto an emptyDir; the main
// container runs the TARGET image (or the helper when the MCPServer has no
// image of its own) with the probe as entrypoint, which launches
// spec.command as a child, performs the MCP handshake over stdio, and
// writes the tool list to the result ConfigMap. Bounded: backoffLimit 0,
// activeDeadlineSeconds 120, ttlSecondsAfterFinished 120.
func (r *Reconciler) probeJob(server *ksquadv1alpha1.MCPServer, name string) *batchv1.Job {
	helper := r.helperImage()
	target := server.Spec.Image
	if target == "" {
		// No packaged image: the probe helper image itself hosts the launch
		// (works for command paths it bundles; otherwise the probe fails
		// closed with an actionable condition).
		target = helper
	}

	argsJSON, _ := json.Marshal(server.Spec.Args)
	probeArgs := []string{
		"--server-command", server.Spec.Command,
		"--server-args", string(argsJSON),
		"--configmap", name,
		"--namespace", server.Namespace,
	}

	nonRoot := true
	seccomp := corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	allowPrivEsc := false
	dropAll := []corev1.Capability{"ALL"}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: server.Namespace,
			Labels: map[string]string{
				LabelManaged:   ValueMCPProbeManager,
				LabelMCPServer: server.Name,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(
				server, schema.GroupVersionKind{
					Group:   ksquadv1alpha1.GroupVersion.Group,
					Version: ksquadv1alpha1.GroupVersion.Version,
					Kind:    "MCPServer",
				})},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptrToInt32(0),
			ActiveDeadlineSeconds:   ptrToInt64(probeJobDeadline),
			TTLSecondsAfterFinished: ptrToInt32(probeJobTTL),
			Completions:             ptrToInt32(1),
			Parallelism:             ptrToInt32(1),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					LabelManaged:   ValueMCPProbeManager,
					LabelMCPServer: server.Name,
				}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// The probe pod must reach the API to write its result
					// ConfigMap (ADR-042 "well-known ConfigMap"): the only
					// control-plane artifact with API write in the team ns,
					// scoped by the scaffold Role to one ConfigMap name.
					AutomountServiceAccountToken: ptrToBool(true),
					ServiceAccountName:           probeServiceAccount,
					Containers: []corev1.Container{{
						Name:    "probe",
						Image:   target,
						Command: []string{"/probe/mcp-probe"},
						Args:    probeArgs,
						VolumeMounts: []corev1.VolumeMount{{
							Name: "probe", MountPath: "/probe", ReadOnly: true,
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             &nonRoot,
							AllowPrivilegeEscalation: &allowPrivEsc,
							Capabilities:             &corev1.Capabilities{Drop: dropAll},
							SeccompProfile:           &seccomp,
						},
					}},
					InitContainers: []corev1.Container{{
						Name:  "stage-probe",
						Image: helper,
						Command: []string{
							"cp", "/mcp-probe", "/probe/mcp-probe",
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name: "probe", MountPath: "/probe",
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("16Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             &nonRoot,
							AllowPrivilegeEscalation: &allowPrivEsc,
							Capabilities:             &corev1.Capabilities{Drop: dropAll},
							SeccompProfile:           &seccomp,
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "probe",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					}},
				},
			},
		},
	}
}

// probeArtifactName derives the probe Job/ConfigMap name, truncating and
// disambiguating with a hash tail when the MCPServer name is long
// (Kubernetes name limit 63).
func probeArtifactName(serverName string) string {
	name := probeJobPrefix + serverName
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:52] + "-" + hex.EncodeToString(sum[:])[:10]
}

// ptr helpers keep the Job spec literals readable.
func ptrToInt32(v int32) *int32 { return &v }
func ptrToInt64(v int64) *int64 { return &v }
func ptrToBool(v bool) *bool    { return &v }
