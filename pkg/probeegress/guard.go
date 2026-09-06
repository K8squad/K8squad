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

// Package probeegress constrains the E3-S2 credential test-connection
// probe's BYO-endpoint egress (ISI-3891, the ISI-3887 N1 advisory).
//
// The apiserver runs in the control-plane namespace with high-trust
// egress; a BYO endpoint Secret lets the caller choose the probe target
// URL. Without a guard the apiserver acts as a confused deputy: an
// authenticated tenant can fold the reachability of control-plane
// internals (Postgres, node IPs, the metadata service) into the probe's
// {ok, detail} status classes — destinations a Run in that squad could
// never reach behind its §12.2 default-deny NetworkPolicy baseline.
//
// The fix is the egress story's own rule, applied at the probe seam:
// test-connection may only reach what a Run could reach. A Run's egress
// is the squad's DECLARED allowlist — the EgressPolicy CRs that live in
// the squad namespace plus any EgressPolicy a Project in the namespace
// references via spec.egressPolicyRef (§12.2, story 4.6; the
// materialization into NetworkPolicies rides the same declaration, so
// the two layers cannot disagree once it lands). The guard resolves the
// BYO URL's host to IPs and requires every resolved address to fall
// inside an allowed CIDR on the URL's port.
//
// Deliberate fail-closed corners, all honest reds for the caller:
//
//   - No EgressPolicy applies to the squad → every BYO target is denied.
//     A green on an endpoint the squad cannot actually reach would be a
//     false green for every Run that uses the credential.
//   - A policy that routes via Proxy, or allows only
//     namespaceSelector-destined rules, cannot be reproduced from an IP
//     at probe time → denied (the probe never rides the proxy).
//   - A hostname that does not resolve → denied (cannot be verified).
//
// Hard-blocked address classes (loopback, link-local incl. the cloud
// metadata service, unspecified, multicast) are denied even when an
// allowlist entry covers them: the apiserver must never attach a
// caller's credential to a link-local or loopback listener.
package probeegress

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// IPResolver is the DNS seam (net.Resolver satisfies it). Tests inject a
// static mapping so the suite never dials a real resolver.
type IPResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// defaultIPResolver adapts net.DefaultResolver to IPResolver.
type defaultIPResolver struct{}

func (defaultIPResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// Guard evaluates one BYO probe target against the squad's declared
// egress allowlist. Construct with the same client the probe path uses
// (a client.Reader); Resolver is optional and defaults to the process
// resolver.
type Guard struct {
	Reader   client.Reader
	Resolver IPResolver
}

// DenialKind classifies why a target was refused. The kind is for
// server-side logs and tests; the API response stays a single curated
// string (NFR-2 discipline — the denial never echoes the URL or IP).
type DenialKind string

const (
	// DenyMalformedURL: not a usable http(s) URL with a host.
	DenyMalformedURL DenialKind = "malformed-url"
	// DenyHardBlock: loopback / link-local / unspecified / multicast —
	// never probeable, regardless of the allowlist.
	DenyHardBlock DenialKind = "hard-block"
	// DenyUnresolvable: the host did not resolve; unverifiable is red.
	DenyUnresolvable DenialKind = "unresolvable"
	// DenyNoPolicy: no EgressPolicy applies to the squad namespace.
	DenyNoPolicy DenialKind = "no-policy"
	// DenyProxyOnly: policies exist but express only proxy /
	// namespaceSelector routes the probe cannot reproduce.
	DenyProxyOnly DenialKind = "proxy-or-selector-only"
	// DenyNotCovered: no allow rule covers every resolved address/port.
	DenyNotCovered DenialKind = "not-covered"
	// DenyPort: an address is covered by CIDR but not on the URL's port.
	DenyPort DenialKind = "port"
)

// Denial is a guard refusal. It is the caller's "no", never an error:
// infra failures surface as err from Allow instead.
type Denial struct {
	Kind   DenialKind
	Detail string
}

func (d *Denial) Error() string {
	return fmt.Sprintf("probeegress: %s: %s", d.Kind, d.Detail)
}

// Allow answers whether the apiserver may dial rawURL on behalf of the
// squad namespace. A nil Denial means allowed. A non-nil error means the
// allowlist itself could not be read (kube API failure) — map it to a
// 502-class response, not a red.
func (g *Guard) Allow(ctx context.Context, namespace, rawURL string) (*Denial, error) {
	host, port, d := parseTarget(rawURL)
	if d != nil {
		return d, nil
	}

	// The allowlist is consulted BEFORE resolution: a squad with no CIDR
	// rules has already answered, and the denial that names the missing
	// declaration is the useful one (no DNS on a foregone conclusion).
	rules, sawProxyOrSelector, err := g.allowlist(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		if sawProxyOrSelector {
			return &Denial{Kind: DenyProxyOnly, Detail: "the squad's egress is declared via a proxy or namespace selector, which test-connection cannot reproduce"}, nil
		}
		return &Denial{Kind: DenyNoPolicy, Detail: "no EgressPolicy applies to this squad; declare the endpoint in one"}, nil
	}

	ips, d := g.resolve(ctx, host)
	if d != nil {
		return d, nil
	}
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		// Unmap so a 4-in-6 form compares against IPv4 prefixes.
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			return &Denial{Kind: DenyUnresolvable, Detail: "unusable address in resolution result"}, nil
		}
		addrs = append(addrs, a.Unmap())
	}
	for _, a := range addrs {
		if hardBlocked(a) {
			return &Denial{Kind: DenyHardBlock, Detail: "loopback, link-local, unspecified and multicast addresses are never probeable"}, nil
		}
	}

	// Every resolved address must be covered on the probe port. A
	// multi-A record where one address slips the allowlist is a denied
	// probe (the half-open answer would be a lie either way).
	for _, a := range addrs {
		covered, portOK := matchRules(rules, a, port)
		if !covered {
			return &Denial{Kind: DenyNotCovered, Detail: "no allow rule covers every resolved address"}, nil
		}
		if !portOK {
			return &Denial{Kind: DenyPort, Detail: fmt.Sprintf("an allow rule covers the address but not port %d", port)}, nil
		}
	}
	return nil, nil
}

// parseTarget extracts host and port from a BYO URL, defaulting the port
// by scheme. byoEndpointURL has already validated the http(s) prefix,
// but the guard is fail-closed on its own parsing too.
func parseTarget(rawURL string) (string, int, *Denial) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", 0, &Denial{Kind: DenyMalformedURL, Detail: "not a usable http(s) URL"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", 0, &Denial{Kind: DenyMalformedURL, Detail: "scheme is not http(s)"}
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, &Denial{Kind: DenyMalformedURL, Detail: "port is not a valid port number"}
		}
		port = n
	}
	return u.Hostname(), port, nil
}

// resolve turns the host into the full set of addresses the apiserver
// would dial. Bracketed IPv6 literals and bare IPv4 literals skip DNS.
func (g *Guard) resolve(ctx context.Context, host string) ([]net.IP, *Denial) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	r := g.Resolver
	if r == nil {
		r = defaultIPResolver{}
	}
	ipAddrs, err := r.LookupIPAddr(ctx, host)
	if err != nil || len(ipAddrs) == 0 {
		return nil, &Denial{Kind: DenyUnresolvable, Detail: "host did not resolve; an unverifiable destination is a red, not a maybe"}
	}
	ips := make([]net.IP, 0, len(ipAddrs))
	for _, a := range ipAddrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// allowRule is one flattened CIDR entry: prefix, carve-outs, and the
// port set (empty = all ports).
type allowRule struct {
	prefix netip.Prefix
	except []netip.Prefix
	ports  []int // empty = all
}

// allowlist gathers the squad's declared egress destinations: every
// EgressPolicy in the namespace, plus every EgressPolicy a Project in
// the namespace references (spec.egressPolicyRef, default namespace =
// the project's own). CIDR rules flatten into allowRules; proxy and
// namespaceSelector shapes are remembered so the denial can name them.
func (g *Guard) allowlist(ctx context.Context, namespace string) ([]allowRule, bool, error) {
	policies, err := g.namespacePolicies(ctx, namespace)
	if err != nil {
		return nil, false, err
	}

	sawProxyOrSelector := false
	seen := map[string]bool{}
	var rules []allowRule
	add := func(p *ksquadv1.EgressPolicy) {
		if seen[p.Namespace+"/"+p.Name] {
			return
		}
		seen[p.Namespace+"/"+p.Name] = true
		if p.Spec.Proxy != nil {
			sawProxyOrSelector = true
		}
		for _, r := range p.Spec.Allow {
			if r.To.CIDR == "" {
				if r.To.NamespaceSelector != nil {
					sawProxyOrSelector = true
				}
				continue
			}
			prefix, err := netip.ParsePrefix(r.To.CIDR)
			if err != nil {
				continue // a malformed declaration is not an allow
			}
			ar := allowRule{prefix: prefix}
			for _, ex := range r.To.Except {
				if ep, err := netip.ParsePrefix(ex); err == nil {
					ar.except = append(ar.except, ep)
				}
			}
			for _, ep := range r.Ports {
				if ep.Protocol != "" && ep.Protocol != "TCP" && ep.Protocol != "tcp" {
					continue // the probe is TCP-only
				}
				if n, err := strconv.Atoi(ep.Port); err == nil {
					ar.ports = append(ar.ports, n)
					continue
				}
				if n, err := net.LookupPort("tcp", ep.Port); err == nil {
					ar.ports = append(ar.ports, n)
				}
			}
			rules = append(rules, ar)
		}
	}

	for i := range policies {
		add(&policies[i])
	}

	var projects ksquadv1.ProjectList
	if err := g.Reader.List(ctx, &projects, client.InNamespace(namespace)); err != nil {
		return nil, false, fmt.Errorf("list projects in %s: %w", namespace, err)
	}
	for i := range projects.Items {
		ref := projects.Items[i].Spec.EgressPolicyRef
		if ref == nil || ref.Name == "" {
			continue
		}
		ns := ref.Namespace
		if ns == "" {
			ns = namespace
		}
		var p ksquadv1.EgressPolicy
		if err := g.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &p); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue // dangling ref: contributes nothing
			}
			return nil, false, fmt.Errorf("read egresspolicy %s/%s: %w", ns, ref.Name, err)
		}
		add(&p)
	}
	return rules, sawProxyOrSelector, nil
}

// namespacePolicies lists the EgressPolicies living in the squad
// namespace itself.
func (g *Guard) namespacePolicies(ctx context.Context, namespace string) ([]ksquadv1.EgressPolicy, error) {
	var list ksquadv1.EgressPolicyList
	if err := g.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list egresspolicies in %s: %w", namespace, err)
	}
	return list.Items, nil
}

// matchRules reports whether one address is inside an allow rule
// (respecting except carve-outs), and whether that same rule also
// admits the port.
func matchRules(rules []allowRule, a netip.Addr, port int) (covered, portOK bool) {
	for _, r := range rules {
		if !r.prefix.Contains(a) {
			continue
		}
		carved := false
		for _, ex := range r.except {
			if ex.Contains(a) {
				carved = true
				break
			}
		}
		if carved {
			continue
		}
		if len(r.ports) == 0 {
			return true, true
		}
		for _, p := range r.ports {
			if p == port {
				return true, true
			}
		}
		return true, false // covered by CIDR, wrong port
	}
	return false, false
}

// hardBlocked names the address classes the apiserver must never probe,
// allowlist or not.
func hardBlocked(a netip.Addr) bool {
	return a.IsLoopback() ||
		a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() ||
		a.IsMulticast() ||
		a.IsUnspecified()
}
