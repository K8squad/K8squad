// Package helm holds guard tests for the control-plane chart. The operator
// ClusterRole in templates/control-plane/rbac.yaml is a hand-maintained copy of
// config/rbac/role.yaml (the controller-gen +kubebuilder marker output). This
// test fails the build if the two diverge, so a future `make manifests` change
// to role.yaml can't silently leave the Helm-installed grant behind (ISI-3494 L4).
package helm

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

type ruleDoc struct {
	Rules []rbacv1.PolicyRule `json:"rules"`
}

// normalize sorts the rule list and every string slice inside each rule so the
// comparison is order-insensitive (controller-gen and the hand copy may list
// apiGroups/resources/verbs in different orders without any real divergence).
func normalize(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	out := make([]rbacv1.PolicyRule, len(rules))
	copy(out, rules)
	for i := range out {
		sort.Strings(out[i].APIGroups)
		sort.Strings(out[i].Resources)
		sort.Strings(out[i].Verbs)
		sort.Strings(out[i].ResourceNames)
		sort.Strings(out[i].NonResourceURLs)
	}
	sort.Slice(out, func(a, b int) bool {
		return strings.Join(out[a].APIGroups, ",")+"|"+strings.Join(out[a].Resources, ",") <
			strings.Join(out[b].APIGroups, ",")+"|"+strings.Join(out[b].Resources, ",")
	})
	return out
}

// operatorRulesFromChart slices the first `rules:` block (the -operator
// ClusterRole, the first document) out of the chart template. The rules block is
// plain YAML — the Helm templating in rbac.yaml only appears in metadata — so it
// unmarshals cleanly on its own without rendering the chart.
func operatorRulesFromChart(t *testing.T, chart string) []rbacv1.PolicyRule {
	t.Helper()
	lines := strings.Split(chart, "\n")
	start := -1
	for i, l := range lines {
		if l == "rules:" { // column-0 `rules:` = the operator ClusterRole's block
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no top-level `rules:` block found in rbac.yaml")
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "---") {
			end = i
			break
		}
	}
	var doc ruleDoc
	if err := yaml.Unmarshal([]byte(strings.Join(lines[start:end], "\n")), &doc); err != nil {
		t.Fatalf("parse operator rules from chart: %v", err)
	}
	return doc.Rules
}

func TestOperatorClusterRoleMatchesGeneratedRole(t *testing.T) {
	roleYAML, err := os.ReadFile("../rbac/role.yaml")
	if err != nil {
		t.Fatalf("read config/rbac/role.yaml: %v", err)
	}
	var gen ruleDoc
	if err := yaml.Unmarshal(roleYAML, &gen); err != nil {
		t.Fatalf("parse config/rbac/role.yaml: %v", err)
	}

	chartYAML, err := os.ReadFile("templates/control-plane/rbac.yaml")
	if err != nil {
		t.Fatalf("read chart rbac.yaml: %v", err)
	}
	chartRules := operatorRulesFromChart(t, string(chartYAML))

	if len(gen.Rules) == 0 {
		t.Fatal("config/rbac/role.yaml has no rules — generation likely broken")
	}
	if want, got := normalize(gen.Rules), normalize(chartRules); !reflect.DeepEqual(want, got) {
		t.Fatalf("operator ClusterRole drift: chart rbac.yaml does not match config/rbac/role.yaml.\n"+
			"Re-run `make manifests` and mirror the rules into config/helm/templates/control-plane/rbac.yaml.\n"+
			"generated (role.yaml):\n%+v\n\nchart (rbac.yaml):\n%+v", want, got)
	}
}

// chartDoc is one `---`-delimited document from the (still-templated) chart,
// with its kind and metadata.name pulled out. metaName is anchored to the first
// `  name:` line under `metadata:` — NOT any substring of the document — so a
// role's explanatory comment or a roleRef that mentions a name can't be mistaken
// for the object's own identity.
type chartDoc struct {
	kind     string
	metaName string
	body     string
}

// splitChartDocs parses the chart into per-document (kind, metadata.name, body)
// tuples. metadata.name stays Helm-templated (`{{ include ... }}-<suffix>`), so
// callers match on its suffix; the body is used for the plain-YAML rules block.
func splitChartDocs(chart string) []chartDoc {
	var docs []chartDoc
	for _, body := range strings.Split(chart, "\n---") {
		var d chartDoc
		d.body = body
		inMeta := false
		for _, l := range strings.Split(body, "\n") {
			switch {
			case strings.HasPrefix(l, "kind: "):
				d.kind = strings.TrimSpace(strings.TrimPrefix(l, "kind:"))
			case l == "metadata:":
				inMeta = true
			case inMeta && strings.HasPrefix(l, "  name:") && d.metaName == "":
				d.metaName = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "name:"))
				inMeta = false // first name under metadata is the object's own
			}
		}
		docs = append(docs, d)
	}
	return docs
}

// clusterRoleRulesByName slices the `rules:` block out of the ClusterRole
// document whose metadata.name ends in the given suffix. The rules block is
// plain YAML — templating only appears in metadata — so it unmarshals on its own
// without rendering the chart. Selection is anchored to kind + metadata.name, so
// a rename of the object (leaving stale comment/roleRef text) fails the lookup
// instead of silently matching the wrong document.
func clusterRoleRulesByName(t *testing.T, chart, suffix string) []rbacv1.PolicyRule {
	t.Helper()
	for _, d := range splitChartDocs(chart) {
		if d.kind != "ClusterRole" || !strings.HasSuffix(d.metaName, "-"+suffix) {
			continue
		}
		lines := strings.Split(d.body, "\n")
		start := -1
		for i, l := range lines {
			if l == "rules:" {
				start = i
				break
			}
		}
		if start < 0 {
			t.Fatalf("ClusterRole %q has no top-level `rules:` block", suffix)
		}
		var parsed ruleDoc
		if err := yaml.Unmarshal([]byte(strings.Join(lines[start:], "\n")), &parsed); err != nil {
			t.Fatalf("parse %q rules: %v", suffix, err)
		}
		return parsed.Rules
	}
	t.Fatalf("no ClusterRole with metadata.name ending %q found in rbac.yaml", "-"+suffix)
	return nil
}

// TestApiserverClusterRoleLeastPrivilege pins the apiserver compose-write grant
// to exactly the verbs the code exercises. Without this guard the apiserver
// ClusterRole, its verbs, or a resource could silently regress — the existing
// lockstep test only inspects the first (operator) rules block — and re-open the
// kube-layer 403 this change (ISI-3550) fixed. The DIRECT client (client.New)
// does get+create+update on every compose kind and list only on teams; no
// watch (no informers), no patch, no delete. Update this expectation only when
// the compose client's verb usage actually changes (internal/apiserver/composecrd.go).
func TestApiserverClusterRoleLeastPrivilege(t *testing.T) {
	chartYAML, err := os.ReadFile("templates/control-plane/rbac.yaml")
	if err != nil {
		t.Fatalf("read chart rbac.yaml: %v", err)
	}
	got := clusterRoleRulesByName(t, string(chartYAML), "apiserver")

	want := []rbacv1.PolicyRule{
		{APIGroups: []string{"ksquad.io"}, Resources: []string{"teams"}, Verbs: []string{"get", "list", "create", "update"}},
		{APIGroups: []string{"ksquad.io"}, Resources: []string{"projects", "agents", "roles", "skills"}, Verbs: []string{"get", "create", "update"}},
	}
	if w, g := normalize(want), normalize(got); !reflect.DeepEqual(w, g) {
		t.Fatalf("apiserver ClusterRole drift: chart rbac.yaml grant is not the least-privilege set.\n"+
			"If the compose client's verb usage changed, update both the chart and this test in lockstep.\n"+
			"want:\n%+v\n\ngot:\n%+v", w, g)
	}

	// The grant is useless without the binding to the apiserver ServiceAccount.
	// Inspect the ONE apiserver ClusterRoleBinding document (anchored to its own
	// metadata.name) and verify inside that same document that its roleRef points
	// at the apiserver ClusterRole AND its subject is the ksquad-apiserver SA — so
	// a deleted apiserver binding can't be masked by an unrelated binding (e.g.
	// scm-webhook) elsewhere in the file satisfying the checks independently.
	assertApiserverBinding(t, string(chartYAML))
}

// assertApiserverBinding finds the ClusterRoleBinding whose metadata.name ends
// in "-apiserver" and, within that single document, requires a roleRef to a
// ClusterRole named "-apiserver" and a ServiceAccount subject "ksquad-apiserver".
func assertApiserverBinding(t *testing.T, chart string) {
	t.Helper()
	for _, d := range splitChartDocs(chart) {
		if d.kind != "ClusterRoleBinding" || !strings.HasSuffix(d.metaName, "-apiserver") {
			continue
		}
		// roleRef block: kind ClusterRole + name suffix -apiserver.
		roleRef := sectionAfter(d.body, "roleRef:")
		if !strings.Contains(roleRef, "kind: ClusterRole") ||
			!nameLineHasSuffix(roleRef, "-apiserver") {
			t.Fatalf("apiserver ClusterRoleBinding roleRef does not target the -apiserver ClusterRole:\n%s", roleRef)
		}
		// subjects block: a ServiceAccount named ksquad-apiserver.
		subjects := sectionAfter(d.body, "subjects:")
		if !strings.Contains(subjects, "kind: ServiceAccount") ||
			!strings.Contains(subjects, "name: ksquad-apiserver") {
			t.Fatalf("apiserver ClusterRoleBinding subject is not ServiceAccount ksquad-apiserver:\n%s", subjects)
		}
		return
	}
	t.Fatal("no ClusterRoleBinding with metadata.name ending -apiserver found in rbac.yaml")
}

// sectionAfter returns the lines from `header` up to (but excluding) the next
// top-level (column-0) key, i.e. the value block belonging to that key.
func sectionAfter(doc, header string) string {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if l == header {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "-") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// nameLineHasSuffix reports whether any `name:` line in the block has a value
// ending in the given suffix (the value stays Helm-templated).
func nameLineHasSuffix(block, suffix string) bool {
	for _, l := range strings.Split(block, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "name:") &&
			strings.HasSuffix(strings.TrimSpace(strings.TrimPrefix(l, "name:")), suffix) {
			return true
		}
	}
	return false
}
