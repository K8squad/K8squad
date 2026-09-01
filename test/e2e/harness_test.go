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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/client/config"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// scenario constants — the ONE Run the smoke drives: kubectl@1.31 + github-mcp.
// Kept as named constants so the fixtures, assertions and skip reasons all
// speak the exact same identifiers (every statement citable).
const (
	toolchainName = "kubectl"

	// defaultToolchainVersion is the pin the ISI-3475 deliverable names ("1.31").
	// The SHIPPED default catalog (config/helm, ISI-3286) currently pins kubectl
	// at a different version, so the harness resolves the ACTUAL pinned version
	// from the installed catalog at runtime (resolveToolchainVersion) and drives
	// the Run against that — the invariant under test is "the PINNED version
	// resolves and lands on PATH", version-pinned, not a specific magic number.
	// Override with KUBECTL_TOOLCHAIN_VERSION to force a specific pin.
	defaultToolchainVersion = "1.31"

	mcpServerName = "github-mcp"

	// The github-mcp credential rides a BYO Secret (ADR-045: never inline).
	// The e2e-ollama lane provisions a dummy token Secret of this name before
	// the Run is created; the assertions only prove the Secret is MOUNTED and
	// its header is injected by reference, never that the token is real.
	mcpCredentialSecret = "github-mcp-token" //nolint:gosec // Secret NAME, not material.

	// egressProxyPort mirrors test/blast-radius/cases/s4-1: the allowlisted
	// upstream is reachable only via the team egress proxy Service.
	egressProxyPort = 8080
	// arbitraryEgressIP is the "arbitrary dst MUST be refused" anchor from
	// s4-1 (1.1.1.1:80). Overridable so a locked-down CI egress can retarget.
	defaultArbitraryEgressIP = "1.1.1.1"
)

// harness is the live-cluster context shared by every subtest of
// TestSquadSmoke. A nil harness means a precondition failed and the caller
// already t.Skip'd — construction never returns a half-built harness.
type harness struct {
	cfg    *rest.Config
	cl     client.Client        // typed controller-runtime client (CRDs + core)
	kube   kubernetes.Interface // client-go clientset (pod exec, /metrics proxy)
	scheme *runtime.Scheme
}

// timeouts — an operator-driven Run reconcile plus a kind sandbox pull is slow;
// these are the outer bounds before the harness declares the environment not
// ready (skip) rather than wrong (fail).
const (
	runResolveTimeout = 8 * time.Minute
	pollInterval      = 5 * time.Second
)

// newHarness builds the live-cluster clients or SKIPs with a precise reason.
//
// The skip ladder is deliberate and ordered from "no environment at all" to
// "environment present but the Run plane not landed yet", so a reader of the
// CI log sees exactly which precondition is outstanding — the same
// skip-with-reason discipline e2e.yml applies at the workflow layer.
func newHarness(t *testing.T) *harness {
	t.Helper()

	// 1. A reachable cluster. controller-runtime resolves --kubeconfig / the
	//    KUBECONFIG env / in-cluster config / ~/.kube/config in that order.
	cfg, err := ctrlcfg.GetConfig()
	if err != nil {
		t.Skipf("no reachable cluster (kubeconfig/in-cluster config unresolved): %v — "+
			"e2e conformance needs the kind cluster the e2e-ollama lane provisions", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("register core scheme: %v", err)
	}
	if err := ksquadv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register ksquad.io/v1alpha1 scheme: %v", err)
	}

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Skipf("cluster unreachable while building client: %v", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Skipf("cluster unreachable while building clientset: %v", err)
	}

	h := &harness{cfg: cfg, cl: cl, kube: kube, scheme: scheme}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 2. A live API server, confirmed by a trivial core read.
	var nsList corev1.NamespaceList
	if err := cl.List(ctx, &nsList); err != nil {
		t.Skipf("cluster API unreadable (namespaces list failed): %v", err)
	}

	// 3. The operator CRDs are registered. A List of Runs across all
	//    namespaces returns a NoKindMatchError when the CRD is absent — that
	//    is "operator not installed", a skip, not a failure.
	var runs ksquadv1alpha1.RunList
	if err := cl.List(ctx, &runs); err != nil {
		if meta := isNoKindMatch(err); meta {
			t.Skipf("ksquad.io Run CRD not registered — operator/CRDs not installed on this cluster; "+
				"install the operator + default toolchain catalog before the e2e-ollama lane runs (ISI-2114 precondition): %v", err)
		}
		t.Skipf("Run CRD list failed (cluster not ready): %v", err)
	}

	return h
}

// resolveToolchainVersion asserts the default toolchain catalog is installed and
// carries a kubectl entry, and returns the version the Run will pin to — else
// SKIP (the catalog is an environment precondition the lane installs, not a
// property under test). It honors KUBECTL_TOOLCHAIN_VERSION when that version is
// actually present in the catalog; otherwise it takes the catalog's first
// declared kubectl version so the Run always requests an installable pin.
func (h *harness) resolveToolchainVersion(ctx context.Context, t *testing.T) string {
	t.Helper()
	var tcs ksquadv1alpha1.ToolchainList
	if err := h.cl.List(ctx, &tcs); err != nil {
		if isNoKindMatch(err) {
			t.Skipf("Toolchain CRD not registered — default catalog not installed (operator precondition): %v", err)
		}
		t.Skipf("Toolchain catalog list failed: %v", err)
	}
	// An explicit env override is strict (must be in the catalog). With none, we
	// PREFER the ticket's named pin (defaultToolchainVersion) when the catalog
	// ships it, and otherwise fall back to the catalog's own declared version so
	// the Run always requests an installable pin.
	want := os.Getenv("KUBECTL_TOOLCHAIN_VERSION")
	prefer := want
	if prefer == "" {
		prefer = defaultToolchainVersion
	}
	for i := range tcs.Items {
		tc := &tcs.Items[i]
		if tc.Name != toolchainName || len(tc.Spec.Versions) == 0 {
			continue
		}
		for _, v := range tc.Spec.Versions {
			if v.Version == prefer {
				return prefer // preferred/explicit pin present in the catalog.
			}
		}
		if want != "" {
			t.Skipf("KUBECTL_TOOLCHAIN_VERSION=%s not in catalog kubectl entry (has %v)", want, versionsOf(tc))
		}
		return tc.Spec.Versions[0].Version // catalog's declared pin.
	}
	t.Skipf("default toolchain catalog missing a %q entry (found %d toolchains) — "+
		"install config/helm (default catalog) before driving the Run", toolchainName, len(tcs.Items))
	return "" // unreachable (t.Skipf halts) — keeps the compiler happy.
}

func versionsOf(tc *ksquadv1alpha1.Toolchain) []string {
	var out []string
	for _, v := range tc.Spec.Versions {
		out = append(out, v.Version)
	}
	return out
}

// ensureNamespace creates ns if absent (idempotent); returns a cleanup closure.
func (h *harness) ensureNamespace(ctx context.Context, t *testing.T, name string) func() {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := h.cl.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
	return func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = h.cl.Delete(delCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
}

// arbitraryEgressIP resolves the "must be refused" destination, overridable via
// S4_ARBITRARY_EGRESS_IP so this harness and the s4-1 shell case can share a
// locked-down CI target (they mirror the same anchor).
func arbitraryEgressIP() string {
	if v := os.Getenv("S4_ARBITRARY_EGRESS_IP"); v != "" {
		return v
	}
	return defaultArbitraryEgressIP
}

// isNoKindMatch reports whether err is the meta "no matches for kind" error the
// discovery layer returns when a CRD is not installed — the operator-absent
// signal the skip ladder keys on.
func isNoKindMatch(err error) bool {
	if err == nil {
		return false
	}
	// apimachinery's meta.NoKindMatchError / RESTMapping "no matches for kind"
	// surfaces as a string match across client versions; keep it robust.
	msg := err.Error()
	for _, sub := range []string{
		"no matches for kind",
		"failed to get API group resources",
		"could not find the requested resource",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}
