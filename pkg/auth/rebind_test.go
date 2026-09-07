package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeTeamIDUpdater scripts the compare-and-swap outcome.
type fakeTeamIDUpdater struct {
	id       uuid.UUID
	err      error
	gotFrom  uuid.UUID
	gotTo    uuid.UUID
	gotPrinc string
	calls    int
}

func (f *fakeTeamIDUpdater) UpdateTeamID(_ context.Context, principal string, from, to uuid.UUID) (uuid.UUID, error) {
	f.calls++
	f.gotPrinc, f.gotFrom, f.gotTo = principal, from, to
	return f.id, f.err
}

// fakeRevoker records the user whose sessions were killed.
type fakeRevoker struct {
	got   uuid.UUID
	err   error
	calls int
}

func (f *fakeRevoker) RevokeAllForUser(_ context.Context, userID uuid.UUID) error {
	f.calls++
	f.got = userID
	return f.err
}

func TestTenancyRebinder_HappySwapRevokesSessions(t *testing.T) {
	uid := uuid.New()
	from, to := uuid.New(), uuid.New()
	users := &fakeTeamIDUpdater{id: uid}
	sess := &fakeRevoker{}
	r := NewTenancyRebinder(users, sess)

	rebound, err := r.RebindTeamID(context.Background(), "user:root", from, to)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rebound {
		t.Fatalf("want rebound=true")
	}
	if users.gotPrinc != "user:root" || users.gotFrom != from || users.gotTo != to {
		t.Fatalf("CAS args wrong: %+v", users)
	}
	if sess.calls != 1 || sess.got != uid {
		t.Fatalf("sessions must be revoked for the rebound user, got calls=%d id=%s", sess.calls, sess.got)
	}
}

func TestTenancyRebinder_CASMissIsNoOp(t *testing.T) {
	// An already-bound admin (team_id no longer matches `from`) → ErrNotFound from
	// the store → rebound=false, no session revoke, no error.
	users := &fakeTeamIDUpdater{err: ErrNotFound}
	sess := &fakeRevoker{}
	r := NewTenancyRebinder(users, sess)

	rebound, err := r.RebindTeamID(context.Background(), "user:bound", uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("CAS miss must not error, got %v", err)
	}
	if rebound {
		t.Fatalf("CAS miss must be rebound=false")
	}
	if sess.calls != 0 {
		t.Fatalf("no swap ⇒ no session revoke, got %d", sess.calls)
	}
}

func TestTenancyRebinder_UpdateErrorPropagates(t *testing.T) {
	users := &fakeTeamIDUpdater{err: errors.New("db down")}
	sess := &fakeRevoker{}
	r := NewTenancyRebinder(users, sess)

	rebound, err := r.RebindTeamID(context.Background(), "user:root", uuid.New(), uuid.New())
	if err == nil {
		t.Fatalf("update error must propagate")
	}
	if rebound {
		t.Fatalf("failed update must be rebound=false")
	}
	if sess.calls != 0 {
		t.Fatalf("failed update ⇒ no session revoke")
	}
}

func TestTenancyRebinder_RevokeErrorSurfacesButRebound(t *testing.T) {
	// team_id already moved; a session-revoke failure must be surfaced (not
	// swallowed) yet reported as rebound=true — the tenancy write DID land.
	users := &fakeTeamIDUpdater{id: uuid.New()}
	sess := &fakeRevoker{err: errors.New("session store down")}
	r := NewTenancyRebinder(users, sess)

	rebound, err := r.RebindTeamID(context.Background(), "user:root", uuid.New(), uuid.New())
	if !rebound {
		t.Fatalf("team_id moved ⇒ rebound=true even if revoke failed")
	}
	if err == nil {
		t.Fatalf("session revoke failure must be surfaced")
	}
}
