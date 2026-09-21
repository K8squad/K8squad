package memory

import (
	"context"
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

func resolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(s); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return s
}

func teamCR(ns, name, uid string, agents ...string) *ksquadv1.Team {
	refs := make([]ksquadv1.ObjectRef, 0, len(agents))
	for _, a := range agents {
		refs = append(refs, ksquadv1.ObjectRef{Name: a})
	}
	return &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec:       ksquadv1.TeamSpec{Agents: refs},
	}
}

func newResolver(t *testing.T, objs ...client.Object) *ClientTeamAgentResolver {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(objs...).Build()
	return NewClientTeamAgentResolver(c)
}

// TestTeamAgentsResolvesByUID: the resolver matches on the Team CR uid
// (coord.work_item.team_id), not name/namespace, and returns the composition
// sorted deterministically — the exact agent-∈-Team set coord's dispatch guard
// checks the assignee against.
func TestTeamAgentsResolvesByUID(t *testing.T) {
	r := newResolver(t,
		teamCR("team-a", "alpha", "uid-alpha", "sam", "john"),
		teamCR("team-b", "beta", "uid-beta", "kim"),
	)
	got, err := r.TeamAgents(context.Background(), "uid-alpha")
	if err != nil {
		t.Fatalf("TeamAgents: unexpected error: %v", err)
	}
	if want := []string{"john", "sam"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("TeamAgents(uid-alpha) = %v, want %v (sorted)", got, want)
	}
}

// TestTeamAgentsEmptyComposition: a Team with no agents resolves to an empty set,
// never an error — the team exists (not dangling); dispatch will then reject any
// assignee as not-a-member via the empty set, which is correct.
func TestTeamAgentsEmptyComposition(t *testing.T) {
	r := newResolver(t, teamCR("team-a", "alpha", "uid-alpha"))
	got, err := r.TeamAgents(context.Background(), "uid-alpha")
	if err != nil {
		t.Fatalf("TeamAgents: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("TeamAgents(empty team) = %v, want empty", got)
	}
}

// TestTeamAgentsDanglingUID: a uid that matches no Team CR is a loud ERROR, never a
// silent empty set — dispatch must fail rather than vacuously reject every agent as
// "not a member" (the invariant the apiserver resolver keeps too).
func TestTeamAgentsDanglingUID(t *testing.T) {
	r := newResolver(t, teamCR("team-a", "alpha", "uid-alpha", "sam"))
	if _, err := r.TeamAgents(context.Background(), "uid-missing"); err == nil {
		t.Fatal("TeamAgents(dangling uid): want error, got nil")
	}
}

// TestTeamAgentsEmptyUID: an empty uid is rejected up front (a malformed call), not
// treated as "list everything".
func TestTeamAgentsEmptyUID(t *testing.T) {
	r := newResolver(t, teamCR("team-a", "alpha", "uid-alpha", "sam"))
	if _, err := r.TeamAgents(context.Background(), ""); err == nil {
		t.Fatal("TeamAgents(empty uid): want error, got nil")
	}
}

// TestTeamAgentsListError: a reader failure propagates as an error (dispatch fails
// closed), distinct from the dangling-team case.
func TestTeamAgentsListError(t *testing.T) {
	r := NewClientTeamAgentResolver(errReader{err: errors.New("boom")})
	if _, err := r.TeamAgents(context.Background(), "uid-alpha"); err == nil {
		t.Fatal("TeamAgents(list error): want error, got nil")
	}
}

// errReader is a client.Reader whose List always fails, to exercise the
// infrastructure-error path.
type errReader struct {
	client.Reader
	err error
}

func (e errReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return e.err
}
