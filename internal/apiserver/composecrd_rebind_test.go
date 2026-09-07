package apiserver

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ============================================================================
// ISI-3924 / ADR-0009 — first-team-create tenancy rebind.
//
// The bootstrap admin is seeded with an invented team_id that no server-assigned
// Team CR uid can ever match, so every team-scoped op 404s. The FIRST Team an
// admin whose team_id backs no Team creates must rebind that admin's tenancy
// root to the new Team's uid and invalidate their sessions. Guardrails: rebind
// ONLY an unresolvable admin root (never a bound tenant — a cross-tenant
// hijack), idempotent, and best-effort AFTER the CR create with the outcome
// surfaced (never silently swallowed).
// ============================================================================

// uidStampApplier mimics the real apiserver stamping metadata.uid on Create (the
// controller-runtime fake client does NOT). It assigns a deterministic uid to a
// Team created with an empty uid so tests can assert the rebind target.
type uidStampApplier struct {
	CRDApplier
	teamUID string
}

func (a *uidStampApplier) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if t, ok := obj.(*ksquadv1.Team); ok && t.UID == "" {
		t.UID = types.UID(a.teamUID)
	}
	return a.CRDApplier.Create(ctx, obj, opts...)
}

// fakeRebinder records RebindTeamID calls and returns a scripted outcome.
type fakeRebinder struct {
	calls   []rebindCall
	rebound bool
	err     error
}

type rebindCall struct {
	principal string
	from, to  uuid.UUID
}

func (f *fakeRebinder) RebindTeamID(_ context.Context, principal string, from, to uuid.UUID) (bool, error) {
	f.calls = append(f.calls, rebindCall{principal, from, to})
	return f.rebound, f.err
}

// newRebindFixture builds a compose service whose applier stamps a server uid on
// Team create and whose rebinder is observable.
func newRebindFixture(t *testing.T, teamUID string, seed ...client.Object) (*ComposeService, *fakeRebinder) {
	t.Helper()
	svc, _ := newComposeFixture(t, nil, seed...)
	svc.applier = &uidStampApplier{CRDApplier: svc.applier, teamUID: teamUID}
	rb := &fakeRebinder{rebound: true}
	svc.rebinder = rb
	return svc, rb
}

// (1) An admin whose team_id backs no Team creates their first Team → team_id is
// rebound to the new Team's server uid and sessions invalidated.
func TestComposeFirstTeamRebindsDanglingAdminRoot(t *testing.T) {
	const freshUID = "99999999-9999-9999-9999-999999999999"
	const newTeamUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	svc, rb := newRebindFixture(t, newTeamUID)

	w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("root", freshUID, true), teamRequest{Name: "isitobservable"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("first Team create must be 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(rb.calls) != 1 {
		t.Fatalf("expected exactly one rebind, got %d", len(rb.calls))
	}
	c := rb.calls[0]
	if c.principal != "root" || c.from != uuid.MustParse(freshUID) || c.to != uuid.MustParse(newTeamUID) {
		t.Fatalf("rebind called with wrong args: %+v", c)
	}
	// A successful rebind carries no warning.
	var res composeResult
	mustJSON(t, w, &res)
	if res.Warning != "" {
		t.Fatalf("successful rebind must not warn, got %q", res.Warning)
	}
}

// (2) A bound admin (team_id resolves to a reconciled Team) creating a 2nd Team
// is NOT rebound — moving a valid tenant would be a cross-tenant hijack.
func TestComposeBoundAdminSecondTeamNotRebound(t *testing.T) {
	// teamUID is seeded by newComposeFixture with a reconciled namespace, so the
	// caller is a bound tenant.
	svc, rb := newRebindFixture(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("root", teamUID, true), teamRequest{Name: "secondsquad"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("2nd Team create must be 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(rb.calls) != 0 {
		t.Fatalf("a bound admin's 2nd Team must NOT rebind, got %d calls: %+v", len(rb.calls), rb.calls)
	}
}

// (3) The non-admin path is unchanged: a non-admin cannot compose a Team (403),
// so the rebind seam is never reached.
func TestComposeNonAdminTeamNeverRebinds(t *testing.T) {
	const freshUID = "99999999-9999-9999-9999-999999999999"
	svc, rb := newRebindFixture(t, "cccccccc-cccc-cccc-cccc-cccccccccccc")

	w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("mallory", freshUID, false), teamRequest{Name: "sneaky"}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin Team compose must be 403, got %d: %s", w.Code, w.Body.String())
	}
	if len(rb.calls) != 0 {
		t.Fatalf("non-admin path must never rebind, got %+v", rb.calls)
	}
}

// (4) A rebind FAILURE after a successful create is surfaced as a response
// warning, never silently swallowed — the Team is real, only re-login defers.
func TestComposeRebindFailureSurfacedAsWarning(t *testing.T) {
	const freshUID = "99999999-9999-9999-9999-999999999999"
	svc, rb := newRebindFixture(t, "dddddddd-dddd-dddd-dddd-dddddddddddd")
	rb.err = errors.New("db down")

	w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("root", freshUID, true), teamRequest{Name: "isitobservable"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create must still 201 despite rebind failure, got %d: %s", w.Code, w.Body.String())
	}
	var res composeResult
	mustJSON(t, w, &res)
	if res.Warning == "" {
		t.Fatalf("rebind failure must surface a warning on the result")
	}
}

// (5) A Team created without a server-assigned uid (defensive: uid never landed)
// must not attempt a rebind to the nil uuid; it warns instead.
func TestComposeRebindSkippedWhenNoServerUID(t *testing.T) {
	const freshUID = "99999999-9999-9999-9999-999999999999"
	// Empty teamUID ⇒ the stamp applier leaves uid unset, mimicking a missing uid.
	svc, rb := newRebindFixture(t, "")

	w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("root", freshUID, true), teamRequest{Name: "isitobservable"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create must 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(rb.calls) != 0 {
		t.Fatalf("must not rebind to a nil uid, got %+v", rb.calls)
	}
	var res composeResult
	mustJSON(t, w, &res)
	if res.Warning == "" {
		t.Fatalf("a missing server uid must surface a warning")
	}
}

// The default onboarding fixture has no rebinder wired (cluster-less dev). A nil
// rebinder must be a safe no-op — Teams still create, admins just aren't rebound.
func TestComposeNilRebinderIsSafeNoOp(t *testing.T) {
	const freshUID = "99999999-9999-9999-9999-999999999999"
	svc, _ := newComposeFixture(t, nil) // rebinder stays nil
	w := do(svc.handleTeam(true), http.MethodPost, "/api/teams",
		caller("root", freshUID, true), teamRequest{Name: "isitobservable"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("nil rebinder must not break Team create, got %d: %s", w.Code, w.Body.String())
	}
}
