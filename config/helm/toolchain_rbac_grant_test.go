// Guard tests for the operator toolchain-RBAC grant (ISI-5291, the out-of-band
// gap ProxOps flagged on ISI-5230). When a catalog Toolchain declares `rbac`,
// the operator renders a per-Run Role/ClusterRole in the tenant namespace that
// grants those rules to the agent SA. Kubernetes privilege-escalation prevention
// forbids a subject from creating an RBAC object granting verbs it does not
// itself hold, so the chart must bind the ksquad-operator SA a ClusterRole
// carrying every RBAC-bearing toolchain's rules — otherwise the kubectl toolchain
// silently fails to land its namespace RBAC on a fresh install / helm upgrade.
//
// These tests prove: (1) nothing leaks by default, (2) catalog-on renders the
// kubectl grant bound to the operator SA, (3) the grant's rules stay in lockstep
// with the kubectl Toolchain's declared rbac rules, and (4) the grant needs the
// control plane (its only subject is the operator SA).
//
// Skips (never fails) when the `helm` binary is absent, matching this repo's
// skip-with-reason convention for tool-gated lanes.
package helm

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const kubectlGrantName = "ksquad-operator-toolchain-kubectl-grant"

// renderCatalog runs `helm template` with the control plane on and the default
// toolchain catalog enabled, plus any extra flags the caller appends.
func renderCatalog(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm binary not on PATH; skipping chart-render guard")
	}
	base := []string{"template", "t", ".",
		// modelConfig.default.model is required for install (ISI-4460); supply a
		// dummy so the render exercises the catalog grant, not the required-value guard.
		"--set", "modelConfig.default.model=test-default",
		"--set", "controlPlane.enabled=true",
		"--set", "controlPlane.database.dsn=postgres://u@h/db",
		"--set", "controlPlane.nats.enabled=false"}
	out, err := exec.Command("helm", append(base, args...)...).CombinedOutput()
	return string(out), err
}

// renderedDoc is the subset of a rendered manifest the grant guards inspect.
type renderedDoc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Rules    []rbacv1.PolicyRule `json:"rules"`    // ClusterRole
	Subjects []rbacv1.Subject    `json:"subjects"` // ClusterRoleBinding
	RoleRef  rbacv1.RoleRef      `json:"roleRef"`  // ClusterRoleBinding
	Spec     struct {
		RBAC struct {
			Rules []rbacv1.PolicyRule `json:"rules"`
		} `json:"rbac"`
	} `json:"spec"` // Toolchain
}

// parseDocs splits a rendered multi-document stream into renderedDoc values,
// skipping comment-only / empty documents that fail to unmarshal cleanly.
func parseDocs(t *testing.T, out string) []renderedDoc {
	t.Helper()
	var docs []renderedDoc
	for _, raw := range strings.Split(out, "\n---") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var d renderedDoc
		if err := yaml.Unmarshal([]byte(raw), &d); err != nil {
			continue // comment-only separators etc.
		}
		docs = append(docs, d)
	}
	return docs
}

func docByKindName(docs []renderedDoc, kind, name string) *renderedDoc {
	for i := range docs {
		if docs[i].Kind == kind && docs[i].Metadata.Name == name {
			return &docs[i]
		}
	}
	return nil
}

// Default install (catalog off) must not render any toolchain grant — the
// operator holds no extra tenant read verbs unless a catalog toolchain needs them.
func TestToolchainGrantDefaultOff(t *testing.T) {
	out, err := renderCatalog(t)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "toolchain-kubectl-grant") || strings.Contains(out, "ksquad.io/toolchain-grant") {
		t.Errorf("default-off render leaked a toolchain grant — it must render only with the catalog enabled:\n%s", out)
	}
}

// Catalog-on: the kubectl grant renders as a ClusterRole bound to the operator
// SA, and its rules are an exact (order-insensitive) copy of the kubectl
// Toolchain's declared rbac rules — the lockstep that keeps the escalation grant
// in step with what the operator actually tries to grant per Run.
func TestToolchainKubectlGrantMatchesToolchainRBAC(t *testing.T) {
	out, err := renderCatalog(t, "--set", "tools.defaultCatalog.enabled=true")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	docs := parseDocs(t, out)

	grant := docByKindName(docs, "ClusterRole", kubectlGrantName)
	if grant == nil {
		t.Fatalf("expected ClusterRole %q when the catalog is enabled; not found:\n%s", kubectlGrantName, out)
	}
	if len(grant.Rules) == 0 {
		t.Fatalf("grant ClusterRole %q has no rules", kubectlGrantName)
	}

	tc := docByKindName(docs, "Toolchain", "kubectl")
	if tc == nil {
		t.Fatalf("expected the kubectl Toolchain to render with the catalog enabled")
	}
	if len(tc.Spec.RBAC.Rules) == 0 {
		t.Fatalf("kubectl Toolchain declares no rbac rules — test seed drifted")
	}

	// normalize() lives in rbac_lockstep_test.go (same package): order-insensitive
	// compare so a reordering in values.yaml is not a false failure.
	if want, got := normalize(tc.Spec.RBAC.Rules), normalize(grant.Rules); !reflect.DeepEqual(want, got) {
		t.Fatalf("grant rules drift: ClusterRole %q does not match the kubectl Toolchain rbac.\n"+
			"The operator must hold EXACTLY what it grants per Run.\ntoolchain: %+v\n\ngrant: %+v",
			kubectlGrantName, want, got)
	}

	binding := docByKindName(docs, "ClusterRoleBinding", kubectlGrantName)
	if binding == nil {
		t.Fatalf("expected ClusterRoleBinding %q; not found", kubectlGrantName)
	}
	if binding.RoleRef.Name != kubectlGrantName || binding.RoleRef.Kind != "ClusterRole" {
		t.Errorf("binding roleRef = %+v; want ClusterRole/%s", binding.RoleRef, kubectlGrantName)
	}
	if len(binding.Subjects) != 1 ||
		binding.Subjects[0].Kind != "ServiceAccount" ||
		binding.Subjects[0].Name != "ksquad-operator" {
		t.Errorf("binding must grant exactly the ksquad-operator ServiceAccount; got %+v", binding.Subjects)
	}
}

// The grant needs the control plane: its only subject is the operator SA, which
// the control plane owns. Catalog-on + controlPlane-off must render no grant
// (an orphan binding to a missing SA is worse than none).
func TestToolchainGrantNeedsControlPlane(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm binary not on PATH; skipping chart-render guard")
	}
	out, err := exec.Command("helm", "template", "t", ".",
		"--set", "modelConfig.default.model=test-default",
		"--set", "controlPlane.enabled=false",
		"--set", "tools.defaultCatalog.enabled=true").CombinedOutput()
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, string(out))
	}
	if strings.Contains(string(out), "toolchain-kubectl-grant") {
		t.Errorf("grant rendered without the control plane — it must be gated on controlPlane.enabled:\n%s", string(out))
	}
}
