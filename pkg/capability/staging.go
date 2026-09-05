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
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

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

// Skill body + permission projection constants (ADR-0004 rev.2 §Decision.2,
// seam S-B): each granted INLINE skill is delivered as its own projected
// ConfigMap mounted at ${SkillsMountPath}/<name>/, carrying SKILL.md (the
// untrusted body) and permissions.json (the reconciler-authored envelope).
// One skill → one ConfigMap → one <name>/ subdir — never an aggregate
// (F2: aggregation risks the 1 MiB ConfigMap ceiling once several default
// skills are enabled). KSQUAD_SKILLS_DIR names the root for the shim
// (S-C reads it; git-sourced bodies arrive via S-D, not this projection).
const (
	// SkillsMountPath is the per-Run root directory every granted skill
	// lands under; each skill occupies its own <name>/ subdir.
	SkillsMountPath = "/ksquad/skills"

	// SkillsDirEnvVar names the skills root directory for the shim.
	SkillsDirEnvVar = "KSQUAD_SKILLS_DIR"

	// SkillBodyFile is the body file inside a skill's dir (data, not
	// authority — D8).
	SkillBodyFile = "SKILL.md"

	// SkillPermissionsFile is the reconciler-authored permission envelope
	// inside a skill's dir (authority, not data — D8: serialized from
	// GrantedSkill.Permissions only, never from body content).
	SkillPermissionsFile = "permissions.json"
)

// SkillsConfigMapName is the per-skill ConfigMap carrying one granted
// inline skill's body + envelope: ksquad-run-<run-name>-skill-<name>
// (same naming dialect as the MCP IR map, fanned out per skill).
func SkillsConfigMapName(run *api.Run, skillName string) string {
	return "ksquad-run-" + run.Name + "-skill-" + skillName
}

// SkillsVolumeName derives the per-skill projected volume name
// ("ksquad-skill-<name>"). Volume names are DNS_LABELs (≤63 chars, no
// dots) while skill names are DNS subdomains, so the name is sanitized
// and — only when truncation is required — disambiguated with a short
// hash of the original skill name.
func SkillsVolumeName(skillName string) string {
	const prefix = "ksquad-skill-"
	name := sanitizeVolumeLabel(skillName)
	if len(prefix)+len(name) <= dnsLabelMaxLen {
		return prefix + name
	}
	sum := sha256.Sum256([]byte(skillName))
	suffix := hex.EncodeToString(sum[:])[:8]
	keep := dnsLabelMaxLen - len(prefix) - len(suffix) - 1
	return prefix + name[:keep] + "-" + suffix
}

// dnsLabelMaxLen is the Kubernetes volume-name ceiling (DNS_LABEL).
const dnsLabelMaxLen = 63

// sanitizeVolumeLabel folds a DNS subdomain into a DNS_LABEL-safe form:
// lowercase, dots and underscores to dashes, everything else outside
// [a-z0-9-] to a dash (skill names are CRD-validated DNS subdomains, so
// this is belt-and-braces, never a loader).
func sanitizeVolumeLabel(name string) string {
	lowered := strings.ToLower(name)
	out := []byte(lowered)
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_':
			out[i] = '-'
		default:
			out[i] = '-'
		}
	}
	trimmed := strings.Trim(string(out), "-")
	if trimmed == "" {
		return "skill"
	}
	return trimmed
}

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
