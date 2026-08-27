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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolchain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resolvedToolchains() []toolchain.Resolved {
	return []toolchain.Resolved{
		{Name: "kubectl", Version: "1.31", Image: "ghcr.io/k8squad/toolchains/kubectl:1.31", SourceNamespace: "ksquad-system"},
		{Name: "git", Version: "2.62", Image: "ghcr.io/k8squad/toolchains/git:2.62", SourceNamespace: "ksquad-system"},
	}
}

func stdioEndpoint() Endpoint {
	return Endpoint{
		Name:                "gh-stdio",
		Transport:           "stdio",
		Command:             "gh-mcp-serve",
		Args:                []string{"--org", "acme"},
		Image:               "ghcr.io/k8squad/mcp/gh:1.4",
		AllowTools:          []string{"create_pull_request"},
		EnvNames:            []string{"KSQUAD_MCP_GH_STDIO_TOKEN"},
		CredentialSecretRef: &api.SecretRef{Name: "gh-token"},
	}
}

func httpEndpoint() Endpoint {
	return Endpoint{
		Name:                "github-mcp",
		Transport:           "streamable-http",
		URL:                 "https://mcp.example/github",
		AllowTools:          []string{"list_issues"},
		CredentialSecretRef: &api.SecretRef{Name: "github-token", Key: "pat"},
	}
}

func TestRenderInitContainersOrderAndContract(t *testing.T) {
	inits := RenderInitContainers(resolvedToolchains())
	require.Len(t, inits, 2)
	// Resolver order (sorted by name): git before kubectl — deterministic
	// staging sequence.
	assert.Equal(t, "stage-git", inits[0].Name)
	assert.Equal(t, "ghcr.io/k8squad/toolchains/git:2.62", inits[0].Image)
	assert.Equal(t, "stage-kubectl", inits[1].Name)

	for _, c := range inits {
		assert.Equal(t, []string{"cp", "-a", "/toolchain/bin/.", "/tools/bin/"}, c.Command)
		require.Len(t, c.VolumeMounts, 1)
		assert.Equal(t, ToolVolumeName, c.VolumeMounts[0].Name)
		assert.Equal(t, ToolMountPath, c.VolumeMounts[0].MountPath)
		require.NotNil(t, c.SecurityContext)
		assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation)
		assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	}
}

func TestAssemblePodKubectlOnPathAndROTools(t *testing.T) {
	run := newRun()
	asm, err := AssemblePod(run, resolvedToolchains(), nil)
	require.NoError(t, err)

	// PATH wiring lands on the agent env, tools mount read-only.
	found := false
	for _, e := range asm.AgentEnv {
		if e.Name == "PATH" {
			found = true
			assert.Equal(t, ToolPathValue, e.Value)
			assert.True(t, strings.HasPrefix(e.Value, "/tools/bin:"))
		}
	}
	assert.True(t, found, "agent PATH env present")
	require.Len(t, asm.AgentMounts, 1)
	assert.True(t, asm.AgentMounts[0].ReadOnly)
	assert.Equal(t, ToolVolumeName, asm.AgentMounts[0].Name)

	// Applying onto a sandbox-shaped pod puts kubectl on PATH.
	pod := sandboxShapedPod()
	require.NoError(t, ApplyToPod(pod, asm))
	agent := podContainer(t, pod, "agent")
	pathVal := envValue(t, agent.Env, "PATH")
	assert.True(t, strings.HasPrefix(pathVal, "/tools/bin:"))
}

func TestAssemblePodMCPSidecarAndCredentials(t *testing.T) {
	run := newRun()
	asm, err := AssemblePod(run, resolvedToolchains(), []Endpoint{stdioEndpoint(), httpEndpoint()})
	require.NoError(t, err)

	// Init order: staging first, then the stdio sidecar.
	require.Len(t, asm.InitContainers, 3)
	assert.Equal(t, "stage-git", asm.InitContainers[0].Name)
	assert.Equal(t, "stage-kubectl", asm.InitContainers[1].Name)
	sidecar := asm.InitContainers[2]
	assert.Equal(t, "mcp-gh-stdio", sidecar.Name)
	assert.Equal(t, "ghcr.io/k8squad/mcp/gh:1.4", sidecar.Image)
	require.NotNil(t, sidecar.RestartPolicy)
	assert.Equal(t, corev1.ContainerRestartPolicyAlways, *sidecar.RestartPolicy)
	require.NotNil(t, sidecar.SecurityContext)
	assert.False(t, *sidecar.SecurityContext.AllowPrivilegeEscalation)

	// stdio-with-image credential rides the SIDECAR as a SecretKeyRef.
	cred := envValueFrom(t, sidecar.Env, "KSQUAD_MCP_GH_STDIO_TOKEN")
	require.NotNil(t, cred.SecretKeyRef)
	assert.Equal(t, "gh-token", cred.SecretKeyRef.Name)
	assert.Equal(t, "token", cred.SecretKeyRef.Key)

	// streamable-http credential rides the AGENT env with the explicit key.
	pod := sandboxShapedPod()
	require.NoError(t, ApplyToPod(pod, asm))
	agent := podContainer(t, pod, "agent")
	httpCred := envValueFrom(t, agent.Env, "KSQUAD_MCP_GITHUB_MCP_TOKEN")
	require.NotNil(t, httpCred.SecretKeyRef)
	assert.Equal(t, "github-token", httpCred.SecretKeyRef.Name)
	assert.Equal(t, "pat", httpCred.SecretKeyRef.Key)

	// IR pointer env + mounts.
	assert.Equal(t, "/ksquad/mcp/config.json", envValue(t, agent.Env, MCPConfigEnvVar))
	assert.True(t, hasMount(agent.VolumeMounts, MCPConfigVolumeName))
}

func TestAssemblePodNoSidecarForHTTPOrBareStdio(t *testing.T) {
	run := newRun()
	bare := Endpoint{Name: "in-image", Transport: "stdio", Command: "bundled-mcp"}
	asm, err := AssemblePod(run, nil, []Endpoint{httpEndpoint(), bare})
	require.NoError(t, err)
	assert.Empty(t, asm.InitContainers, "no sidecar for http or stdio-without-image")
	assert.NotEmpty(t, asm.AgentEnv)
}

func TestAssemblePodEmptyEnvelopeIsBare(t *testing.T) {
	asm, err := AssemblePod(newRun(), nil, nil)
	require.NoError(t, err)
	assert.Empty(t, asm.InitContainers)
	assert.Empty(t, asm.Volumes)
	assert.Empty(t, asm.AgentEnv)
	assert.Empty(t, asm.AgentMounts)
}

func TestApplyToPodCollisionFailsClosed(t *testing.T) {
	run := newRun()
	asm, err := AssemblePod(run, resolvedToolchains(), []Endpoint{stdioEndpoint()})
	require.NoError(t, err)

	shadowed := sandboxShapedPod()
	shadowed.Spec.Volumes = append(shadowed.Spec.Volumes, ToolVolume())
	err = ApplyToPod(shadowed, asm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides")

	envClash := sandboxShapedPod()
	envClash.Spec.Containers[0].Env = append(envClash.Spec.Containers[0].Env, corev1.EnvVar{Name: "PATH", Value: "/bin"})
	err = ApplyToPod(envClash, asm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PATH")

	noAgent := sandboxShapedPod()
	noAgent.Spec.Containers[0].Name = "not-agent"
	err = ApplyToPod(noAgent, asm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"agent" container`)
}

func TestMCPConfigMapFor(t *testing.T) {
	run := newRun()
	cm, err := MCPConfigMapFor(run, []Endpoint{httpEndpoint()})
	require.NoError(t, err)
	assert.Equal(t, "ksquad-run-r1-mcp", cm.Name)
	assert.Equal(t, runNS, cm.Namespace)
	assert.Equal(t, "ksquad-operator", cm.Labels["app.kubernetes.io/managed-by"])

	require.Len(t, cm.OwnerReferences, 1)
	assert.True(t, *cm.OwnerReferences[0].Controller)

	assert.Contains(t, cm.Data[MCPConfigFile], `"version": 1`)
	assert.Contains(t, cm.Data[MCPConfigFile], `"github-mcp"`)
	assert.NotContains(t, cm.Data[MCPConfigFile], "github-token-value")
	// Secret NAME appears (provenance), secret material never does —
	// there is nothing to assert beyond the name being a name.
	assert.Contains(t, cm.Data[MCPConfigFile], `"name": "github-token"`)
}

func sandboxShapedPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ksquad-sandbox-r1", Namespace: runNS},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "runtime:1"}},
			Volumes:    []corev1.Volume{{Name: "tmp"}},
		},
	}
}

func podContainer(t *testing.T, pod *corev1.Pod, name string) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	t.Fatalf("container %q not found", name)
	return nil
}

func envValue(t *testing.T, env []corev1.EnvVar, name string) string {
	t.Helper()
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	t.Fatalf("env %q not found", name)
	return ""
}

func envValueFrom(t *testing.T, env []corev1.EnvVar, name string) *corev1.EnvVarSource {
	t.Helper()
	for _, e := range env {
		if e.Name == name {
			require.NotNil(t, e.ValueFrom)
			return e.ValueFrom
		}
	}
	t.Fatalf("env %q not found", name)
	return nil
}

func hasMount(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}
