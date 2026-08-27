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

package capability

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolchain"
)

// PodAssembly is the capability plane's contribution to a sandbox pod
// (ADR-044 step 6): init containers (tool staging, then MCP sidecars),
// added volumes, and the env/mounts the AGENT container gains. The
// sandbox package's hardened shape stays authoritative — ApplyToPod only
// appends, with collision guards that fail closed.
type PodAssembly struct {
	// InitContainers are appended after any caller-supplied inits:
	// staging containers first (sequential, complete before anything
	// else), then MCP native sidecars (restartable init containers,
	// K8s ≥1.28).
	InitContainers []corev1.Container

	// Volumes are the added volumes (tool emptyDir, MCP config
	// projection).
	Volumes []corev1.Volume

	// AgentEnv is env added to the agent container: PATH wiring,
	// K8SQUAD_MCP_CONFIG, streamable-http credential projections.
	AgentEnv []corev1.EnvVar

	// AgentMounts are mounts added to the agent container (read-only).
	AgentMounts []corev1.VolumeMount
}

// AgentContainerName is the sandbox package's runtime container name
// (pkg/sandbox: the one container BuildSandboxPod emits is "agent").
const AgentContainerName = "agent"

// AssemblePod computes the capability contribution for a sandbox pod from
// the resolved envelope (ADR-044 step 6). Pure: no cluster access — the
// caller supplies the resolver's output and the resolved endpoints.
func AssemblePod(run *api.Run, resolved []toolchain.Resolved, endpoints []Endpoint) (*PodAssembly, error) {
	asm := &PodAssembly{}

	// Init containers: staging first (tools exist before the runtime
	// starts), then MCP sidecars (native sidecars start after plain init
	// containers complete — the config they serve is already projected,
	// so there is no race for the runtime's config render at start).
	asm.InitContainers = append(asm.InitContainers, RenderInitContainers(resolved)...)
	for _, ep := range endpoints {
		if ep.Transport == string(api.MCPTransportStdio) && ep.Image != "" {
			asm.InitContainers = append(asm.InitContainers, renderMCPSidecar(ep, ep.CredentialSecretRef))
		}
	}

	if len(resolved) > 0 {
		asm.Volumes = append(asm.Volumes, ToolVolume())
		asm.AgentMounts = append(asm.AgentMounts, ToolVolumeMounts()...)
		asm.AgentEnv = append(asm.AgentEnv, ToolPathEnv())
	}

	if len(endpoints) > 0 {
		vol, mount := MCPConfigVolume(run)
		asm.Volumes = append(asm.Volumes, vol)
		asm.AgentMounts = append(asm.AgentMounts, mount)
		asm.AgentEnv = append(asm.AgentEnv, corev1.EnvVar{
			Name:  MCPConfigEnvVar,
			Value: MCPConfigMountPath + "/" + MCPConfigFile,
		})
		for _, ep := range endpoints {
			// stdio-with-sidecar credentials ride the sidecar; a stdio
			// server whose command lives in the runtime image and every
			// streamable-http server read their credential from the
			// agent container's env.
			if ep.CredentialSecretRef == nil {
				continue
			}
			if ep.Transport == string(api.MCPTransportStdio) && ep.Image != "" {
				continue
			}
			asm.AgentEnv = append(asm.AgentEnv, credentialEnvFor(ep, ep.CredentialSecretRef)...)
		}
	}
	return asm, nil
}

// ApplyToPod applies an assembly onto a built sandbox pod, fail-closed on
// collisions (the sandbox package's duplicate-volume guard discipline):
// an assembly element that would shadow caller-supplied pod shape is a
// construction error, never a silent override.
func ApplyToPod(pod *corev1.Pod, asm *PodAssembly) error {
	if asm == nil {
		return nil
	}
	existingVolumes := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		existingVolumes[v.Name] = true
	}
	for _, v := range asm.Volumes {
		if existingVolumes[v.Name] {
			return fmt.Errorf("capability assembly volume %q collides with an existing pod volume; refusing to shadow pod shape", v.Name)
		}
		existingVolumes[v.Name] = true
		pod.Spec.Volumes = append(pod.Spec.Volumes, v)
	}

	agentIdx := -1
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == AgentContainerName {
			agentIdx = i
			break
		}
	}
	if agentIdx < 0 {
		return fmt.Errorf("capability assembly requires the sandbox %q container; refusing to apply to a pod without one", AgentContainerName)
	}
	agent := &pod.Spec.Containers[agentIdx]

	existingEnv := map[string]bool{}
	for _, e := range agent.Env {
		existingEnv[e.Name] = true
	}
	for _, e := range asm.AgentEnv {
		if existingEnv[e.Name] {
			return fmt.Errorf("capability assembly env %q collides with an existing agent container env; refusing to shadow pod shape", e.Name)
		}
		existingEnv[e.Name] = true
		agent.Env = append(agent.Env, e)
	}

	existingMounts := map[string]bool{}
	for _, m := range agent.VolumeMounts {
		existingMounts[m.Name] = true
	}
	for _, m := range asm.AgentMounts {
		if existingMounts[m.Name] {
			return fmt.Errorf("capability assembly mount %q collides with an existing agent container mount; refusing to shadow pod shape", m.Name)
		}
		existingMounts[m.Name] = true
		agent.VolumeMounts = append(agent.VolumeMounts, m)
	}

	pod.Spec.InitContainers = append(pod.Spec.InitContainers, asm.InitContainers...)
	return nil
}

// MCPConfigMapName is the per-Run ConfigMap carrying the MCP IR:
// ksquad-run-<run-name>-mcp (same naming dialect as the RBAC renderer).
func MCPConfigMapName(run *api.Run) string {
	return "ksquad-run-" + run.Name + "-mcp"
}

// irDocument is the projected document: versioned for forward evolution
// without re-parsing ambiguity in the runtime adapters.
type irDocument struct {
	Version   int        `json:"version"`
	Endpoints []Endpoint `json:"endpoints"`
}

// RenderMCPConfigData marshals the IR document the ConfigMap carries.
func RenderMCPConfigData(endpoints []Endpoint) ([]byte, error) {
	return json.MarshalIndent(irDocument{Version: 1, Endpoints: endpoints}, "", "  ")
}

// MCPConfigMapFor builds the per-Run MCP IR ConfigMap, owner-ref'd to the
// Run (GC with the Run, same discipline as the rendered Role). The
// sandbox projects it read-only; runtime adapters read it from
// K8SQUAD_MCP_CONFIG and render native config at start.
func MCPConfigMapFor(run *api.Run, endpoints []Endpoint) (*corev1.ConfigMap, error) {
	data, err := RenderMCPConfigData(endpoints)
	if err != nil {
		return nil, fmt.Errorf("render mcp ir for run %s/%s: %w", run.Namespace, run.Name, err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MCPConfigMapName(run),
			Namespace: run.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "ksquad-operator",
				"ksquad.io/run":                run.Name,
			},
			// Controller owner reference: Kubernetes GC deletes the map
			// with the Run (same discipline as the rendered per-Run Role).
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(run, api.GroupVersion.WithKind("Run"))},
		},
		Data: map[string]string{MCPConfigFile: string(data)},
	}
	return cm, nil
}

// MCPConfigVolume returns the projected ConfigMap volume + the agent's
// read-only mount for the IR.
func MCPConfigVolume(run *api.Run) (corev1.Volume, corev1.VolumeMount) {
	vol := corev1.Volume{
		Name: MCPConfigVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: MCPConfigMapName(run)},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      MCPConfigVolumeName,
		MountPath: MCPConfigMountPath,
		ReadOnly:  true,
	}
	return vol, mount
}

// EnsureMCPConfigMap converges the per-Run IR ConfigMap (create-or-update
// keyed on content; the manifest is immutable for the life of the Run, so
// convergence only repairs drift).
func EnsureMCPConfigMap(ctx context.Context, c client.Client, run *api.Run, endpoints []Endpoint) error {
	if len(endpoints) == 0 {
		// Nothing owed — remove a stale map if the envelope shrank
		// (only possible across Run generations; terminal Runs GC via
		// the owner reference anyway).
		stale := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: MCPConfigMapName(run), Namespace: run.Namespace}}
		if err := c.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale mcp configmap for run %s/%s: %w", run.Namespace, run.Name, err)
		}
		return nil
	}
	desired, err := MCPConfigMapFor(run, endpoints)
	if err != nil {
		return err
	}
	existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("read mcp configmap %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		return c.Create(ctx, desired)
	}
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	return c.Update(ctx, existing)
}
