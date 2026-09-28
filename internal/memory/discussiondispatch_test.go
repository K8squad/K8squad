package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// Dispatch-on-mention parity for the discussion_post MCP tool (ISI-5125). These prove the tool path fires
// the SAME trigger + loop-guard hop stamp the REST endpoint fires (via the shared discussion.* free
// functions), so a dispatched Run that replies through the tool still chains agent→agent and stays bounded.

// mentionCaptureWriter is a DiscussionWriter that echoes the payload + author identity into the returned
// Message (as the real store does), so a test can assert BOTH the pre-write hop stamp (captured payload)
// and the post-write dispatch decision (which reads the returned message).
type mentionCaptureWriter struct {
	gotPayload *json.RawMessage
	gotAuth    discussion.AuthorContext
}

func (w *mentionCaptureWriter) OpenThread(_ context.Context, projectID string, auth discussion.AuthorContext, title, body string) (*discussion.Thread, error) {
	return &discussion.Thread{ID: uuid.MustParse(testThreadID), ProjectID: projectID, TeamID: auth.TeamID, Title: title}, nil
}

func (w *mentionCaptureWriter) PostMessage(_ context.Context, _ string, _, threadID uuid.UUID, auth discussion.AuthorContext, body string, parentID *uuid.UUID, _ *string, _ *string, payload *json.RawMessage) (*discussion.Message, error) {
	w.gotPayload = payload
	w.gotAuth = auth
	return &discussion.Message{
		ID:              uuid.MustParse("99999999-9999-9999-9999-999999999999"),
		ThreadID:        threadID,
		ParentID:        parentID,
		AuthorPrincipal: auth.Principal,
		AuthorAgentID:   auth.AgentID, // agent-vs-human is derived from this, as the store does
		AuthorRunID:     auth.RunID,
		Body:            body,
		Payload:         payload, // echo the stamped payload so the dispatch decision reads the hop
		CreatedAt:       time.Unix(2, 0).UTC(),
	}, nil
}

// captureDispatcher records every DispatchMention it receives.
type captureDispatcher struct{ calls []discussion.MentionDispatch }

func (d *captureDispatcher) DispatchMention(_ context.Context, m discussion.MentionDispatch) error {
	d.calls = append(d.calls, m)
	return nil
}

// fixedHopResolver maps a specific runID to a dispatched-run hop; any other run is not a thread-run.
type fixedHopResolver struct {
	runID string
	hop   int
}

func (r fixedHopResolver) HopForDispatchedRun(_ context.Context, runID string) (int, bool, error) {
	if runID == r.runID {
		return r.hop, true, nil
	}
	return 0, false, nil
}

// staticRoster resolves a fixed set of agent names for any team (Status empty = opt-out degraded, the
// documented tool-path behaviour).
type staticRoster struct{ names []string }

func (s staticRoster) RosterForTeam(_ context.Context, _ uuid.UUID) ([]discussion.TeamAgent, error) {
	out := make([]discussion.TeamAgent, 0, len(s.names))
	for _, n := range s.names {
		out = append(out, discussion.TeamAgent{Name: n})
	}
	return out, nil
}

func mentionToolServer(t *testing.T, dw DiscussionWriter, dd *DiscussionDispatch) *httptest.Server {
	t.Helper()
	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil, dw).WithDiscussionDispatch(dd)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestToolMentionDispatchHumanReply is AC1: a @-mention in a reply posted via the discussion_post tool by a
// HUMAN dispatches exactly one Run for the named agent, at hop 1 — parity with the REST postMessage path.
func TestToolMentionDispatchHumanReply(t *testing.T) {
	dw := &mentionCaptureWriter{}
	disp := &captureDispatcher{}
	dd := NewDiscussionDispatch(disp, fixedHopResolver{}, staticRoster{names: []string{"Robo-Coder", "Reviewer"}})
	srv := mentionToolServer(t, dw, dd)

	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"@Robo-Coder please take a look"}`,
		map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "human:henrik"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if len(disp.calls) != 1 {
		t.Fatalf("dispatched %d runs, want exactly 1 (parity with REST)", len(disp.calls))
	}
	c := disp.calls[0]
	if c.AgentName != "Robo-Coder" {
		t.Fatalf("dispatched agent = %q, want Robo-Coder", c.AgentName)
	}
	if c.HopDepth != 1 {
		t.Fatalf("human-triggered dispatch hop = %d, want 1", c.HopDepth)
	}
	if c.ThreadID.String() != testThreadID {
		t.Fatalf("dispatch thread = %s, want %s", c.ThreadID, testThreadID)
	}
	if c.ProjectID != "squad-a/proj" || c.TeamID.String() != testTeamID {
		t.Fatalf("dispatch project/team = %q/%s, want squad-a/proj/%s", c.ProjectID, c.TeamID, testTeamID)
	}
	if c.TriggeredByAgentID != nil {
		t.Fatalf("human trigger should carry no agent id, got %v", c.TriggeredByAgentID)
	}
	// A human post is hop 0 and is not stamped (no Run identity).
	if dw.gotPayload != nil {
		t.Fatalf("human post payload = %s, want nil (no hop stamp)", string(*dw.gotPayload))
	}
}

// TestToolMentionAgentReplyIncrementsHop is AC2 (chaining): an agent→agent reply via the tool, from a run
// dispatched at hop 1, carries the stamped hop and dispatches the next agent at hop 2 (bounded, not reset).
func TestToolMentionAgentReplyIncrementsHop(t *testing.T) {
	dw := &mentionCaptureWriter{}
	disp := &captureDispatcher{}
	dd := NewDiscussionDispatch(disp, fixedHopResolver{runID: "run-hop1", hop: 1}, staticRoster{names: []string{"Reviewer"}})
	srv := mentionToolServer(t, dw, dd)

	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"@Reviewer over to you"}`,
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
	// Pre-write stamp: the stored reply carries hopDepth=1 (the run's dispatched hop), from the run's
	// identity — not an agent-supplied number.
	if dw.gotPayload == nil {
		t.Fatal("agent reply from a dispatched run must be hop-stamped, got nil payload")
	}
	if got := discussion.DispatchHopOf(dw.gotPayload); got != 1 {
		t.Fatalf("stamped hop = %d, want 1 (the run's dispatched hop)", got)
	}
	// Post-write dispatch: the next agent runs at hop 2.
	if len(disp.calls) != 1 {
		t.Fatalf("dispatched %d runs, want 1", len(disp.calls))
	}
	if disp.calls[0].HopDepth != 2 {
		t.Fatalf("agent→agent dispatch hop = %d, want 2 (incremented, not reset)", disp.calls[0].HopDepth)
	}
	if disp.calls[0].TriggeredByAgentID == nil {
		t.Fatal("agent-triggered dispatch must carry the triggering agent id (initiator=agent)")
	}
}

// TestToolMentionHopThreeRefused is AC2 (bound): a reply from a run already at hop 2 would dispatch at hop
// 3, which exceeds the cap — so the tool path dispatches NOBODY, closing the paid loop end-to-end.
func TestToolMentionHopThreeRefused(t *testing.T) {
	dw := &mentionCaptureWriter{}
	disp := &captureDispatcher{}
	dd := NewDiscussionDispatch(disp, fixedHopResolver{runID: "run-hop2", hop: 2}, staticRoster{names: []string{"Reviewer"}})
	srv := mentionToolServer(t, dw, dd)

	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"@Reviewer one more pass"}`,
		map[string]string{
			"X-Team-Id":      testTeamID,
			"X-Principal-Id": "agent:reviewer",
			"X-Agent-Id":     "agent-uuid-reviewer",
			"X-Run-Id":       "run-hop2",
		})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	// The reply is still stamped (hop 2) and still committed — only the onward dispatch is refused.
	if got := discussion.DispatchHopOf(dw.gotPayload); got != 2 {
		t.Fatalf("stamped hop = %d, want 2", got)
	}
	if len(disp.calls) != 0 {
		t.Fatalf("dispatched %d runs, want 0 (hop 3 refused — loop bound)", len(disp.calls))
	}
}

// TestToolMentionUnwiredIsInert proves the nil-dispatch path: without WithDiscussionDispatch the tool posts
// exactly as before — no stamp, no dispatch, no panic (a DB-less/cluster-less deployment).
func TestToolMentionUnwiredIsInert(t *testing.T) {
	dw := &mentionCaptureWriter{}
	srv := mentionToolServer(t, dw, nil)

	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"squad-a/proj","thread_id":"`+testThreadID+`","body":"@Reviewer hi"}`,
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
	if dw.gotPayload != nil {
		t.Fatalf("unwired tool must not stamp, got %s", string(*dw.gotPayload))
	}
}
