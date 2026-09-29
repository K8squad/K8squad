package apiserver

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeWorkItemProjectReader stands in for coord.WorkItemReadStore.WorkItemProject: `items` maps a
// work-item UUID to its owning-project UID + authoritative title; a missing key reads as
// ErrWorkItemNotFound (the not-in-my-Team / unknown-id contract). `readErr`, when set, forces a
// coord-plane failure for any lookup. gotTeam records the last team scope the resolver passed.
type fakeWorkItemProjectReader struct {
	items   map[string]coord.WorkItemProjectRef
	readErr error
	gotTeam string
}

func (f *fakeWorkItemProjectReader) WorkItemProject(_ context.Context, workItemID, teamID string) (coord.WorkItemProjectRef, error) {
	f.gotTeam = teamID
	if f.readErr != nil {
		return coord.WorkItemProjectRef{}, f.readErr
	}
	ref, ok := f.items[workItemID]
	if !ok {
		return coord.WorkItemProjectRef{}, coord.ErrWorkItemNotFound
	}
	return ref, nil
}

const (
	trrRoomSlug = "acme/backlog"
	trrRoomUID  = "912e88e2-7f56-4d46-8a81-f2eab0019421"
	trrInProj1  = "11111111-1111-1111-1111-111111111111"
	trrInProj2  = "22222222-2222-2222-2222-222222222222"
	trrOtherPrj = "33333333-3333-3333-3333-333333333333"
	trrOtherUID = "44444444-4444-4444-4444-444444444444"
	trrMissing  = "55555555-5555-5555-5555-555555555555"
)

func newTestTicketRefResolver(t *testing.T, reader *fakeWorkItemProjectReader, refs ProjectRefResolver) discussion.TicketRefResolver {
	t.Helper()
	return ticketRefResolver{reads: reader, refs: refs}
}

// In-project refs resolve and their titles are canonicalized from coord; an out-of-project UUID and an
// unknown/foreign-team UUID are both dropped. This is the LINK-not-dispatch guardrail's resolution half.
func TestResolveTicketRefsProjectScoping(t *testing.T) {
	reader := &fakeWorkItemProjectReader{items: map[string]coord.WorkItemProjectRef{
		trrInProj1:  {ProjectID: trrRoomUID, Title: "Canonical One"},
		trrInProj2:  {ProjectID: trrRoomUID, Title: "Canonical Two"},
		trrOtherPrj: {ProjectID: trrOtherUID, Title: "Foreign"},
	}}
	refs := &fakeProjectRefs{res: ProjectRefResolution{UID: trrRoomUID}}
	r := newTestTicketRefResolver(t, reader, refs)

	team := uuid.MustParse("7191cc8c-f4b7-4b60-b63e-d25408ac0d1c")
	got, err := r.ResolveTicketRefs(context.Background(), trrRoomSlug, team, []discussion.TicketRef{
		{WorkItemID: trrInProj1, Title: "client-supplied label"},  // canonicalized away
		{WorkItemID: trrOtherPrj, Title: "out of project"},        // dropped
		{WorkItemID: trrMissing, Title: "unknown / foreign team"}, // dropped
		{WorkItemID: trrInProj2, Title: "another"},                // canonicalized
	})
	if err != nil {
		t.Fatalf("ResolveTicketRefs: unexpected error %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("resolved %d refs, want 2 (only the two in-project survive): %+v", len(got), got)
	}
	if got[0].WorkItemID != trrInProj1 || got[0].Title != "Canonical One" {
		t.Fatalf("ref[0] = %+v, want trrInProj1 canonicalized to %q", got[0], "Canonical One")
	}
	if got[1].WorkItemID != trrInProj2 || got[1].Title != "Canonical Two" {
		t.Fatalf("ref[1] = %+v, want trrInProj2 canonicalized to %q", got[1], "Canonical Two")
	}
	if refs.gotRef != trrRoomSlug {
		t.Fatalf("project resolver asked to resolve %q, want the room slug %q", refs.gotRef, trrRoomSlug)
	}
	if reader.gotTeam != team.String() {
		t.Fatalf("coord read scoped to team %q, want %q", reader.gotTeam, team.String())
	}
}

// A zero Team scope is the fleet-admin trusted path: the resolver passes an EMPTY teamID to coord
// (mirrors the empty-teamID convention the read store documents), not the zero UUID's string form.
func TestResolveTicketRefsAdminTeamScope(t *testing.T) {
	reader := &fakeWorkItemProjectReader{items: map[string]coord.WorkItemProjectRef{
		trrInProj1: {ProjectID: trrRoomUID, Title: "One"},
	}}
	refs := &fakeProjectRefs{res: ProjectRefResolution{UID: trrRoomUID}}
	r := newTestTicketRefResolver(t, reader, refs)

	got, err := r.ResolveTicketRefs(context.Background(), trrRoomSlug, uuid.Nil, []discussion.TicketRef{
		{WorkItemID: trrInProj1},
	})
	if err != nil {
		t.Fatalf("ResolveTicketRefs: unexpected error %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("resolved %d refs, want 1", len(got))
	}
	if reader.gotTeam != "" {
		t.Fatalf("admin (uuid.Nil) team scope reached coord as %q, want empty (trusted fleet-admin path)", reader.gotTeam)
	}
}

// A real coord read failure surfaces as an error (the seam then degrades the whole post to link-free);
// an unresolvable room project degrades to no references without erroring.
func TestResolveTicketRefsErrorPaths(t *testing.T) {
	t.Run("coord read failure surfaces", func(t *testing.T) {
		reader := &fakeWorkItemProjectReader{readErr: errors.New("coord down")}
		refs := &fakeProjectRefs{res: ProjectRefResolution{UID: trrRoomUID}}
		r := newTestTicketRefResolver(t, reader, refs)
		if _, err := r.ResolveTicketRefs(context.Background(), trrRoomSlug, uuid.Nil, []discussion.TicketRef{{WorkItemID: trrInProj1}}); err == nil {
			t.Fatal("expected an error when the coord read fails")
		}
	})

	t.Run("unresolvable room project degrades to link-free", func(t *testing.T) {
		reader := &fakeWorkItemProjectReader{items: map[string]coord.WorkItemProjectRef{trrInProj1: {ProjectID: trrRoomUID}}}
		refs := &fakeProjectRefs{err: ErrProjectNotFound}
		r := newTestTicketRefResolver(t, reader, refs)
		got, err := r.ResolveTicketRefs(context.Background(), trrRoomSlug, uuid.Nil, []discussion.TicketRef{{WorkItemID: trrInProj1}})
		if err != nil {
			t.Fatalf("unresolvable project should degrade, not error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("resolved %d refs, want 0 (room project unresolvable)", len(got))
		}
	})

	t.Run("empty candidate set", func(t *testing.T) {
		r := newTestTicketRefResolver(t, &fakeWorkItemProjectReader{}, &fakeProjectRefs{})
		got, err := r.ResolveTicketRefs(context.Background(), trrRoomSlug, uuid.Nil, nil)
		if err != nil || got != nil {
			t.Fatalf("empty candidates = (%+v, %v), want (nil, nil)", got, err)
		}
	})
}

// NewTicketRefResolver returns a nil interface when either dependency is absent, so a DB-less dev run
// leaves references dropped (link-free) exactly as before the wiring landed.
func TestNewTicketRefResolverNilDeps(t *testing.T) {
	if r := NewTicketRefResolver(nil, &fakeProjectRefs{}); r != nil {
		t.Fatal("nil coord read store should yield a nil resolver")
	}
	if r := NewTicketRefResolver((*coord.WorkItemReadStore)(nil), nil); r != nil {
		t.Fatal("nil project resolver should yield a nil resolver")
	}
}
