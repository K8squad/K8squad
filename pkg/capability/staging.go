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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolchain"
)

// The tool-staging contract (ADR-044 step 6, spike C image strategy):
//
//   - every toolchain image exposes its provides[] binaries under
//     /toolchain/bin/ and contains a `cp` (busybox-level is enough — the
//     curated catalog images are built to this contract);
//   - Run assembly stages ONE init container per resolved toolchain, in
//     resolver order (sorted by name), copying /toolchain/bin/ onto a
//     pod-local shared emptyDir mounted at /tools;
//   - the runtime (agent) container mounts /tools READ-ONLY and gets
//     PATH=/tools/bin:<default path> prepended — kubectl lands on PATH
//     before the runtime starts (sequential init containers, NFR);
//   - emptyDir, not PVC: staging is pod-local and immutable-in-practice
//     (ADR-044 consequences); digest-pinned images make it reproducible.
const (
	// ToolVolumeName is the shared tool staging volume (emptyDir).
	ToolVolumeName = "ksquad-tools"

	// ToolMountPath is where the shared tool volume lands in-container.
	ToolMountPath = "/tools"

	// ToolBinSubdir is the bin directory inside the tool volume.
	ToolBinSubdir = "bin"

	// StagingSourceDir is the in-image binary directory the staging
	// contract requires toolchain images to expose.
	StagingSourceDir = "/toolchain/bin"

	// ToolPathValue is the PATH the agent container runs with: staged
	// tools first, then the standard locations. Deterministic (env var
	// expansion of an existing PATH is not expressible in a pod spec).
	ToolPathValue = "/tools/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// MCP config projection constants (ADR-044 step 6): the IR is delivered
// as a projected ConfigMap; K8SQUAD_MCP_CONFIG names its path so the
// runtime adapters (pkg/shim/runtimes) can render native config at start.
const (
	// MCPConfigVolumeName is the projected-ConfigMap volume carrying the
	// MCP IR.
	MCPConfigVolumeName = "ksquad-mcp-config"

	// MCPConfigMountPath is where the IR lands in-container.
	MCPConfigMountPath = "/ksquad/mcp"

	// MCPConfigFile is the IR file name inside the mount.
	MCPConfigFile = "config.json"

	// MCPConfigEnvVar names the IR path for the runtime adapters.
	MCPConfigEnvVar = "K8SQUAD_MCP_CONFIG"
)

// ToolVolume returns the shared staging volume (pod-local emptyDir).
func ToolVolume() corev1.Volume {
	return corev1.Volume{
		Name: ToolVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
}

// RenderInitContainers renders one hardened init container per resolved
// toolchain (ADR-044 step 6): image pinned by the resolver, staging copy
// per the contract above, no privilege, automountServiceAccountToken is
// pod-level false (ADR-045). Containers are name-ordered so staging is
// deterministic regardless of caller order (the resolver already sorts;
// this re-asserts it).
func RenderInitContainers(resolved []toolchain.Resolved) []corev1.Container {
	ordered := append([]toolchain.Resolved(nil), resolved...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	inits := make([]corev1.Container, 0, len(ordered))
	for _, res := range ordered {
		inits = append(inits, corev1.Container{
			Name:  "stage-" + res.Name,
			Image: res.Image,
			Command: []string{
				"cp", "-a", StagingSourceDir + "/.", ToolMountPath + "/" + ToolBinSubdir + "/",
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: ToolVolumeName, MountPath: ToolMountPath},
			},
			SecurityContext: hardenedContainerSecurity(),
		})
	}
	return inits
}

// ToolVolumeMounts returns the agent container's read-only tool mounts.
func ToolVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: ToolVolumeName, MountPath: ToolMountPath, ReadOnly: true},
	}
}

// ToolPathEnv returns the PATH env var wiring staged tools onto the
// runtime container's PATH.
func ToolPathEnv() corev1.EnvVar {
	return corev1.EnvVar{Name: "PATH", Value: ToolPathValue}
}

// hardenedContainerSecurity is the ADR-045 posture every ADDED container
// (init, sidecar) carries, matching the agent container's discipline.
func hardenedContainerSecurity() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// renderMCPSidecar renders the native sidecar for a stdio MCPServer with
// an image (ADR-044 step 6): a K8s ≥1.28 restartable init container, so
// the server starts before the runtime container and stays alive beside
// it. The credential rides as a SecretKeyRef env — the kubelet
// materializes it, the control plane never reads the Secret (ADR-045 D5).
func renderMCPSidecar(ep Endpoint, cred *api.SecretRef) corev1.Container {
	c := corev1.Container{
		Name:            "mcp-" + ep.Name,
		Image:           ep.Image,
		Command:         []string{ep.Command},
		Args:            append([]string(nil), ep.Args...),
		SecurityContext: hardenedContainerSecurity(),
		// Native sidecar (KEP-753): restartable init container.
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		VolumeMounts:  []corev1.VolumeMount{},
	}
	if cred != nil && cred.Name != "" {
		key := cred.Key
		if key == "" {
			key = defaultCredentialKey
		}
		c.Env = append(c.Env, corev1.EnvVar{
			Name: CredentialEnvName(ep.Name),
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cred.Name},
					Key:                  key,
				},
			},
		})
	}
	return c
}

// credentialEnvFor returns the agent-container env projection for a
// streamable-http server's credential (the runtime's MCP client needs the
// token to authenticate; stdio sidecars get theirs on the sidecar itself).
func credentialEnvFor(ep Endpoint, cred *api.SecretRef) []corev1.EnvVar {
	if cred == nil || cred.Name == "" {
		return nil
	}
	key := cred.Key
	if key == "" {
		key = defaultCredentialKey
	}
	return []corev1.EnvVar{{
		Name: CredentialEnvName(ep.Name),
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: cred.Name},
				Key:                  key,
			},
		},
	}}
}
