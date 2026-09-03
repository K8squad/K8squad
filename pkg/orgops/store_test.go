package orgops

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/taskio"
)

const (
	runUID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	squadNS = "bmad-squad"
)

// newStoreFixture builds a CRDStore over a fake client seeded with a Run whose
// UID is runUID in namespace squadNS (the tenancy root the token resolves), plus
// any extra objects. Returns the store and a captured-provenance slice pointer.
func newStoreFixture(t *testing.T, seed ...client.Object) (*CRDStore, *[]map[string]any) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	objs := []client.Object{
		&ksquadv1.Run{
			ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: squadNS, UID: types.UID(runUID)},
		},
	}
	objs = append(objs, seed...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	var captured []map[string]any
	sink := func(_ context.Context, eventType, principal string, payload map[string]any) {
		row := map[string]any{"eventType": eventType, "principal": principal}
		for k, v := range payload {
			row[k] = v
		}
		captured = append(captured, row)
	}
	return NewCRDStore(c, sink), &captured
}

func runTok() taskio.RunToken {
	return taskio.RunToken{RunID: runUID, WorkItemID: "wi-1", Principal: "ceo-agent", Scopes: []string{ScopeOrgWrite, ScopeProjectWrite}}
}

// A create lands in the CALLING RUN's namespace (resolved from the token's
// RunID), never a client-named one — so org:write cannot reach across tenants.
func TestCreateAgentLandsInRunNamespace(t *testing.T) {
	s, prov := newStoreFixture(t)
	res, err := s.CreateAgent(context.Background(), runTok(), AgentInput{
		Name:                "new-agent",
		RuntimeRef:          objectRefInput{Name: "rt"},
		RoleRef:             objectRefInput{Name: "coder"},
		Model:               "claude",
		CredentialSecretRef: secretRefInput{Name: "cred"},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if res.Namespace != squadNS || res.Name != "new-agent" || res.Operation != "created" {
		t.Fatalf("unexpected result: %+v", res)
	}
	// The CR is actually there, in the run's namespace.
	var got ksquadv1.Agent
	if err := s.c.Get(context.Background(), client.ObjectKey{Namespace: squadNS, Name: "new-agent"}, &got); err != nil {
		t.Fatalf("agent not created in %s: %v", squadNS, err)
	}
	if got.Spec.RoleRef.Name != "coder" || got.Spec.Model != "claude" {
		t.Fatalf("spec not applied: %+v", got.Spec)
	}
	if len(*prov) != 1 || (*prov)[0]["operation"] != "created" {
		t.Fatalf("provenance not recorded: %+v", *prov)
	}
}

// A token whose RunID resolves to no Run is ErrNamespaceUnresolved (404) — a
// caller with no run has no org to mutate. Nothing is written.
func TestCreateUnresolvedRunIs404(t *testing.T) {
	s, _ := newStoreFixture(t)
	tok := runTok()
	tok.RunID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	_, err := s.CreateProject(context.Background(), tok, ProjectInput{Name: "p", Repo: struct {
		URL string `json:"url"`
		Ref string `json:"ref,omitempty"`
	}{URL: "https://x"}})
	if !errors.Is(err, ErrNamespaceUnresolved) {
		t.Fatalf("unresolved run: got %v, want ErrNamespaceUnresolved", err)
	}
}

// A duplicate create is ErrConflict (strict create, never an implicit edit).
func TestCreateAgentConflict(t *testing.T) {
	existing := &ksquadv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: squadNS}}
	s, _ := newStoreFixture(t, existing)
	_, err := s.CreateAgent(context.Background(), runTok(), AgentInput{
		Name: "dup", RuntimeRef: objectRefInput{Name: "rt"}, RoleRef: objectRefInput{Name: "r"},
		Model: "m", CredentialSecretRef: secretRefInput{Name: "c"},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("dup create: got %v, want ErrConflict", err)
	}
}

// Validation rejects a bad name / missing required field before any cluster contact.
func TestCreateValidation(t *testing.T) {
	s, _ := newStoreFixture(t)
	if _, err := s.CreateAgent(context.Background(), runTok(), AgentInput{Name: "Bad Name"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad name: got %v, want ErrValidation", err)
	}
	if _, err := s.CreateAgent(context.Background(), runTok(), AgentInput{Name: "ok"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing fields: got %v, want ErrValidation", err)
	}
	// Skill with an unknown source type.
	if _, err := s.CreateSkill(context.Background(), runTok(), SkillInput{Name: "sk"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad skill source: got %v, want ErrValidation", err)
	}
}

// ArchiveProject stamps the archived annotation on an existing Project and is
// idempotent; a missing Project is ErrNotFound.
func TestArchiveProject(t *testing.T) {
	proj := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp", Namespace: squadNS},
		Spec:       ksquadv1.ProjectSpec{Repo: ksquadv1.RepoSpec{URL: "https://x"}},
	}
	s, prov := newStoreFixture(t, proj)

	res, err := s.ArchiveProject(context.Background(), runTok(), "webapp")
	if err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}
	if res.Operation != "archived" {
		t.Fatalf("op = %q, want archived", res.Operation)
	}
	var got ksquadv1.Project
	if err := s.c.Get(context.Background(), client.ObjectKey{Namespace: squadNS, Name: "webapp"}, &got); err != nil {
		t.Fatalf("re-get project: %v", err)
	}
	if got.Annotations[AnnotationArchived] != "true" || got.Annotations[AnnotationArchivedAt] == "" {
		t.Fatalf("archive annotations missing: %+v", got.Annotations)
	}
	// Idempotent second archive: still success, no second provenance row churn error.
	if _, err := s.ArchiveProject(context.Background(), runTok(), "webapp"); err != nil {
		t.Fatalf("re-archive: %v", err)
	}
	if len(*prov) < 1 {
		t.Fatalf("expected provenance for the first archive: %+v", *prov)
	}

	// Missing project → ErrNotFound.
	if _, err := s.ArchiveProject(context.Background(), runTok(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive missing: got %v, want ErrNotFound", err)
	}
}

// Assign resolves an existing teammate Agent in the run's namespace and returns
// the A2A carrier; an unknown target is ErrNotFound. It writes no board row
// (coord has no assignee — I4/no-P2P).
func TestAssignResolvesTargetOrNotFound(t *testing.T) {
	teammate := &ksquadv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: squadNS}}
	s, prov := newStoreFixture(t, teammate)

	res, err := s.Assign(context.Background(), runTok(), AssignInput{ToAgent: "coder", WorkItemID: "wi-1"})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if res.A2A.Verb != "SubmitTask" || res.A2A.TargetAgent != "coder" || res.A2A.AgentCardRef != squadNS+"/coder" {
		t.Fatalf("A2A carrier not resolved: %+v", res.A2A)
	}
	if res.CoordEnqueue != "/api/task-io/subtask" {
		t.Fatalf("coord enqueue carrier missing: %+v", res)
	}
	if len(*prov) != 1 || (*prov)[0]["operation"] != "assigned" {
		t.Fatalf("assign provenance not recorded: %+v", *prov)
	}

	// Unknown teammate → ErrNotFound (existence-checked within the run's squad).
	if _, err := s.Assign(context.Background(), runTok(), AssignInput{ToAgent: "ghost"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("assign unknown target: got %v, want ErrNotFound", err)
	}
}
