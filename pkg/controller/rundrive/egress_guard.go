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

package rundrive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// egress_guard.go — the ISI-7705 dispatch-time, endpoint-aware egress guard.
//
// PR #1062 (ISI-7691) made the proxy-less-sandbox condition LOUD at boot and
// warm bind, but boot/bind is the wrong seam for an ENDPOINT-DEPENDENT
// condition: whether a proxy-less sandbox is harmful depends on the resolved
// model endpoint (an external BYO FQDN behind the team-namespace egress
// NetworkPolicy hangs ~134s on TCP SYN retries → a2a follow_error →
// zombie_no_sandbox, ISI-7686; an in-cluster endpoint or LAN Ollama
// 10.0.0.185 is a supported proxy-less config). buildTask's
// resolveEffectiveEndpoint is the single seam that sees the endpoint for
// EVERY run — cold-boot AND warm-bind (Pool.Bind never cold-boots) — so the
// guard lives there, right after the endpoint resolves and before the
// EndpointGate slot is taken.
//
// Fail condition (all three must hold):
//  1. the resolved endpoint host is an EXTERNAL FQDN (modelendpoint.
//     ClassifyEndpointURL — the same classification the team controller's BYO
//     netpol logic uses; an IP or in-cluster svc DNS name is netpol-
//     matchable and never trips the guard),
//  2. the sandbox carries no EFFECTIVE forward proxy for that host — no
//     non-blank KSQUAD_SANDBOX_HTTPS_PROXY/HTTP_PROXY, or NO_PROXY covers the
//     host (NO_PROXY=* bypasses every host and counts as unproxied, mirroring
//     warmpool envHasForwardProxy, ISI-7691 review H1),
//  3. the run's namespace carries an egress NetworkPolicy (the team
//     default-deny posture — same probe the e2e smoke uses).
//
// On a trip the guard returns *EgressBlockedError (wrapping
// errSandboxEgressBlocked); the driver classifies it with IsEgressBlocked and
// fails the run fast with a durable, non-null Run condition + audit rows
// (egressBlockedFail) instead of the 134s hang.

// errSandboxEgressBlocked is the sentinel the dispatch-time egress guard wraps
// (ISI-7705). errors.Is walks the wrap chain, so it survives the a2a submit →
// effects wrapping back to the driver, exactly like the contextasm fail-closed
// sentinels (ISI-5543).
var errSandboxEgressBlocked = errors.New("rundrive: sandbox egress to external BYO model endpoint blocked (proxy-less sandbox behind egress NetworkPolicy)")

// EgressBlockedError is the typed fail-fast cause the guard raises when an
// external-FQDN BYO endpoint is unreachable from a proxy-less sandbox in an
// egress-policed namespace. It carries the evidence for the Run condition +
// audit row: the resolved endpoint, its host, and the run namespace. The
// endpoint BaseURL is safe to record — resolution validated http(s)+host and
// never embeds the token.
type EgressBlockedError struct {
	Endpoint  string // resolved BaseURL that will not be reachable
	Host      string // external FQDN host of Endpoint
	Namespace string // run namespace carrying the egress NetworkPolicy
}

func (e *EgressBlockedError) Error() string {
	return fmt.Sprintf("rundrive: external BYO model endpoint %q (host %q) is unreachable from the proxy-less sandbox in namespace %s: no effective KSQUAD_SANDBOX_*_PROXY routes the host through the egress proxy and the namespace carries an egress NetworkPolicy — failing fast at dispatch instead of the ~134s TCP SYN-retry hang (ISI-7686/ISI-7691/ISI-7705)",
		e.Endpoint, e.Host, e.Namespace)
}

// Unwrap returns the sentinel so IsEgressBlocked (errors.Is) classifies the
// error through any wrap chain.
func (e *EgressBlockedError) Unwrap() error { return errSandboxEgressBlocked }

// IsEgressBlocked reports whether err is the dispatch-time egress guard's
// deterministic fail-fast (ISI-7705): an external BYO FQDN endpoint behind an
// egress-policed namespace with no effective sandbox proxy. Like
// contextasm.IsFailClosed it is the single classifier the driver's terminal
// path keys on; every other failure (a transient read, a sandbox race) must
// NOT match.
func IsEgressBlocked(err error) bool { return errors.Is(err, errSandboxEgressBlocked) }

// guardDispatchEgress is the ISI-7705 check called from buildTask with the
// EFFECTIVE endpoint (after the 5.11 fallback switch) before the endpoint
// gate slot is taken. It returns nil for every healthy configuration —
// in-cluster/LAN endpoints, proxied sandboxes, NO_PROXY-covered hosts,
// proxy-less dev namespaces — and *EgressBlockedError only for the harmful
// triple (external FQDN + no effective proxy for the host + egress policy).
func (d *operatorDispatch) guardDispatchEgress(ctx context.Context, run *api.Run, baseURL string) error {
	host, _, _, kind := modelendpoint.ClassifyEndpointURL(baseURL)
	if kind != modelendpoint.HostExternalFQDN {
		// IP (LAN Ollama / public IP → ipBlock rule) or in-cluster Service
		// DNS (namespace peer): netpol-matchable, and a supported proxy-less
		// config per deploy/helm/ksquad/values.yaml — never trips the guard.
		return nil
	}
	if proxyCoversHost(d.sandboxProxyEnv(), host) {
		return nil // the sandbox routes this host through the forward proxy
	}
	policed, err := namespaceEgressPoliced(ctx, d.cfg.Client, run.Namespace)
	if err != nil {
		// Best-effort, mirroring the team netpol resolver (byoModelTargets):
		// a transient read failure must not fail runs — the level-triggered
		// next pass re-evaluates with a warm cache.
		slog.WarnContext(ctx, "rundrive: could not list NetworkPolicies for the dispatch egress guard; skipping fail-fast this pass (ISI-7705)",
			"run.namespace", run.Namespace, "host", host, "err", err.Error())
		return nil
	}
	if !policed {
		return nil // proxy-less dev cluster — direct egress legitimate (ISI-5477)
	}
	return &EgressBlockedError{Endpoint: baseURL, Host: host, Namespace: run.Namespace}
}

// sandboxProxyEnv returns the KSQUAD_SANDBOX_*_PROXY env the operator stamps
// on every sandbox pod (the cmd/operator/main.go ISI-5477 pass-through of the
// operator's own env, which is also what the warm pool's podEnv carries — so
// cold-boot and warm-bind pods see the same values). Configurable for tests;
// nil reads the namespaced vars (plus the conventional NO_PROXY) from the
// operator process env, the exact source the warmpool stamps from.
func (d *operatorDispatch) sandboxProxyEnv() map[string]string {
	if d.cfg.SandboxProxyEnv != nil {
		return d.cfg.SandboxProxyEnv()
	}
	return sandboxProxyEnvFromOS()
}

// sandboxProxyEnvNames are the env names the egress guard consults.
var sandboxProxyEnvNames = []string{
	"KSQUAD_SANDBOX_HTTPS_PROXY",
	"KSQUAD_SANDBOX_HTTP_PROXY",
	"KSQUAD_SANDBOX_NO_PROXY",
	"NO_PROXY",
}

// sandboxProxyEnvFromOS reads the proxy env from the operator process env.
func sandboxProxyEnvFromOS() map[string]string {
	m := map[string]string{}
	for _, name := range sandboxProxyEnvNames {
		if v, ok := os.LookupEnv(name); ok {
			m[name] = v
		}
	}
	return m
}

// proxyCoversHost reports whether the sandbox proxy env routes host through
// the forward proxy: a non-blank (trimmed) KSQUAD_SANDBOX_HTTPS_PROXY or
// KSQUAD_SANDBOX_HTTP_PROXY must be present AND the host must NOT be bypassed
// by NO_PROXY (KSQUAD_SANDBOX_NO_PROXY or the conventional name). NO_PROXY=*
// bypasses every host, so it counts as unproxied — mirroring warmpool
// envHasForwardProxy (ISI-7691 review H1: a whitespace-only proxy value is
// NOT proxied either).
func proxyCoversHost(env map[string]string, host string) bool {
	proxied := false
	for _, name := range []string{"KSQUAD_SANDBOX_HTTPS_PROXY", "KSQUAD_SANDBOX_HTTP_PROXY"} {
		if strings.TrimSpace(env[name]) != "" {
			proxied = true
		}
	}
	if !proxied {
		return false
	}
	for _, name := range []string{"KSQUAD_SANDBOX_NO_PROXY", "NO_PROXY"} {
		if noProxyBypasses(env[name], host) {
			return false
		}
	}
	return true
}

// noProxyBypasses reports whether a NO_PROXY value bypasses host under the
// standard semantics: "*" matches everything; an entry matches the host
// exactly or as a domain suffix ("example.com" and ".example.com" both match
// "api.example.com"); an entry may carry a port ("host:443") which is dropped
// for the match. Comparison is case-insensitive, entries comma-separated.
func noProxyBypasses(noProxy, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, entry := range strings.Split(noProxy, ",") {
		e := strings.ToLower(strings.TrimSpace(entry))
		if e == "" {
			continue
		}
		if e == "*" {
			return true
		}
		e = strings.TrimPrefix(e, ".")
		if h, _, err := net.SplitHostPort(e); err == nil {
			e = h
		}
		if host == e || strings.HasSuffix(host, "."+e) {
			return true
		}
	}
	return false
}

// namespaceEgressPoliced reports whether ns carries any egress NetworkPolicy
// (PolicyTypes contains Egress, or non-empty egress rules) — the team
// default-deny posture. This is the same probe the e2e smoke's
// sandbox/proxy_env_present_for_egress_policed_run assertion uses, so the
// guard and the conformance harness agree on "egress-policed".
func namespaceEgressPoliced(ctx context.Context, c client.Client, ns string) (bool, error) {
	var nps networkingv1.NetworkPolicyList
	if err := c.List(ctx, &nps, client.InNamespace(ns)); err != nil {
		return false, err
	}
	for i := range nps.Items {
		for _, pt := range nps.Items[i].Spec.PolicyTypes {
			if pt == networkingv1.PolicyTypeEgress {
				return true, nil
			}
		}
		if len(nps.Items[i].Spec.Egress) > 0 {
			return true, nil
		}
	}
	return false, nil
}
