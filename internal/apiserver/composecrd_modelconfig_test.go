package apiserver

import (
	"context"
	"net/http"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/auth"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// ISI-4890 S1 — the org-default model tier (modelconfig compose kind). It is the
// ONE compose kind that is a FIXED singleton (k8squad-system/default), admin-only,
// and NOT team-scoped: planModelConfig pins the identity so the write lands exactly
// where the resolver (pkg/modelendpoint) reads it. handleModelConfig always upserts
// (create-or-edit) so the Settings Save is idempotent.

// mcKey is the well-known target every modelconfig write must land on.
func mcKey() client.ObjectKey {
	return client.ObjectKey{
		Namespace: modelendpoint.DefaultSystemNamespace, // k8squad-system
		Name:      modelendpoint.DefaultModelConfigName,  // default
	}
}

// TestModelConfig_AdminCreate_FixedIdentity — an admin sets the org default; it
// lands at the fixed k8squad-system/default (NOT the caller's team namespace),
// revision 1, with a provenance row. The primary + fallback + BYO endpoint all
// round-trip onto ModelConfigSpec.
func TestModelConfig_AdminCreate_FixedIdentity(t *testing.T) {
	svc, prov := newComposeFixture(t, nil)
	body := modelConfigRequest{
		Model:            "claude-opus-4-8",
		FallbackModel:    &fallbackModelWire{Model: "claude-haiku-4-5"},
		ModelEndpointRef: &secretRefWire{Name: "byo-endpoint"},
	}
	// caller's team UID is a bogus one that resolves to NO namespace — proving the
	// fixed-namespace path never runs the team-namespace resolver for this kind.
	w := do(svc.handleModelConfig(), http.MethodPost, "/api/modelconfig",
		caller("root", "99999999-9999-9999-9999-999999999999", true), body, nil)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var res composeResult
	mustJSON(t, w, &res)
	if res.Namespace != modelendpoint.DefaultSystemNamespace || res.Name != modelendpoint.DefaultModelConfigName {
		t.Fatalf("want fixed identity %s/%s, got %s/%s",
			modelendpoint.DefaultSystemNamespace, modelendpoint.DefaultModelConfigName, res.Namespace, res.Name)
	}
	if res.Revision != 1 || res.Operation != "created" {
		t.Fatalf("unexpected result: %+v", res)
	}
	var got ksquadv1.ModelConfig
	if err := svc.applier.Get(context.Background(), mcKey(), &got); err != nil {
		t.Fatalf("modelconfig not applied at %v: %v", mcKey(), err)
	}
	if got.Spec.Model != "claude-opus-4-8" {
		t.Fatalf("primary model not persisted: %+v", got.Spec)
	}
	if got.Spec.FallbackModel == nil || got.Spec.FallbackModel.Model != "claude-haiku-4-5" {
		t.Fatalf("fallback not persisted: %+v", got.Spec.FallbackModel)
	}
	if got.Spec.ModelEndpointRef == nil || got.Spec.ModelEndpointRef.Name != "byo-endpoint" {
		t.Fatalf("BYO endpoint ref not persisted: %+v", got.Spec.ModelEndpointRef)
	}
	if len(*prov) != 1 || (*prov)[0]["operation"] != "created" {
		t.Fatalf("want one created provenance row, got %+v", *prov)
	}
}

// TestModelConfig_AdminEdit_Upserts — a second Save revises the singleton (upsert),
// never 409s, and bumps the revision. This is the create-or-edit contract the
// Settings Save relies on.
func TestModelConfig_AdminEdit_Upserts(t *testing.T) {
	svc, _ := newComposeFixture(t, nil)
	admin := caller("root", teamUID, true)

	first := do(svc.handleModelConfig(), http.MethodPost, "/api/modelconfig", admin,
		modelConfigRequest{Model: "claude-opus-4-8"}, nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("first save want 201, got %d: %s", first.Code, first.Body.String())
	}

	second := do(svc.handleModelConfig(), http.MethodPost, "/api/modelconfig", admin,
		modelConfigRequest{Model: "claude-sonnet-5"}, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second save want 200 (updated), got %d: %s", second.Code, second.Body.String())
	}
	var res composeResult
	mustJSON(t, second, &res)
	if res.Operation != "updated" || res.Revision != 2 {
		t.Fatalf("want updated rev 2, got %+v", res)
	}
	var got ksquadv1.ModelConfig
	if err := svc.applier.Get(context.Background(), mcKey(), &got); err != nil {
		t.Fatalf("modelconfig missing after edit: %v", err)
	}
	if got.Spec.Model != "claude-sonnet-5" {
		t.Fatalf("edit did not revise the primary: %q", got.Spec.Model)
	}
}

// TestModelConfig_NonAdminForbidden — the adminOnly write scope 403s a non-admin
// EVEN with a project grant (the org default is not project-scoped). Server is
// authoritative regardless of the client gate.
func TestModelConfig_NonAdminForbidden(t *testing.T) {
	svc, _ := newComposeFixture(t, grant("alice", "widget", auth.ProjectRoleMaintainer))
	w := do(svc.handleModelConfig(), http.MethodPost, "/api/modelconfig",
		caller("alice", teamUID, false), modelConfigRequest{Model: "claude-opus-4-8"}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 for non-admin, got %d: %s", w.Code, w.Body.String())
	}
}

// TestModelConfig_EmptyModel422 — the fail-closed guardrail (AC4): an empty primary
// is a clean pre-apply 422 on the `model` field, and nothing is written.
func TestModelConfig_EmptyModel422(t *testing.T) {
	svc, _ := newComposeFixture(t, nil)
	w := do(svc.handleModelConfig(), http.MethodPost, "/api/modelconfig",
		caller("root", teamUID, true), modelConfigRequest{Model: ""}, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for empty model, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Fields []fieldError `json:"fields"`
	}
	mustJSON(t, w, &body)
	if len(body.Fields) != 1 || body.Fields[0].Field != "model" {
		t.Fatalf("want a single field error on `model`, got %+v", body.Fields)
	}
	// Nothing landed.
	var got ksquadv1.ModelConfig
	if err := svc.applier.Get(context.Background(), mcKey(), &got); err == nil {
		t.Fatalf("empty-model write should not have applied a ModelConfig")
	}
}

// TestModelConfig_GetHydration — GET returns the persisted default mapped onto the
// write wire shape; a missing default is a 404 (the empty-form state); a non-admin
// read is 403.
func TestModelConfig_GetHydration(t *testing.T) {
	seed := &ksquadv1.ModelConfig{
		Spec: ksquadv1.ModelConfigSpec{Model: "claude-opus-4-8"},
	}
	seed.SetName(modelendpoint.DefaultModelConfigName)
	seed.SetNamespace(modelendpoint.DefaultSystemNamespace)
	svc, _ := newComposeFixture(t, nil, seed)

	// admin GET → 200 with the wire shape
	w := do(svc.handleModelConfigGet(), http.MethodGet, "/api/modelconfig",
		caller("root", teamUID, true), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var wire modelConfigRequest
	mustJSON(t, w, &wire)
	if wire.Model != "claude-opus-4-8" {
		t.Fatalf("hydration wire wrong: %+v", wire)
	}

	// non-admin GET → 403
	forbidden := do(svc.handleModelConfigGet(), http.MethodGet, "/api/modelconfig",
		caller("alice", teamUID, false), nil, nil)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("want 403 for non-admin read, got %d", forbidden.Code)
	}

	// missing default → 404 (empty-form state)
	empty, _ := newComposeFixture(t, nil)
	miss := do(empty.handleModelConfigGet(), http.MethodGet, "/api/modelconfig",
		caller("root", teamUID, true), nil, nil)
	if miss.Code != http.StatusNotFound {
		t.Fatalf("want 404 for absent default, got %d", miss.Code)
	}
}
