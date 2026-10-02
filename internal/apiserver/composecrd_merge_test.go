package apiserver

// ISI-5359 acceptance tests — field-scoped merge writes (ISI-5305 §5 Gap 1).
//
// PUT /api/compose/{kind}/{name} used to be a full-spec replace against lossy
// read projections: any live spec field the caller did not (or could not)
// resend was silently wiped on save. These tests pin the AC directly: editing
// ONE field via the API must leave every unmentioned field intact — exercised
// per the issue with a Role, a Skill and a Team that each carry fields the
// partial PUT does not send — plus the RFC 7386 null-delete semantic.
//
// The partial PUTs drive raw map bodies (not the request structs) so the set
// of "sent" top-level fields is exact: whatever the map omits is, by
// construction, a field the caller did not mention.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ── Role: partial PUT keeps every unsent field (AC: role with fields the caller didn't send) ──
//
// Seed a role with model + runtimeClassHint + phase/coordinator config +
// defaultSkills; then PUT ONLY name/project/promptRef (a prompt-version bump —
// the exact "edit one field" flow) and assert the four unsent fields survive.
func TestComposeEditRolePartialPutKeepsUnsentFields(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))

	seed := roleRequest{
		Project:   "widget",
		Name:      "engineer",
		PromptRef: objectRefWire{Name: "engineer-prompt"},
		DefaultSkills: []objectRefWire{
			{Name: "code-review"}, {Name: "tdd"},
		},
		RuntimeClassHint: "kata",
		Model:            "claude-opus-4-8",
		ActivePhases:     []string{"implementation", "code_review"},
		Coordinator:      true,
		CoordinatorMode:  "propose",
	}
	if w := do(svc.handleRole(true), http.MethodPost, "/api/roles",
		caller("bob", teamUID, false), seed, nil); w.Code != http.StatusCreated {
		t.Fatalf("seed role create failed: %d %s", w.Code, w.Body.String())
	}

	// Partial edit: ONLY the promptRef changes. model, runtimeClassHint,
	// activePhases, coordinator, coordinatorMode and defaultSkills are NOT sent.
	partial := map[string]any{
		"project": "widget",
		"name":    "engineer",
		"promptRef": map[string]any{
			"name": "engineer-prompt-v2",
		},
	}
	w := do(svc.handleRole(false), http.MethodPut, "/api/roles/engineer",
		caller("bob", teamUID, false), partial, map[string]string{"name": "engineer"})
	if w.Code != http.StatusOK {
		t.Fatalf("partial role edit want 200, got %d: %s", w.Code, w.Body.String())
	}

	var got ksquadv1.Role
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "engineer"}, &got); err != nil {
		t.Fatalf("get role after partial edit: %v", err)
	}
	if got.Spec.PromptRef.Name != "engineer-prompt-v2" {
		t.Fatalf("sent field promptRef must be updated, got %q", got.Spec.PromptRef.Name)
	}
	if got.Spec.Model != "claude-opus-4-8" {
		t.Errorf("unsent field model wiped: got %q", got.Spec.Model)
	}
	if got.Spec.RuntimeClassHint != "kata" {
		t.Errorf("unsent field runtimeClassHint wiped: got %q", got.Spec.RuntimeClassHint)
	}
	if len(got.Spec.ActivePhases) != 2 || got.Spec.ActivePhases[0] != "implementation" {
		t.Errorf("unsent field activePhases wiped: got %v", got.Spec.ActivePhases)
	}
	if !got.Spec.Coordinator || got.Spec.CoordinatorMode != "propose" {
		t.Errorf("unsent coordinator config wiped: coordinator=%v mode=%q", got.Spec.Coordinator, got.Spec.CoordinatorMode)
	}
	if len(got.Spec.DefaultSkills) != 2 || got.Spec.DefaultSkills[0].Name != "code-review" {
		t.Errorf("unsent field defaultSkills wiped: got %v", got.Spec.DefaultSkills)
	}
}

// ── Role: JSON null deletes exactly one field (RFC 7386 semantics) ────────────
//
// The merge patch null sentinel clears spec.model while the same PUT's other
// unsent fields (defaultSkills) still ride through — delete is scoped, not a
// wholesale replace.
func TestComposeEditRoleNullDeletesSentFieldOnly(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))

	seed := roleRequest{
		Project:   "widget",
		Name:      "reviewer",
		PromptRef: objectRefWire{Name: "reviewer-prompt"},
		DefaultSkills: []objectRefWire{
			{Name: "code-review"},
		},
		Model: "claude-sonnet-4-5",
	}
	if w := do(svc.handleRole(true), http.MethodPost, "/api/roles",
		caller("bob", teamUID, false), seed, nil); w.Code != http.StatusCreated {
		t.Fatalf("seed role create failed: %d %s", w.Code, w.Body.String())
	}

	partial := map[string]any{
		"project":   "widget",
		"name":      "reviewer",
		"promptRef": map[string]any{"name": "reviewer-prompt"},
		"model":     nil, // explicit null ⇒ RFC 7386 delete of spec.model
	}
	w := do(svc.handleRole(false), http.MethodPut, "/api/roles/reviewer",
		caller("bob", teamUID, false), partial, map[string]string{"name": "reviewer"})
	if w.Code != http.StatusOK {
		t.Fatalf("null-delete role edit want 200, got %d: %s", w.Code, w.Body.String())
	}

	var got ksquadv1.Role
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "reviewer"}, &got); err != nil {
		t.Fatalf("get role after null-delete: %v", err)
	}
	if got.Spec.Model != "" {
		t.Errorf("null-sent model must be deleted, got %q", got.Spec.Model)
	}
	if len(got.Spec.DefaultSkills) != 1 || got.Spec.DefaultSkills[0].Name != "code-review" {
		t.Errorf("unsent field defaultSkills must survive the null-delete: got %v", got.Spec.DefaultSkills)
	}
}

// ── Skill: partial PUT keeps unsent capability fields ─────────────────────────
//
// source is admission-required so even a one-field permissions edit must send
// it; toolchains/sidecars/mcpToolRefs are NOT sent and must survive (they were
// the classic silent-drop on every skill save before Gap 1/ISI-5360 Gap 3).
func TestComposeEditSkillPartialPutKeepsUnsentFields(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("bob", "widget", auth.ProjectRoleContributor))

	seed := skillRequest{
		Project: "widget",
		Name:    "deploy-helper",
		Source: struct {
			Type   string `json:"type"`
			Inline string `json:"inline,omitempty"`
			Git    *struct {
				RepoRef string `json:"repoRef"`
				Ref     string `json:"ref"`
				Path    string `json:"path,omitempty"`
			} `json:"git,omitempty"`
		}{Type: "inline", Inline: "run the deploy"},
		McpToolRefs: []string{"k8s-tools"},
		Permissions: []string{"repo:read"},
		Toolchains:  []string{"go", "node"},
		Sidecars:    []string{"redis"},
	}
	if w := do(svc.handleSkill(true), http.MethodPost, "/api/skills",
		caller("bob", teamUID, false), seed, nil); w.Code != http.StatusCreated {
		t.Fatalf("seed skill create failed: %d %s", w.Code, w.Body.String())
	}

	// Edit ONLY permissions (plus the admission-required identity/source).
	partial := map[string]any{
		"project": "widget",
		"name":    "deploy-helper",
		"source": map[string]any{
			"type":   "inline",
			"inline": "run the deploy",
		},
		"permissions": []string{"repo:read", "repo:write"},
	}
	w := do(svc.handleSkill(false), http.MethodPut, "/api/skills/deploy-helper",
		caller("bob", teamUID, false), partial, map[string]string{"name": "deploy-helper"})
	if w.Code != http.StatusOK {
		t.Fatalf("partial skill edit want 200, got %d: %s", w.Code, w.Body.String())
	}

	var got ksquadv1.Skill
	if err := svc.applier.Get(context.Background(), client.ObjectKey{Namespace: teamNS, Name: "deploy-helper"}, &got); err != nil {
		t.Fatalf("get skill after partial edit: %v", err)
	}
	if len(got.Spec.Permissions) != 2 || got.Spec.Permissions[1] != "repo:write" {
		t.Errorf("sent field permissions must be updated, got %v", got.Spec.Permissions)
	}
	if got.Spec.Source.Inline != "run the deploy" {
		t.Errorf("sent field source must be carried, got %q", got.Spec.Source.Inline)
	}
	if len(got.Spec.McpToolRefs) != 1 || got.Spec.McpToolRefs[0].Name != "k8s-tools" {
		t.Errorf("unsent field mcpToolRefs wiped: got %v", got.Spec.McpToolRefs)
	}
	if len(got.Spec.Requires.Toolchains) != 2 {
		t.Errorf("unsent field toolchains wiped: got %v", got.Spec.Requires.Toolchains)
	}
	if len(got.Spec.Requires.Sidecars) != 1 || got.Spec.Requires.Sidecars[0] != "redis" {
		t.Errorf("unsent field sidecars wiped: got %v", got.Spec.Requires.Sidecars)
	}
}

// ── Team: partial PUT keeps the unsent half of the composition ────────────────
//
// Seed a Team with agents AND projects; PUT only the agents list (the roster
// tab's save). The projects half of the composition must ride through — before
// Gap 1 this exact edit silently disbanded every project membership.
func TestComposeEditTeamPartialPutKeepsUnsentFields(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("root", "widget", auth.ProjectRoleMaintainer))

	seed := teamRequest{
		Name:     "isitobservable",
		Agents:   []string{"backend-dev", "frontend-dev"},
		Projects: []string{"widget", "gadget"},
	}
	if w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("root", teamUID, true), seed, nil); w.Code != http.StatusCreated {
		t.Fatalf("seed team create failed: %d %s", w.Code, w.Body.String())
	}

	// Edit ONLY the agents roster; projects is not sent.
	partial := map[string]any{
		"name":   "isitobservable",
		"agents": []string{"backend-dev"},
	}
	w := do(svc.handleTeam(false), http.MethodPut, "/api/teams/isitobservable",
		caller("root", teamUID, true), partial, map[string]string{"name": "isitobservable"})
	if w.Code != http.StatusOK {
		t.Fatalf("partial team edit want 200, got %d: %s", w.Code, w.Body.String())
	}

	var got ksquadv1.Team
	if err := svc.applier.Get(context.Background(),
		client.ObjectKey{Namespace: defaultSystemNamespace, Name: "isitobservable"}, &got); err != nil {
		t.Fatalf("get team after partial edit: %v", err)
	}
	if len(got.Spec.Agents) != 1 || got.Spec.Agents[0].Name != "backend-dev" {
		t.Errorf("sent field agents must be updated, got %v", got.Spec.Agents)
	}
	if len(got.Spec.Projects) != 2 || got.Spec.Projects[1].Name != "gadget" {
		t.Errorf("unsent field projects wiped: got %v", got.Spec.Projects)
	}
}

// decodeComposeRequest must record exactly the caller-sent top-level fields —
// the contract mergeSpecFields' delete/overwrite logic is built on.
func TestDecodeComposeRequestSentFieldsExact(t *testing.T) {
	var req roleRequest
	body := `{"project":"widget","name":"r","promptRef":{"name":"p"},"model":null,"unusedExplicitNull":null}`
	r := httptest.NewRequest(http.MethodPut, "/api/roles/r", strings.NewReader(body))
	w := httptest.NewRecorder()
	sent, ok := decodeComposeRequest(w, r, &req)
	if !ok {
		t.Fatalf("decode failed: %d %s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(sent) != len(raw) {
		t.Fatalf("sent map must mirror the request keys exactly: sent=%v raw=%v", keys(sent), keys(raw))
	}
	for k, v := range raw {
		got, ok := sent[k]
		if !ok {
			t.Fatalf("field %q missing from sent map", k)
		}
		if string(got) != string(v) {
			t.Fatalf("field %q raw mismatch: sent=%s want=%s", k, got, v)
		}
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
