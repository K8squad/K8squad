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
	"github.com/K8squad/K8squad/pkg/toolcred"
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
	// projection, per-skill body+envelope projections).
	Volumes []corev1.Volume

	// AgentEnv is env added to the agent container: PATH wiring,
	// K8SQUAD_MCP_CONFIG, KSQUAD_SKILLS_DIR, streamable-http credential
	// projections.
	AgentEnv []corev1.EnvVar

	// AgentMounts are mounts added to the agent container (read-only;
	// one per granted inline skill at /ksquad/skills/<name>/).
	AgentMounts []corev1.VolumeMount
}

// AgentContainerName is the sandbox package's runtime container name
// (pkg/sandbox: the one container BuildSandboxPod emits is "agent").
const AgentContainerName = "agent"

// AssemblePod computes the capability contribution for a sandbox pod from
// the resolved envelope (ADR-044 step 6, ADR-0004 S-B). Pure: no cluster
// access — the caller supplies the resolver's output, the resolved
// endpoints, and the granted skills. Skills fan out per granted INLINE
// skill: one projected ConfigMap volume + read-only mount at
// ${SkillsMountPath}/<name>/ each, plus the KSQUAD_SKILLS_DIR env naming
// the root (git-sourced skills are S-D, not this seam).
func AssemblePod(run *api.Run, resolved []toolchain.Resolved, endpoints []Endpoint, skills []GrantedSkill) (*PodAssembly, error) {
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

	// Skill body + permission projection (ADR-0004 S-B): per granted
	// inline skill, one projected ConfigMap volume mounted read-only at
	// its own ${KSQUAD_SKILLS_DIR}/<name>/ subdir; the env names the
	// single root the shim (S-C) walks. No inline skills → no volumes,
	// no env — the bare posture is unchanged.
	inlineSkills := InlineSkills(skills)
	for _, skill := range inlineSkills {
		vol, mount := SkillConfigMapVolume(run, skill.Name)
		asm.Volumes = append(asm.Volumes, vol)
		asm.AgentMounts = append(asm.AgentMounts, mount)
	}
	if len(inlineSkills) > 0 {
		asm.AgentEnv = append(asm.AgentEnv, corev1.EnvVar{
			Name:  SkillsDirEnvVar,
			Value: SkillsMountPath,
		})
	}

	// Auxiliary (non-model) tool credentials (ISI-3565): a github-token
	// entry lands GH_TOKEN + GITHUB_TOKEN on the agent container by
	// reference for a local gh/git. This rides the LIVE assembly seam
	// (sibling to the MCP credentialEnvFor path above) rather than the dark
	// pkg/credential.Resolve. Fail closed on an unknown purpose or empty
	// Secret name — a mis-declared aux credential must abort assembly, not
	// dispatch a sandbox whose gh/git authenticates as nobody. The
	// ApplyToPod env-collision guard then rejects an aux env name that would
	// shadow a model-cred or KSQUAD_MCP_* env.
	for i, tc := range run.Spec.ToolCredentials {
		inj, err := toolcred.Inject(toolcred.Purpose(tc.Purpose), tc.SecretRef)
		if err != nil {
			return nil, fmt.Errorf("assemble tool credential spec.toolCredentials[%d]: %w", i, err)
		}
		asm.AgentEnv = append(asm.AgentEnv, inj.Env...)
		asm.Volumes = append(asm.Volumes, inj.Volumes...)
		asm.AgentMounts = append(asm.AgentMounts, inj.Mounts...)
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

// maxSkillConfigMapBytes is the hard Kubernetes ConfigMap ceiling this
// projection must respect per skill (ADR-0004 rev.2 F2 / AC6 backstop).
// S-F admission already caps source.inline at 256 KiB, so this branch is
// unreachable for well-formed CRs — it exists as defense-in-depth.
const maxSkillConfigMapBytes = 1 << 20

// permissionsDocument is the reconciler-authored permission envelope the
// per-skill ConfigMap carries: versioned for forward evolution, mirroring
// the MCP IR document discipline. Permissions is ALWAYS a list — an empty
// envelope marshals as [], never null, never absent (AC2 fail-closed
// default; the vocabulary is GrantedSkill.Permissions verbatim).
type permissionsDocument struct {
	Version     int      `json:"version"`
	Permissions []string `json:"permissions"`
}

// RenderSkillPermissions serializes a granted skill's permission envelope
// from GrantedSkill.Permissions ONLY (D8 trust boundary): the single
// write path for permissions.json. Nothing here reads the body — the
// caller keeps the body (SKILL.md) and envelope (permissions.json) writes
// visibly separate (see SkillConfigMapFor).
func RenderSkillPermissions(permissions []string) ([]byte, error) {
	if permissions == nil {
		permissions = []string{}
	}
	return json.MarshalIndent(permissionsDocument{Version: 1, Permissions: permissions}, "", "  ")
}

// InlineSkills filters a granted-skill set down to the inline bodies this
// projection delivers (ADR-0004 S-B scope: git-sourced bodies arrive via
// the S-D init-container fetch, never through this ConfigMap path — the
// guard is explicit so a git skill can never leak an empty inline body
// into a projected map).
func InlineSkills(skills []GrantedSkill) []GrantedSkill {
	out := make([]GrantedSkill, 0, len(skills))
	for _, s := range skills {
		if s.SourceType == api.SkillSourceInline {
			out = append(out, s)
		}
	}
	return out
}

// SkillConfigMapFor builds ONE granted inline skill's projected ConfigMap
// (F2: per skill, never an aggregate): ksquad-run-<run>-skill-<name>,
// owner-ref'd to the Run (GC with the Run, same discipline as the MCP IR
// map). Two data keys from two visibly separate sources —
//
//	SKILL.md         ← the untrusted inline body (data)
//	permissions.json ← GrantedSkill.Permissions, CR envelope (authority)
//
// — the D8 trust boundary in one object. Fail-closed on the 1 MiB
// ConfigMap ceiling (AC6 backstop): the error names the offending skill
// and suggests git-sourcing; a body is never truncated.
func SkillConfigMapFor(run *api.Run, skill GrantedSkill) (*corev1.ConfigMap, error) {
	// D8: the envelope is serialized from the CR-copied permissions
	// before the body is even looked at — separate sources, same object.
	perms, err := RenderSkillPermissions(skill.Permissions)
	if err != nil {
		return nil, fmt.Errorf("render permissions envelope for skill %s: %w", skill.Name, err)
	}
	name := SkillsConfigMapName(run, skill.Name)
	if len(name) > 253 {
		return nil, fmt.Errorf("skill configmap name %q exceeds the 253-char object name limit; shorten the Run or Skill name", name)
	}
	data := map[string]string{
		SkillBodyFile:        skill.Inline,
		SkillPermissionsFile: string(perms),
	}
	var total int
	for _, v := range data {
		total += len(v)
	}
	if total > maxSkillConfigMapBytes {
		return nil, fmt.Errorf("skill %q projection is %d bytes, over the 1 MiB ConfigMap limit; git-source this skill instead (source.type=git)", skill.Name, total)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: run.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "ksquad-operator",
				"ksquad.io/run":                run.Name,
				"ksquad.io/component":          "skill",
				"ksquad.io/skill":              skill.Name,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(run, api.GroupVersion.WithKind("Run"))},
		},
		Data: data,
	}, nil
}

// SkillConfigMapVolume returns the per-skill projected ConfigMap volume
// and the agent's read-only mount at ${SkillsMountPath}/<name>/ — one
// volume+mount per granted skill (F2), beside the MCP IR volume.
func SkillConfigMapVolume(run *api.Run, skillName string) (corev1.Volume, corev1.VolumeMount) {
	volName := SkillsVolumeName(skillName)
	vol := corev1.Volume{
		Name: volName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: SkillsConfigMapName(run, skillName)},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      volName,
		MountPath: SkillsMountPath + "/" + skillName,
		ReadOnly:  true,
	}
	return vol, mount
}

// EnsureSkillConfigMaps converges the per-skill projection ConfigMaps for
// a Run's granted INLINE skills (one object each, F2) and garbage-collects
// skill ConfigMaps no longer owed — same create-or-update drift-repair
// posture as EnsureMCPConfigMap, fanned out per skill. Git-sourced skills
// are skipped explicitly (S-D delivers those); a Run granting no inline
// skills produces no ConfigMaps and the bare posture is unchanged.
func EnsureSkillConfigMaps(ctx context.Context, c client.Client, run *api.Run, skills []GrantedSkill) error {
	inline := InlineSkills(skills)

	desired := map[string]bool{}
	for _, skill := range inline {
		cm, err := SkillConfigMapFor(run, skill)
		if err != nil {
			return fmt.Errorf("build skill configmap for run %s/%s: %w", run.Namespace, run.Name, err)
		}
		desired[cm.Name] = true
		existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cm.Name, Namespace: cm.Namespace}}
		if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("read skill configmap %s/%s: %w", cm.Namespace, cm.Name, err)
			}
			if err := c.Create(ctx, cm); err != nil {
				return fmt.Errorf("create skill configmap %s/%s: %w", cm.Namespace, cm.Name, err)
			}
			continue
		}
		existing.Labels = cm.Labels
		existing.Data = cm.Data
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("update skill configmap %s/%s: %w", cm.Namespace, cm.Name, err)
		}
	}

	// GC: sweep this Run's skill-projection ConfigMaps that are no longer
	// owed (a skill dropped across generations — the manifest is
	// immutable, so this is the same drift case EnsureMCPConfigMap's
	// empty-endpoints delete covers, fanned out per skill).
	var live corev1.ConfigMapList
	if err := c.List(ctx, &live,
		client.InNamespace(run.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/managed-by": "ksquad-operator",
			"ksquad.io/run":                run.Name,
			"ksquad.io/component":          "skill",
		}); err != nil {
		return fmt.Errorf("list skill configmaps for run %s/%s: %w", run.Namespace, run.Name, err)
	}
	for i := range live.Items {
		name := live.Items[i].Name
		if desired[name] {
			continue
		}
		stale := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: run.Namespace}}
		if err := c.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale skill configmap %s/%s: %w", run.Namespace, name, err)
		}
	}
	return nil
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
