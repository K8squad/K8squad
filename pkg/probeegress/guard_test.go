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

package probeegress

import (
	"context"
	"net"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// runtimeScheme registers the full ksquad API set (EgressPolicy, Project)
// plus corev1 for good measure.
func runtimeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register ksquad scheme: %v", err)
	}
	return scheme
}

// staticResolver answers fixed IPs so the suite never dials DNS.
type staticResolver map[string][]net.IP

func (r staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

const ns = "ksquad-team-alpha"

func cidrPolicy(name string, cidrs ...string) *ksquadv1.EgressPolicy {
	p := &ksquadv1.EgressPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	for _, c := range cidrs {
		p.Spec.Allow = append(p.Spec.Allow, ksquadv1.EgressRule{
			To: ksquadv1.EgressDestination{CIDR: c},
		})
	}
	return p
}

func newGuard(t *testing.T, objs []client.Object, r IPResolver) *Guard {
	t.Helper()
	scheme := runtimeScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Guard{Reader: c, Resolver: r}
}

func runAllow(t *testing.T, g *Guard, rawURL string) *Denial {
	t.Helper()
	d, err := g.Allow(context.Background(), ns, rawURL)
	if err != nil {
		t.Fatalf("Allow(%q): infra error, want a decision: %v", rawURL, err)
	}
	return d
}

func TestAllowLiteralIPAgainstPolicy(t *testing.T) {
	// The 7.5 homelab shape: LAN Ollama, declared by the squad.
	g := newGuard(t, []client.Object{cidrPolicy("lan", "10.0.0.0/8")}, staticResolver{})
	if d := runAllow(t, g, "http://10.0.0.185:11434/v1"); d != nil {
		t.Fatalf("declared LAN endpoint must pass, got %+v", d)
	}
	// Same range on the default http port, and https default port.
	if d := runAllow(t, g, "http://10.1.2.3/x"); d != nil {
		t.Fatalf("declared default-port endpoint must pass, got %+v", d)
	}
	if d := runAllow(t, g, "https://10.1.2.3/x"); d != nil {
		t.Fatalf("declared https endpoint must pass, got %+v", d)
	}
}

func TestDenyWithoutPolicy(t *testing.T) {
	g := newGuard(t, nil, staticResolver{})
	d := runAllow(t, g, "http://10.0.0.185:11434/v1")
	if d == nil || d.Kind != DenyNoPolicy {
		t.Fatalf("undeclared endpoint must fail closed with no-policy, got %+v", d)
	}
}

func TestDenyNotCovered(t *testing.T) {
	g := newGuard(t, []client.Object{cidrPolicy("lan", "10.0.0.0/8")}, staticResolver{})
	d := runAllow(t, g, "http://192.168.1.1:8080/")
	if d == nil || d.Kind != DenyNotCovered {
		t.Fatalf("out-of-policy endpoint must be denied, got %+v", d)
	}
}

func TestDenyControlPlaneStyleTargets(t *testing.T) {
	// The confused-deputy finding: a squad with a generous allowlist must
	// still not probe loopback or the link-local metadata range even when
	// a policy literally covers them.
	g := newGuard(t, []client.Object{cidrPolicy("wide", "0.0.0.0/0")}, staticResolver{})
	for _, u := range []string{"http://127.0.0.1:8080/", "http://169.254.169.254/latest/meta-data/", "http://0.0.0.0:8080/", "http://[::1]:8080/", "http://[fe80::1]:8080/"} {
		if d := runAllow(t, g, u); d == nil || d.Kind != DenyHardBlock {
			t.Fatalf("%s must be hard-blocked, got %+v", u, d)
		}
	}
}

func TestDenyUnresolvable(t *testing.T) {
	g := newGuard(t, []client.Object{cidrPolicy("lan", "10.0.0.0/8")}, staticResolver{})
	d := runAllow(t, g, "http://ollama.lan:11434/")
	if d == nil || d.Kind != DenyUnresolvable {
		t.Fatalf("unresolvable host must fail closed, got %+v", d)
	}
}

func TestHostnameAllAddressesMustBeCovered(t *testing.T) {
	// A multi-A host where one record slips the allowlist is denied —
	// the probe must not reach anything the squad did not declare.
	res := staticResolver{"ollama.svc": {net.ParseIP("10.0.0.5"), net.ParseIP("192.168.0.9")}}
	g := newGuard(t, []client.Object{cidrPolicy("lan", "10.0.0.0/8")}, res)
	if d := runAllow(t, g, "http://ollama.svc:11434/"); d == nil || d.Kind != DenyNotCovered {
		t.Fatalf("split answer must be denied, got %+v", d)
	}

	res2 := staticResolver{"ollama.svc": {net.ParseIP("10.0.0.5"), net.ParseIP("10.0.0.6")}}
	g2 := newGuard(t, []client.Object{cidrPolicy("lan", "10.0.0.0/8")}, res2)
	if d := runAllow(t, g2, "http://ollama.svc:11434/"); d != nil {
		t.Fatalf("fully covered multi-A host must pass, got %+v", d)
	}
}

func TestPortRestrictions(t *testing.T) {
	p := &ksquadv1.EgressPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ports"}}
	p.Spec.Allow = []ksquadv1.EgressRule{{
		To: ksquadv1.EgressDestination{CIDR: "10.0.0.0/8"},
		Ports: []ksquadv1.EgressPort{
			{Port: "11434"},
			{Port: "443"},
		},
	}}
	g := newGuard(t, []client.Object{p}, staticResolver{})
	if d := runAllow(t, g, "http://10.0.0.185:11434/v1"); d != nil {
		t.Fatalf("declared port must pass, got %+v", d)
	}
	if d := runAllow(t, g, "https://10.0.0.185/"); d != nil {
		t.Fatalf("https default port is declared, got %+v", d)
	}
	d := runAllow(t, g, "http://10.0.0.185:8080/")
	if d == nil || d.Kind != DenyPort {
		t.Fatalf("undeclared port must be denied with port kind, got %+v", d)
	}
}

func TestExceptCarveOut(t *testing.T) {
	p := &ksquadv1.EgressPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "carve"}}
	p.Spec.Allow = []ksquadv1.EgressRule{{
		To: ksquadv1.EgressDestination{
			CIDR:   "10.0.0.0/8",
			Except: []string{"10.0.0.185/32"},
		},
	}}
	g := newGuard(t, []client.Object{p}, staticResolver{})
	if d := runAllow(t, g, "http://10.0.0.185:11434/"); d == nil || d.Kind != DenyNotCovered {
		t.Fatalf("carved-out address must be denied, got %+v", d)
	}
	if d := runAllow(t, g, "http://10.0.0.184:11434/"); d != nil {
		t.Fatalf("sibling address must pass, got %+v", d)
	}
}

func TestDenyProxyAndSelectorOnly(t *testing.T) {
	proxy := &ksquadv1.EgressPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "corp"}}
	proxy.Spec.Proxy = &ksquadv1.EgressProxy{Address: "10.0.0.5/32", Port: "8080"}
	g := newGuard(t, []client.Object{proxy}, staticResolver{})
	d := runAllow(t, g, "https://api.example.com/")
	if d == nil || d.Kind != DenyProxyOnly {
		t.Fatalf("proxy-only policy must be denied with proxy kind, got %+v", d)
	}

	sel := &ksquadv1.EgressPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "incluster"}}
	sel.Spec.Allow = []ksquadv1.EgressRule{{
		To: ksquadv1.EgressDestination{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": "ollama"},
		}},
	}}
	g2 := newGuard(t, []client.Object{sel}, staticResolver{})
	d2 := runAllow(t, g2, "http://10.0.0.5:11434/")
	if d2 == nil || d2.Kind != DenyProxyOnly {
		t.Fatalf("selector-only policy must be denied with proxy kind, got %+v", d2)
	}
}

func TestProjectReferencedPolicy(t *testing.T) {
	// A Project in the squad namespace referencing an operator-managed
	// EgressPolicy elsewhere must contribute to the allowlist.
	policy := cidrPolicy("shared-lan", "10.0.0.0/8")
	policy.Namespace = "ksquad-system"
	proj := &ksquadv1.Project{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p1"}}
	proj.Spec.EgressPolicyRef = &ksquadv1.ObjectRef{Name: "shared-lan", Namespace: "ksquad-system"}
	g := newGuard(t, []client.Object{policy, proj}, staticResolver{})
	if d := runAllow(t, g, "http://10.0.0.185:11434/"); d != nil {
		t.Fatalf("project-referenced policy must pass, got %+v", d)
	}

	// Dangling reference contributes nothing (fail-closed, not fail-open).
	proj2 := &ksquadv1.Project{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p2"}}
	proj2.Spec.EgressPolicyRef = &ksquadv1.ObjectRef{Name: "ghost", Namespace: "ksquad-system"}
	g2 := newGuard(t, []client.Object{proj2}, staticResolver{})
	if d := runAllow(t, g2, "http://10.0.0.185:11434/"); d == nil || d.Kind != DenyNoPolicy {
		t.Fatalf("dangling project ref must leave the squad policyless, got %+v", d)
	}
}

func TestMalformedURLs(t *testing.T) {
	g := newGuard(t, []client.Object{cidrPolicy("lan", "10.0.0.0/8")}, staticResolver{})
	for _, u := range []string{"not a url", "ftp://10.0.0.1/x", "http://", "http://10.0.0.1:notaport/"} {
		if d := runAllow(t, g, u); d == nil || d.Kind != DenyMalformedURL {
			t.Fatalf("%q must be malformed, got %+v", u, d)
		}
	}
}
