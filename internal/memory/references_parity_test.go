package memory

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// Ticket-reference parity for the discussion_post tool (ISI-5166, plan ISI-5134 S2). A ticket reference is
// a LINK, not a dispatch: a reference posted through the MCP tool must land in Message.Payload.references
// IDENTICALLY to the REST path (ISI-5165), an out-of-project/unknown ref must be dropped, and a reference
// must NEVER dispatch. These ride the same fake-writer harness as the ISI-5125 dispatch-parity tests: the
// resolution seam is exercised through a fake resolver, and the stored payload is asserted from the
// capturing writer — the drift class this story closes is "REST persists refs, MCP silently doesn't".

// fakeMemRefResolver is the discussion.TicketRefResolver seam with a canned in-project corpus: `known`
// maps a work-item UUID to its canonical title; any candidate not in `known` is unknown/out-of-project and
// dropped. It captures the projectID/teamID/refs it was handed so a test can prove the project-narrowing
// scope rides the call (never the request body).
type fakeMemRefResolver struct {
	known             map[string]string
	err               error
	capturedProjectID string
	capturedTeamID    uuid.UUID
	capturedRefs      []discussion.TicketRef
}

func (f *fakeMemRefResolver) ResolveTicketRefs(_ context.Context, projectID string, teamID uuid.UUID, refs []discussion.TicketRef) ([]discussion.TicketRef, error) {
	f.capturedProjectID = projectID
	f.capturedTeamID = teamID
	f.capturedRefs = refs
	if f.err != nil {
		return nil, f.err
	}
	out := make([]discussion.TicketRef, 0, len(refs))
	for _, r := range refs {
		title, ok := f.known[r.WorkItemID]
		if !ok {
			continue // unknown / out-of-project — dropped, never persisted
		}
		out = append(out, discussion.TicketRef{WorkItemID: r.WorkItemID, Title: title})
	}
	return out, nil
}

const (
	refInProject    = "aaaaaaaa-1111-2222-3333-444444444444"
	refOutOfProject = "bbbbbbbb-9999-8888-7777-666666666666"
)

// TestToolTicketRefPersistsHTTP is the S2 AC over the HTTP tool edge: a ticket reference posted via
// discussion_post lands in Message.Payload.references with the resolver's canonical title, and the
// project-narrowing scope rides the resolver call — identical to the REST postMessage path.
func TestToolTicketRefPersistsHTTP(t *testing.T) {
	dw := &mentionCaptureWriter{}
	resolver := &fakeMemRefResolver{known: map[string]string{refInProject: "Canonical Title"}}
	dd := NewDiscussionDispatch(nil, nil, nil, resolver) // reference-only bundle: no dispatch, no hop
	srv := mentionToolServer(t, dw, dd)

	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"see this ticket",`+
			`"references":[{"workItemId":"`+refInProject+`","title":"stale client title"}]}`,
		map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "human:henrik"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	persisted := discussion.ReferencesOf(dw.gotPayload)
	if len(persisted) != 1 || persisted[0].WorkItemID != refInProject || persisted[0].Title != "Canonical Title" {
		t.Fatalf("persisted refs = %+v, want the in-project ref with the canonical title", persisted)
	}
	if resolver.capturedProjectID != "squad-a/proj" || resolver.capturedTeamID.String() != testTeamID {
		t.Fatalf("resolver scope = {project %q, team %v}, want {squad-a/proj, %s}",
			resolver.capturedProjectID, resolver.capturedTeamID, testTeamID)
	}
}

// TestToolTicketRefDropsOutOfProject is the S2 drop AC: an out-of-project/unknown ref never persists, and a
// post whose refs are ALL out-of-project stays link-free (no `references` key) — same as the REST path.
func TestToolTicketRefDropsOutOfProject(t *testing.T) {
	dw := &mentionCaptureWriter{}
	resolver := &fakeMemRefResolver{known: map[string]string{refInProject: "In Project"}}
	dd := NewDiscussionDispatch(nil, nil, nil, resolver)
	srv := mentionToolServer(t, dw, dd)

	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"mixed refs",`+
			`"references":[{"workItemId":"`+refOutOfProject+`","title":"elsewhere"},{"workItemId":"`+refInProject+`","title":"here"}]}`,
		map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "human:henrik"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	persisted := discussion.ReferencesOf(dw.gotPayload)
	if len(persisted) != 1 || persisted[0].WorkItemID != refInProject {
		t.Fatalf("persisted refs = %+v, want only the in-project ref (out-of-project dropped)", persisted)
	}

	// All out-of-project ⇒ the payload stays link-free (nil), exactly as a plain post.
	dw2 := &mentionCaptureWriter{}
	srv2 := mentionToolServer(t, dw2, NewDiscussionDispatch(nil, nil, nil, &fakeMemRefResolver{known: map[string]string{}}))
	res2 := discussionPostReq(t, srv2.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"no valid refs",`+
			`"references":[{"workItemId":"`+refOutOfProject+`"}]}`,
		map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "human:henrik"})
	defer res2.Body.Close()
	if dw2.gotPayload != nil {
		t.Fatalf("all-out-of-project payload = %s, want nil (nothing persisted)", string(*dw2.gotPayload))
	}
}

// TestToolTicketRefNilResolverDrops proves the best-effort degrade the REST path shares: with no resolver
// wired (the state until ISI-5170 lands the concrete resolver on BOTH edges) a referenced ticket degrades
// to a link-free but durable message — true parity, no regression.
func TestToolTicketRefNilResolverDrops(t *testing.T) {
	dw := &mentionCaptureWriter{}
	// A bundle with a hop resolver but NO ref resolver: dispatch/hop still work, references are dropped.
	dd := NewDiscussionDispatch(nil, fixedHopResolver{}, nil, nil)
	srv := mentionToolServer(t, dw, dd)

	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"link me",`+
			`"references":[{"workItemId":"`+refInProject+`","title":"x"}]}`,
		map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "human:henrik"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if dw.gotPayload != nil {
		t.Fatalf("nil-resolver payload = %s, want nil (references dropped, no regression)", string(*dw.gotPayload))
	}
}

// TestToolTicketRefErrorBestEffort proves a resolver error never fails the tool call: the message still
// commits, just link-free — the reference is durable metadata, not a write fence.
func TestToolTicketRefErrorBestEffort(t *testing.T) {
	dw := &mentionCaptureWriter{}
	dd := NewDiscussionDispatch(nil, nil, nil, &fakeMemRefResolver{err: errors.New("coord down")})
	srv := mentionToolServer(t, dw, dd)

	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"link me",`+
			`"references":[{"workItemId":"`+refInProject+`","title":"x"}]}`,
		map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "human:henrik"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort — a resolver error never fails the post)", res.StatusCode)
	}
	if dw.gotPayload != nil {
		t.Fatalf("resolver-error payload = %s, want nil (link-free but durable)", string(*dw.gotPayload))
	}
}

// TestToolTicketRefCoexistsWithHopAndDispatch is the coexistence AC: an agent reply that BOTH @-mentions an
// agent AND links a ticket carries the loop-guard hop and the reference in ONE payload, dispatches the
// mentioned agent (hop incremented), and never dispatches the referenced ticket. This is the tool-path
// analogue of the REST TestMentionAndTicketReferenceCoexist.
func TestToolTicketRefCoexistsWithHopAndDispatch(t *testing.T) {
	dw := &mentionCaptureWriter{}
	disp := &captureDispatcher{}
	resolver := &fakeMemRefResolver{known: map[string]string{refInProject: "The linked ticket"}}
	dd := NewDiscussionDispatch(disp, fixedHopResolver{runID: "run-hop1", hop: 1}, staticRoster{names: []string{"Reviewer"}}, resolver)
	srv := mentionToolServer(t, dw, dd)

	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"@Reviewer see this ticket",`+
			`"references":[{"workItemId":"`+refInProject+`","title":"x"}]}`,
		map[string]string{
			"X-Team-Id":      testTeamID,
			"X-Principal-Id": "agent:robo",
			"X-Agent-Id":     "agent-uuid-robo",
			"X-Run-Id":       "run-hop1",
		})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	// One payload carries BOTH the hop stamp and the ticket link (JSON-merge, neither clobbers the other).
	if got := discussion.DispatchHopOf(dw.gotPayload); got != 1 {
		t.Fatalf("stamped hop = %d, want 1 (the run's dispatched hop)", got)
	}
	refs := discussion.ReferencesOf(dw.gotPayload)
	if len(refs) != 1 || refs[0].WorkItemID != refInProject {
		t.Fatalf("payload refs = %+v, want the linked ticket alongside the hop", refs)
	}
	// The @-mention dispatches Reviewer at hop 2; the referenced ticket is NOT a dispatch target.
	if len(disp.calls) != 1 || disp.calls[0].AgentName != "Reviewer" {
		t.Fatalf("dispatch = %+v, want exactly [Reviewer] (the reference must not dispatch)", disp.calls)
	}
	if disp.calls[0].HopDepth != 2 {
		t.Fatalf("agent→agent dispatch hop = %d, want 2 (incremented, not reset)", disp.calls[0].HopDepth)
	}
}

// TestToolTicketRefPersistsMCP proves the SAME persistence over the MCP JSON-RPC edge (callDiscussionPost),
// not just the HTTP shim — the two tool transports share the one stampTicketRefs hook, so a reference posted
// through either lands in Payload.references identically.
func TestToolTicketRefPersistsMCP(t *testing.T) {
	dw := &mentionCaptureWriter{}
	resolver := &fakeMemRefResolver{known: map[string]string{refInProject: "Canonical Title"}}
	dd := NewDiscussionDispatch(nil, nil, nil, resolver)
	m := NewToolMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil, dw).WithDiscussionDispatch(dd)

	agentID, runID := "agent-uuid-robo", "run-x"
	sess := mcpSession{
		team:      testTeamID,
		principal: "agent:robo",
		agentID:   &agentID,
		runID:     &runID,
	}
	raw := []byte(`{"project_id":"squad-a/proj","thread_id":"` + testThreadID + `","body":"see this ticket",` +
		`"references":[{"workItemId":"` + refInProject + `","title":"stale"}]}`)

	_, rpcErr := m.callDiscussionPost(context.Background(), sess, raw)
	if rpcErr != nil {
		t.Fatalf("callDiscussionPost rpc error = %+v, want nil", rpcErr)
	}
	persisted := discussion.ReferencesOf(dw.gotPayload)
	if len(persisted) != 1 || persisted[0].WorkItemID != refInProject || persisted[0].Title != "Canonical Title" {
		t.Fatalf("MCP-persisted refs = %+v, want the in-project ref with the canonical title", persisted)
	}
	if resolver.capturedProjectID != "squad-a/proj" || resolver.capturedTeamID.String() != testTeamID {
		t.Fatalf("resolver scope = {project %q, team %v}, want {squad-a/proj, %s}",
			resolver.capturedProjectID, resolver.capturedTeamID, testTeamID)
	}
}
