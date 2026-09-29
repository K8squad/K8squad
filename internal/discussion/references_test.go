package discussion

// Ticket-reference coverage (ISI-5165, plan ISI-5134 S1). A ticket reference is a LINK, not a
// dispatch: it is validated against the message's project, persisted into the message payload under
// `references`, and MUST NOT trigger any agent run. These tests ride the default unit lane — the
// payload helpers are pure, and the resolution hook is exercised through a fake resolver with a nil
// *Store (exactly like the sibling mention-search / dispatch tests). The three plan-mandated
// scenarios are all here: an in-project ref resolves + persists, an out-of-project/unknown ref is
// dropped, and an @-mention + a ticket reference coexist in one message payload without collision.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeRefResolver is the TicketRefResolver seam with a canned in-project corpus: `known` maps a
// work-item UUID to its canonical title; any candidate not in `known` is treated as unknown or
// out-of-project and dropped. It captures the projectID/teamID/refs it was handed so a test can prove
// the project-narrowing scope rides the call.
type fakeRefResolver struct {
	known             map[string]string // in-project work-item UUID -> canonical title
	err               error
	capturedProjectID string
	capturedTeamID    uuid.UUID
	capturedRefs      []TicketRef
}

func (f *fakeRefResolver) ResolveTicketRefs(_ context.Context, projectID string, teamID uuid.UUID, refs []TicketRef) ([]TicketRef, error) {
	f.capturedProjectID = projectID
	f.capturedTeamID = teamID
	f.capturedRefs = refs
	if f.err != nil {
		return nil, f.err
	}
	out := make([]TicketRef, 0, len(refs))
	for _, r := range refs {
		title, ok := f.known[r.WorkItemID]
		if !ok {
			continue // unknown / out-of-project — dropped, never persisted
		}
		out = append(out, TicketRef{WorkItemID: r.WorkItemID, Title: title})
	}
	return out, nil
}

// --- StampReferences / ReferencesOf round-trip ------------------------------

func TestStampReferencesRoundTrip(t *testing.T) {
	// Empty ref set leaves the payload untouched (no `references` key), including a nil payload.
	if got := StampReferences(nil, nil); got != nil {
		t.Fatalf("StampReferences(nil, nil) = %s, want nil (link-free)", *got)
	}
	if got := ReferencesOf(nil); got != nil {
		t.Fatalf("ReferencesOf(nil) = %v, want nil", got)
	}

	refs := []TicketRef{
		{WorkItemID: "11111111-1111-1111-1111-111111111111", Title: "Fix the intake sweep"},
		{WorkItemID: "22222222-2222-2222-2222-222222222222", Title: "Wire the exporter"},
	}
	stamped := StampReferences(nil, refs)
	if stamped == nil {
		t.Fatal("StampReferences: expected a payload for a non-empty ref set")
	}
	got := ReferencesOf(stamped)
	if len(got) != 2 || got[0].WorkItemID != refs[0].WorkItemID || got[0].Title != refs[0].Title ||
		got[1].WorkItemID != refs[1].WorkItemID {
		t.Fatalf("round-trip refs = %+v, want %+v", got, refs)
	}
}

// --- coexistence: a hop stamp and ticket references share one payload -------

func TestStampReferencesCoexistsWithDispatchHop(t *testing.T) {
	// A dispatched agent reply carries a loop-guard hop AND links a ticket: both must survive in one
	// payload object (the JSON-merge pattern), neither clobbering the other.
	hopPayload := StampDispatchHop(nil, 1)
	refs := []TicketRef{{WorkItemID: "33333333-3333-3333-3333-333333333333", Title: "Related ticket"}}
	merged := StampReferences(&hopPayload, refs)

	if got := DispatchHopOf(merged); got != 1 {
		t.Fatalf("hop after ref merge = %d, want 1 (preserved)", got)
	}
	gotRefs := ReferencesOf(merged)
	if len(gotRefs) != 1 || gotRefs[0].WorkItemID != refs[0].WorkItemID {
		t.Fatalf("refs after merge = %+v, want the linked ticket preserved", gotRefs)
	}

	// And the reverse order: stamping a hop onto a references payload preserves the links.
	refPayload := StampReferences(nil, refs)
	both := StampDispatchHop(refPayload, 2)
	if DispatchHopOf(&both) != 2 || len(ReferencesOf(&both)) != 1 {
		t.Fatalf("merge (refs then hop) = %s, want both hop 2 and the link", both)
	}
}

// --- normalizeTicketRefs: dedup + UUID-keyed MVP enforcement ----------------

func TestNormalizeTicketRefs(t *testing.T) {
	id1 := "11111111-1111-1111-1111-111111111111"
	id2 := "22222222-2222-2222-2222-222222222222"
	in := []TicketRef{
		{WorkItemID: id1, Title: "first"},
		{WorkItemID: "  " + id1 + "  ", Title: "dup, trimmed to same id"}, // de-duped
		{WorkItemID: "", Title: "blank id dropped"},
		{WorkItemID: "ISI-1234", Title: "typed literal is out of scope (not a UUID)"}, // dropped
		{WorkItemID: id2, Title: "second"},
	}
	got := normalizeTicketRefs(in)
	if len(got) != 2 {
		t.Fatalf("normalizeTicketRefs = %+v, want 2 (dedup + drop blank/non-UUID)", got)
	}
	if got[0].WorkItemID != id1 || got[0].Title != "first" {
		t.Fatalf("normalizeTicketRefs[0] = %+v, want first-seen %s/first", got[0], id1)
	}
	if got[1].WorkItemID != id2 {
		t.Fatalf("normalizeTicketRefs[1] = %+v, want %s (order preserved)", got[1], id2)
	}
}

// --- (1) an in-project ref resolves + persists ------------------------------

func TestStampTicketRefsResolvesAndPersists(t *testing.T) {
	inProject := "11111111-1111-1111-1111-111111111111"
	resolver := &fakeRefResolver{known: map[string]string{inProject: "Canonical Title"}}
	h := NewHandlerWithDeps(nil, nil, nil)
	h.SetTicketRefResolver(resolver)

	team := uuid.New()
	auth := AuthorContext{Principal: "user:alice", TeamID: team}
	// The client-supplied title is deliberately stale — the resolver canonicalizes it.
	refs := []TicketRef{{WorkItemID: inProject, Title: "stale client title"}}

	payload := h.stampTicketRefs(context.Background(), "squad-a/proj", auth, refs, nil)

	persisted := ReferencesOf(payload)
	if len(persisted) != 1 || persisted[0].WorkItemID != inProject || persisted[0].Title != "Canonical Title" {
		t.Fatalf("persisted refs = %+v, want the in-project ref with the canonical title", persisted)
	}
	// The project-narrowing scope rides the resolver call (never the request body).
	if resolver.capturedProjectID != "squad-a/proj" || resolver.capturedTeamID != team {
		t.Fatalf("resolver scope = {project %q, team %v}, want {squad-a/proj, %v}",
			resolver.capturedProjectID, resolver.capturedTeamID, team)
	}
}

// --- (2) out-of-project / unknown refs are dropped, never persisted ---------

func TestStampTicketRefsDropsOutOfProject(t *testing.T) {
	inProject := "11111111-1111-1111-1111-111111111111"
	outOfProject := "99999999-9999-9999-9999-999999999999"
	resolver := &fakeRefResolver{known: map[string]string{inProject: "In Project"}}
	h := NewHandlerWithDeps(nil, nil, nil)
	h.SetTicketRefResolver(resolver)

	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}
	refs := []TicketRef{{WorkItemID: outOfProject, Title: "elsewhere"}, {WorkItemID: inProject, Title: "here"}}

	payload := h.stampTicketRefs(context.Background(), "squad-a/proj", auth, refs, nil)

	persisted := ReferencesOf(payload)
	if len(persisted) != 1 || persisted[0].WorkItemID != inProject {
		t.Fatalf("persisted refs = %+v, want only the in-project ref (out-of-project dropped)", persisted)
	}

	// When EVERY ref is out-of-project the payload stays link-free (and untouched).
	onlyOut := h.stampTicketRefs(context.Background(), "squad-a/proj", auth,
		[]TicketRef{{WorkItemID: outOfProject}}, nil)
	if onlyOut != nil {
		t.Fatalf("all-out-of-project payload = %s, want nil (nothing persisted)", *onlyOut)
	}
}

// --- (3) an @-mention and a ticket reference coexist in one message ---------

func TestMentionAndTicketReferenceCoexist(t *testing.T) {
	inProject := "11111111-1111-1111-1111-111111111111"
	resolver := &fakeRefResolver{known: map[string]string{inProject: "The linked ticket"}}
	roster := []TeamAgent{{Name: "john", Status: "working"}}
	h := NewHandlerWithDeps(nil, nil, nil)
	h.SetTicketRefResolver(resolver)

	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}
	// One message @-mentions john (a dispatch) AND links a ticket (a reference).
	payload := h.stampTicketRefs(context.Background(), "squad-a/proj", auth,
		[]TicketRef{{WorkItemID: inProject, Title: "x"}}, nil)

	// The reference is persisted in the payload…
	if refs := ReferencesOf(payload); len(refs) != 1 || refs[0].WorkItemID != inProject {
		t.Fatalf("payload refs = %+v, want the linked ticket", refs)
	}
	// …and the @-mention still dispatches john off the body, unaffected by the reference payload.
	msg := agentMsg("@john please look at this ticket", "party", "", payload)
	targets, dropped := resolveMentionTargets(msg, roster)
	if dropped != 0 || len(targets) != 1 || targets[0].AgentName != "john" {
		t.Fatalf("dispatch = %v (dropped %d), want exactly [john] alongside the reference", names(targets), dropped)
	}
	// The reference itself is NOT a dispatch: no ref UUID leaked into the dispatch targets.
	for _, tgt := range targets {
		if tgt.AgentName == inProject {
			t.Fatal("a ticket reference must never become a dispatch target")
		}
	}
}

// --- best-effort degrades: nil resolver, resolver error, no candidates ------

func TestStampTicketRefsBestEffort(t *testing.T) {
	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}
	refs := []TicketRef{{WorkItemID: "11111111-1111-1111-1111-111111111111", Title: "x"}}

	// Nil resolver: references are dropped (room stays link-free), payload untouched.
	noResolver := NewHandlerWithDeps(nil, nil, nil)
	if got := noResolver.stampTicketRefs(context.Background(), "p", auth, refs, nil); got != nil {
		t.Fatalf("nil-resolver payload = %s, want nil (references dropped)", *got)
	}

	// Resolver error degrades to a link-free but durable message (the payload passes through).
	base := json.RawMessage(`{"foo":"bar"}`)
	errResolver := NewHandlerWithDeps(nil, nil, nil)
	errResolver.SetTicketRefResolver(&fakeRefResolver{err: errors.New("coord down")})
	got := errResolver.stampTicketRefs(context.Background(), "p", auth, refs, &base)
	if got == nil || ReferencesOf(got) != nil {
		t.Fatalf("resolver-error payload = %v, want the original payload with no references", got)
	}

	// No candidates (all malformed): the resolver is never consulted; payload passes through.
	okResolver := &fakeRefResolver{known: map[string]string{}}
	h := NewHandlerWithDeps(nil, nil, nil)
	h.SetTicketRefResolver(okResolver)
	h.stampTicketRefs(context.Background(), "p", auth, []TicketRef{{WorkItemID: "not-a-uuid"}}, nil)
	if okResolver.capturedRefs != nil {
		t.Fatalf("resolver consulted with %+v, want no call for an all-malformed candidate set", okResolver.capturedRefs)
	}
}
