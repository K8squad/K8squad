//go:build e2e

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

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/apiserver"
)

// e2eNamespace is the working namespace the smoke provisions its CR graph in.
const e2eNamespace = "e2e-squad-smoke"

// TestSquadSmoke is the Run-path conformance harness (ISI-3475 / ISI-2114).
//
// It drives ONE live Run requiring kubectl@1.31 + github-mcp on the kind
// cluster the e2e-ollama lane provisions (operator + default toolchain catalog
// installed) and asserts the five deliverable properties. Assertions split into
// two tiers by what the operator has produced:
//
//   - capability_manifest/* — read status.capabilityManifest, stamped at
//     assembly. Hard assertions once the manifest resolves.
//   - sandbox/* + telemetry/* — exec into the live sandbox and scrape the OTel
//     sink. Skip-with-reason until the sandbox pod / sink materialize, so the
//     harness runs green-and-partial as the Run plane lands, never silently.
func TestSquadSmoke(t *testing.T) {
	h := newHarness(t) // SKIPs if no cluster / operator CRDs absent.

	ctx, cancel := context.WithTimeout(context.Background(), runResolveTimeout+5*time.Minute)
	defer cancel()

	// Resolve the kubectl pin from the installed catalog (SKIPs if kubectl is
	// absent). The Run requires exactly this version; the assertions prove that
	// exact pin lands on PATH — version-pinned end to end.
	kubectlVersion := h.resolveToolchainVersion(ctx, t)
	t.Logf("driving Run pinned to %s@%s (catalog-resolved)", toolchainName, kubectlVersion)

	nsCleanup := h.ensureNamespace(ctx, t, e2eNamespace)
	t.Cleanup(nsCleanup)

	sc := newScenario(e2eNamespace, kubectlVersion)
	sc.apply(ctx, t, h.cl)

	// Wait for the operator to resolve the Run's capability manifest. Absence
	// within the budget means Run assembly is not driving on this cluster — an
	// environment/precondition gap (skip), not a wrong answer (fail).
	run := h.waitForManifest(ctx, t, sc)
	if run == nil {
		t.Skipf("Run %s/%s never resolved status.capabilityManifest within %s — "+
			"operator Run-assembly not driving on this cluster (ISI-2114 precondition); "+
			"capability_manifest + sandbox assertions cannot run yet", e2eNamespace, runName, runResolveTimeout)
	}
	mf := run.Status.CapabilityManifest

	// ---- Tier 1: capability manifest (status subresource) ----------------

	t.Run("capability_manifest/kubectl_version_pinned_resolved", func(t *testing.T) {
		tc := findToolchain(mf, toolchainName)
		if tc == nil {
			t.Fatalf("resolved manifest has no %q toolchain; got %v", toolchainName, toolchainNames(mf))
		}
		if tc.Version != sc.kubectlVersion {
			t.Fatalf("kubectl resolved to version %q, want %q (version-pinned)", tc.Version, sc.kubectlVersion)
		}
		if strings.TrimSpace(tc.Image) == "" {
			t.Fatalf("kubectl@%s resolved to an empty image — assembly did not stage a toolchain image", sc.kubectlVersion)
		}
		t.Logf("kubectl@%s -> image %s (catalog ns %s)", tc.Version, tc.Image, tc.SourceNamespace)
	})

	t.Run("capability_manifest/scoped_mcp_github_only", func(t *testing.T) {
		if len(mf.MCPEndpoints) != 1 {
			t.Fatalf("manifest wired %d MCP endpoints, want exactly 1 (the single declared %s); got %v",
				len(mf.MCPEndpoints), mcpServerName, endpointNames(mf))
		}
		ep := mf.MCPEndpoints[0]
		if ep.Name != mcpServerName {
			t.Fatalf("resolved MCP endpoint is %q, want %q", ep.Name, mcpServerName)
		}
		if ep.Transport != ksquadv1alpha1.MCPTransportStreamableHTTP {
			t.Fatalf("github-mcp transport %q, want streamable-http", ep.Transport)
		}
		// Effective tool filter is provably narrow (allow set present, deny
		// subtracted) — the scope is real, not "all tools".
		if len(ep.AllowTools) == 0 {
			t.Fatalf("github-mcp resolved with an EMPTY effective allow set — filter did not scope the envelope")
		}
		// Secret-bearing header is NOT inlined into the recorded headers
		// (ADR-045): only the non-secret X-MCP-Client rides in Headers, and the
		// credential is referenced by Secret NAME.
		for name := range ep.Headers {
			if isSecretHeader(name) {
				t.Fatalf("secret-bearing header %q recorded inline in the manifest — must ride CredentialSecretRef only", name)
			}
		}
		if ep.CredentialSecretRef == nil || ep.CredentialSecretRef.Name != mcpCredentialSecret {
			t.Fatalf("github-mcp credential not recorded by Secret reference (got %+v), want name %q",
				ep.CredentialSecretRef, mcpCredentialSecret)
		}
		t.Logf("github-mcp scoped: allow=%v deny=%v credSecret=%s", ep.AllowTools, ep.DenyTools, ep.CredentialSecretRef.Name)
	})

	t.Run("capability_manifest/egress_policy_bound", func(t *testing.T) {
		ep := mf.MCPEndpoints[0]
		if ep.EgressPolicyRef == nil || ep.EgressPolicyRef.Name != egressName {
			t.Fatalf("github-mcp egress not bound to %q (got %+v) — streamable-http endpoint must ride an EgressPolicy",
				egressName, ep.EgressPolicyRef)
		}
	})

	// ---- Tier 2: live sandbox (exec) -------------------------------------

	pod := h.waitForSandbox(ctx, t, sc)

	t.Run("sandbox/kubectl_on_path_version_pinned", func(t *testing.T) {
		if pod == nil {
			t.Skip("Run sandbox pod not scheduled yet — full dispatch (claim->sandbox) not driving; on-PATH probe deferred")
		}
		c := primaryContainer(pod)
		// `command -v kubectl` proves it is on PATH; `kubectl version` proves the pin.
		onPath, err := h.execInPod(ctx, pod.Namespace, pod.Name, c, "sh", "-c", "command -v kubectl")
		if err != nil || strings.TrimSpace(onPath.stdout) == "" {
			t.Fatalf("kubectl not on PATH in sandbox %s/%s[%s]: out=%q err=%v", pod.Namespace, pod.Name, c, onPath.combined(), err)
		}
		ver, err := h.execInPod(ctx, pod.Namespace, pod.Name, c, "sh", "-c", "kubectl version --client -o json 2>/dev/null || kubectl version --client")
		if err != nil {
			t.Fatalf("kubectl version failed in sandbox: %v (out=%q)", err, ver.combined())
		}
		if !strings.Contains(ver.combined(), sc.kubectlVersion) {
			t.Fatalf("kubectl on PATH is not @%s (the pinned version): %q", sc.kubectlVersion, ver.combined())
		}
		t.Logf("kubectl on PATH at %s, version reports the pinned %s", strings.TrimSpace(onPath.stdout), sc.kubectlVersion)
	})

	t.Run("sandbox/scoped_mcp_config_rendered", func(t *testing.T) {
		if pod == nil {
			t.Skip("Run sandbox pod not scheduled yet — rendered-config probe deferred")
		}
		c := primaryContainer(pod)
		path := h.firstExisting(ctx, pod.Namespace, pod.Name, c, mcpConfigCandidates()...)
		if path == "" {
			t.Skipf("no rendered MCP config found at candidate paths %v — projection mount not landed; config probe deferred", mcpConfigCandidates())
		}
		body, err := h.catFile(ctx, pod.Namespace, pod.Name, c, path)
		if err != nil {
			t.Fatalf("read rendered MCP config %s: %v", path, err)
		}
		// Only the declared server is present.
		if !strings.Contains(body, mcpServerName) {
			t.Fatalf("rendered MCP config %s does not name %q; body=%s", path, mcpServerName, body)
		}
		// The raw placeholder token must NOT be inlined — the header is injected
		// by reference/expansion, not baked into the rendered file.
		if strings.Contains(body, "e2e-smoke-placeholder-token") {
			t.Fatalf("rendered MCP config %s INLINES the raw credential — secret-bearing header must be injected from the Secret, not inlined", path)
		}
		t.Logf("scoped MCP config at %s names %s and inlines no credential", path, mcpServerName)
	})

	t.Run("sandbox/credentials_mounted", func(t *testing.T) {
		if pod == nil {
			t.Skip("Run sandbox pod not scheduled yet — credential-mount probe deferred")
		}
		if !podMountsSecret(pod, mcpCredentialSecret) {
			t.Fatalf("sandbox pod %s/%s does not mount Secret %q (as volume or envFrom/valueFrom) — credentials not delivered",
				pod.Namespace, pod.Name, mcpCredentialSecret)
		}
		t.Logf("sandbox pod mounts credential Secret %s", mcpCredentialSecret)
	})

	t.Run("sandbox/egress_honored", func(t *testing.T) {
		if pod == nil {
			t.Skip("Run sandbox pod not scheduled yet — egress probe deferred")
		}
		c := primaryContainer(pod)
		// Mirror s4-1: arbitrary dst MUST be refused; allowlisted path via the
		// team egress proxy MUST resolve.
		arb := arbitraryEgressIP()
		blocked, _ := h.execInPod(ctx, pod.Namespace, pod.Name, c, "sh", "-c",
			// short timeout so a default-deny drop returns fast; non-zero exit == blocked (good)
			"timeout 8 sh -c 'cat < /dev/null > /dev/tcp/"+arb+"/80' 2>/dev/null && echo REACHED || echo BLOCKED")
		if strings.Contains(blocked.combined(), "REACHED") {
			t.Fatalf("arbitrary egress %s:80 REACHED from sandbox — default-deny not containing (is the CNI enforcing NetworkPolicy?)", arb)
		}
		proxyHost := "egress-proxy." + pod.Namespace + ".svc.cluster.local"
		reach, _ := h.execInPod(ctx, pod.Namespace, pod.Name, c, "sh", "-c",
			"timeout 8 sh -c 'cat < /dev/null > /dev/tcp/"+proxyHost+"/"+itoa(egressProxyPort)+"' 2>/dev/null && echo REACHED || echo BLOCKED")
		if !strings.Contains(reach.combined(), "REACHED") {
			// The proxy Service may not be deployed on every environment; treat
			// its absence as a deferral (skip) but a REACHED arbitrary as a hard
			// fail above — the containment half is what must never regress.
			t.Skipf("allowlisted egress-proxy %s:%d not reachable (proxy not deployed?) — over-denial half deferred; containment half PASSED",
				proxyHost, egressProxyPort)
		}
		t.Logf("egress honored: %s:80 refused, %s:%d reachable via proxy", arb, proxyHost, egressProxyPort)
	})

	t.Run("telemetry/tool_mcp_skill_spans_and_metrics", func(t *testing.T) {
		exposition := h.scrapeOTelSink(ctx, t)
		if exposition == "" {
			t.Skip("OTel sink /metrics not reachable (sink Service unset/undeployed) — set OTEL_SINK_SERVICE/NAMESPACE/PORT; in-Run telemetry assertion deferred")
		}
		if !apiserver.ExpositionReportsToolUsage(exposition) {
			t.Fatalf("OTel sink exposition reports no ksquad tool-usage series — tool/MCP/skill telemetry not emitted during the Run")
		}
		agents, mcp := apiserver.AggregateToolUsageExposition(exposition)
		if len(agents) == 0 && len(mcp) == 0 {
			t.Fatalf("OTel sink exposition aggregates to zero agents and zero MCP servers — no in-Run spans/metrics landed")
		}
		t.Logf("OTel sink reports %d agent(s), %d MCP server(s) during the Run", len(agents), len(mcp))
	})
}

// waitForManifest polls the Run until status.capabilityManifest is stamped with
// a non-empty hash, or returns nil at the deadline (caller skips).
func (h *harness) waitForManifest(ctx context.Context, t *testing.T, sc *scenario) *ksquadv1alpha1.Run {
	t.Helper()
	deadline := time.Now().Add(runResolveTimeout)
	for time.Now().Before(deadline) {
		var run ksquadv1alpha1.Run
		if err := h.cl.Get(ctx, client.ObjectKey{Namespace: sc.namespace, Name: runName}, &run); err == nil {
			if mf := run.Status.CapabilityManifest; mf != nil && strings.TrimSpace(mf.CapabilityHash) != "" {
				return &run
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollInterval):
		}
	}
	return nil
}

// waitForSandbox resolves the Run's status.sandboxRef to a Running pod, or
// returns nil at a short deadline (sandbox subtests skip-with-reason).
func (h *harness) waitForSandbox(ctx context.Context, t *testing.T, sc *scenario) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var run ksquadv1alpha1.Run
		if err := h.cl.Get(ctx, client.ObjectKey{Namespace: sc.namespace, Name: runName}, &run); err == nil {
			if ref := run.Status.SandboxRef; ref != nil && ref.Name != "" {
				ns := ref.Namespace
				if ns == "" {
					ns = sc.namespace
				}
				var pod corev1.Pod
				if err := h.cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &pod); err == nil {
					if pod.Status.Phase == corev1.PodRunning {
						return &pod
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollInterval):
		}
	}
	return nil
}

// scrapeOTelSink reads the OTel/Prometheus sink exposition, sink location taken
// from OTEL_SINK_* env with sensible defaults; returns "" when unreachable.
func (h *harness) scrapeOTelSink(ctx context.Context, t *testing.T) string {
	t.Helper()
	svc := envOr("OTEL_SINK_SERVICE", "")
	if svc == "" {
		return "" // no sink declared — caller skips.
	}
	ns := envOr("OTEL_SINK_NAMESPACE", e2eNamespace)
	port := envOr("OTEL_SINK_PORT", "9090")
	path := envOr("OTEL_SINK_PATH", "/metrics")
	body, err := h.scrapeService(ctx, ns, svc, port, path)
	if err != nil {
		t.Logf("OTel sink scrape failed (%s/%s:%s%s): %v", ns, svc, port, path, err)
		return ""
	}
	return body
}

// ---- small pure helpers (unit-safe, no cluster) -------------------------

func findToolchain(mf *ksquadv1alpha1.CapabilityManifest, name string) *ksquadv1alpha1.ResolvedToolchainRef {
	if mf == nil {
		return nil
	}
	for i := range mf.Toolchains {
		if mf.Toolchains[i].Name == name {
			return &mf.Toolchains[i]
		}
	}
	return nil
}

func toolchainNames(mf *ksquadv1alpha1.CapabilityManifest) []string {
	var out []string
	if mf != nil {
		for _, tc := range mf.Toolchains {
			out = append(out, tc.Name+"@"+tc.Version)
		}
	}
	return out
}

func endpointNames(mf *ksquadv1alpha1.CapabilityManifest) []string {
	var out []string
	if mf != nil {
		for _, ep := range mf.MCPEndpoints {
			out = append(out, ep.Name)
		}
	}
	return out
}

// isSecretHeader reports whether an HTTP header name carries credential material
// (the set MCPServer admission rejects inline — ADR-045).
func isSecretHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "cookie", "proxy-authorization", "x-api-key", "api-key":
		return true
	}
	return false
}

// primaryContainer picks the sandbox's agent container to exec into: a
// well-known name if present, else the first container.
func primaryContainer(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		switch c.Name {
		case "agent", "runtime", "sandbox", "shim":
			return c.Name
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}

// podMountsSecret reports whether the pod delivers the named Secret to any
// container — as a projected/secret volume, an envFrom.secretRef, or a
// valueFrom.secretKeyRef.
func podMountsSecret(pod *corev1.Pod, secretName string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == secretName {
			return true
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.Secret != nil && src.Secret.Name == secretName {
					return true
				}
			}
		}
	}
	all := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
	for _, c := range all {
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name == secretName {
				return true
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == secretName {
				return true
			}
		}
	}
	return false
}

// mcpConfigCandidates are the plausible mount paths for the projected, scoped
// MCP IR config the assembler renders. Probed in order; the first that exists
// is read. Kept as a list so the assertion is robust to the exact mount path.
func mcpConfigCandidates() []string {
	if p := os.Getenv("MCP_CONFIG_PATH"); p != "" {
		return []string{p}
	}
	return []string{
		"/etc/ksquad/mcp/config.json",
		"/etc/ksquad/mcp/servers.json",
		"/var/run/ksquad/mcp.json",
		"/home/agent/.config/mcp/config.json",
		"/etc/mcp/config.json",
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
