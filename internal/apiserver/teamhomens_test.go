package apiserver

// teamhomens_test.go — unit coverage for the ONE shared Team-UID → HOME-ns
// resolver (ISI-5422). The split-layout regression guards live in the
// per-surface suites (secretwrite_test, composecrd_test) and must stay green
// unchanged; these tests pin the core itself:
//   - the metadata (HOME) namespace is the answer, Status.Namespace (EXEC ns)
//     is never returned — the ISI-5415 drift, now impossible by construction;
//   - unknown UID / empty UID fail closed with ErrTeamNamespaceUnresolved;
//   - a reader failure passes through untouched;
//   - the read-model vocabulary mapping (errTeamNotFoundFrom) translates only
//     the sentinel.

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

func TestResolveTeamHomeNamespaceReturnsHomeNotExec(t *testing.T) {
	// Split layout (ISI-4128): home ns "bmad-squad", exec ns
	// "ksquad-team-bmad-squad-f6e8fc70". The resolver must answer with the
	// HOME ns — the exact disagreement that was ISI-5415.
	home := teamWithStatus("bmad-squad", "bmad-squad", "11111111-1111-1111-1111-111111111111", "ksquad-team-bmad-squad-f6e8fc70")
	c := fake.NewClientBuilder().WithScheme(secretWriteScheme(t)).WithObjects(home).Build()

	ns, err := resolveTeamHomeNamespace(context.Background(), c, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("want home ns, got err %v", err)
	}
	if ns != "bmad-squad" {
		t.Fatalf("resolveTeamHomeNamespace = %q, want %q (Status.Namespace must never be the answer)", ns, "bmad-squad")
	}
}

func TestResolveTeamHomeNamespaceFailClosed(t *testing.T) {
	seeded := teamWithStatus("bmad-squad", "bmad-squad", "11111111-1111-1111-1111-111111111111", "ksquad-team-bmad-squad-f6e8fc70")
	c := fake.NewClientBuilder().WithScheme(secretWriteScheme(t)).WithObjects(seeded).Build()

	t.Run("unknown UID", func(t *testing.T) {
		if _, err := resolveTeamHomeNamespace(context.Background(), c, "99999999-9999-9999-9999-999999999999"); !errors.Is(err, ErrTeamNamespaceUnresolved) {
			t.Fatalf("unknown UID: got %v, want ErrTeamNamespaceUnresolved", err)
		}
	})

	t.Run("empty UID", func(t *testing.T) {
		if _, err := resolveTeamHomeNamespace(context.Background(), c, ""); !errors.Is(err, ErrTeamNamespaceUnresolved) {
			t.Fatalf("empty UID: got %v, want ErrTeamNamespaceUnresolved", err)
		}
	})

	t.Run("empty home ns is impossible to seed through a real client but guarded", func(t *testing.T) {
		// A fake client cannot store a namespace-less Team, so the guard is
		// exercised directly on the extraction helper — the only path an
		// empty metadata ns could take.
		if _, err := teamHomeNamespace(&ksquadv1.Team{}); !errors.Is(err, ErrTeamNamespaceUnresolved) {
			t.Fatalf("empty home ns: got %v, want ErrTeamNamespaceUnresolved", err)
		}
	})
}

// failingReader surfaces the List error the informer cache could raise; the
// shared core must pass it through untouched (never translate, never swallow).
type failingReader struct{ client.Reader }

func (failingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return errors.New("boom: cache unavailable")
}

func TestResolveTeamHomeNamespacePassesReaderErrorThrough(t *testing.T) {
	if _, err := resolveTeamHomeNamespace(context.Background(), failingReader{}, "11111111-1111-1111-1111-111111111111"); err == nil || errors.Is(err, ErrTeamNamespaceUnresolved) {
		t.Fatalf("reader failure: got %v, want the raw transport error", err)
	}
}

func TestMatchTeamByUIDReturnsTheCR(t *testing.T) {
	// The probes (credentialtest, repoauthtest) annotate the matched CR, so
	// the core must hand back the object itself, not just its namespace.
	seeded := team("bmad-squad", "bmad-squad", "11111111-1111-1111-1111-111111111111")
	c := fake.NewClientBuilder().WithScheme(secretWriteScheme(t)).WithObjects(seeded).Build()

	got, err := matchTeamByUID(context.Background(), c, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("want the Team CR, got err %v", err)
	}
	if got.Name != "bmad-squad" {
		t.Fatalf("matchTeamByUID returned %q, want %q", got.Name, "bmad-squad")
	}
}

func TestErrTeamNotFoundFromMapsOnlyTheSentinel(t *testing.T) {
	if got := errTeamNotFoundFrom(ErrTeamNamespaceUnresolved); !errors.Is(got, ErrTeamNotFound) {
		t.Fatalf("sentinel: got %v, want ErrTeamNotFound", got)
	}
	other := errors.New("boom")
	if got := errTeamNotFoundFrom(other); !errors.Is(got, other) {
		t.Fatalf("transport error must pass through, got %v", got)
	}
}
