package decisionraise

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

type fakeResolver struct {
	projectID string
	teamID    uuid.UUID
	threadID  uuid.UUID
	ok        bool
	err       error
	calls     int
}

func (f *fakeResolver) ThreadForDispatchedRun(_ context.Context, runID string) (string, uuid.UUID, uuid.UUID, bool, error) {
	f.calls++
	return f.projectID, f.teamID, f.threadID, f.ok, f.err
}

type fakePoster struct {
	calls         int
	projectID     string
	teamID        uuid.UUID
	threadID      uuid.UUID
	auth          discussion.AuthorContext
	body          string
	payload       discussion.DecisionRequestPayload
	workItemID    string
	idempotency   string
	postedMessage *discussion.Message
	err           error
}

func (f *fakePoster) PostDecisionRequest(_ context.Context, projectID string, teamID, threadID uuid.UUID, auth discussion.AuthorContext, body string, payload discussion.DecisionRequestPayload, _ *discussion.DecisionTarget, workItemID, idempotencyKey string, _ *uuid.UUID) (*discussion.Message, error) {
	f.calls++
	f.projectID, f.teamID, f.threadID = projectID, teamID, threadID
	f.auth, f.body, f.payload = auth, body, payload
	f.workItemID, f.idempotency = workItemID, idempotencyKey
	if f.err != nil {
		return nil, f.err
	}
	if f.postedMessage == nil {
		f.postedMessage = &discussion.Message{ThreadID: threadID}
	}
	return f.postedMessage, nil
}

func TestRequestDecisionPostsCardIntoRaisingThread(t *testing.T) {
	projectID := "ns/proj"
	teamID, threadID := uuid.New(), uuid.New()
	runID := "11111111-1111-1111-1111-111111111111"
	res := &fakeResolver{projectID: projectID, teamID: teamID, threadID: threadID, ok: true}
	post := &fakePoster{}
	r, err := New(res, post)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msg, err := r.RequestDecision(context.Background(), runID, "alfred", "wi-1", discussion.DecisionRaise{
		Title:   "Which region?",
		Options: []string{"eu", "us"},
	})
	if err != nil {
		t.Fatalf("RequestDecision: %v", err)
	}
	if msg == nil || post.calls != 1 {
		t.Fatalf("poster calls = %d", post.calls)
	}
	if post.projectID != projectID || post.teamID != teamID || post.threadID != threadID {
		t.Fatalf("poster tenancy = %s/%s/%s", post.projectID, post.teamID, post.threadID)
	}
	if post.workItemID != "wi-1" {
		t.Fatalf("workItemID = %q", post.workItemID)
	}
	if post.auth.Principal != "alfred" || post.auth.AgentID == nil || *post.auth.AgentID != "alfred" {
		t.Fatalf("auth = %+v", post.auth)
	}
	if post.auth.RunID == nil || *post.auth.RunID != runID {
		t.Fatalf("auth.RunID = %+v", post.auth.RunID)
	}
	if len(post.payload.Options) != 2 || post.payload.Options[0].ID != "a" || post.payload.Options[1].Label != "b) us" {
		t.Fatalf("payload options = %+v", post.payload.Options)
	}
	if !strings.HasPrefix(post.idempotency, "decision-raise:"+runID+":") {
		t.Fatalf("idempotency = %q", post.idempotency)
	}
	if !strings.Contains(post.body, "Which region?") {
		t.Fatalf("body = %q", post.body)
	}
}

func TestRequestDecisionIdempotencyKeyDeterministic(t *testing.T) {
	res := &fakeResolver{ok: true}
	post := &fakePoster{}
	r, _ := New(res, post)
	ask := discussion.DecisionRaise{Title: "T", Options: []string{"a", "b"}}
	if _, err := r.RequestDecision(context.Background(), "run-1", "p", "wi", ask); err != nil {
		t.Fatalf("first: %v", err)
	}
	first := post.idempotency
	if _, err := r.RequestDecision(context.Background(), "run-1", "p", "wi", ask); err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != post.idempotency {
		t.Fatalf("idempotency drifted: %q vs %q", first, post.idempotency)
	}
	// A different ask must derive a different key.
	if _, err := r.RequestDecision(context.Background(), "run-1", "p", "wi", discussion.DecisionRaise{Title: "T2", Options: []string{"a", "b"}}); err != nil {
		t.Fatalf("third: %v", err)
	}
	if first == post.idempotency {
		t.Fatalf("different ask produced the same idempotency key %q", first)
	}
}

func TestRequestDecisionRefusesWithoutThread(t *testing.T) {
	res := &fakeResolver{ok: false}
	post := &fakePoster{}
	r, _ := New(res, post)
	_, err := r.RequestDecision(context.Background(), "run-1", "p", "wi", discussion.DecisionRaise{Title: "T", Options: []string{"a", "b"}})
	if !errors.Is(err, ErrNoRaisingThread) {
		t.Fatalf("err = %v, want ErrNoRaisingThread", err)
	}
	if post.calls != 0 {
		t.Fatalf("poster must not be called on refusal, got %d", post.calls)
	}
}

func TestRequestDecisionValidatesBeforeResolving(t *testing.T) {
	res := &fakeResolver{ok: true}
	post := &fakePoster{}
	r, _ := New(res, post)
	_, err := r.RequestDecision(context.Background(), "run-1", "p", "wi", discussion.DecisionRaise{Title: "T", Options: []string{"only"}})
	if !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("err = %v, want ErrInvalidDecisionPayload", err)
	}
	if res.calls != 0 || post.calls != 0 {
		t.Fatalf("an invalid ask must short-circuit before any seam: resolver=%d poster=%d", res.calls, post.calls)
	}
}

func TestNewRefusesNilSeams(t *testing.T) {
	if _, err := New(nil, &fakePoster{}); err == nil {
		t.Fatal("nil resolver must refuse")
	}
	if _, err := New(&fakeResolver{}, nil); err == nil {
		t.Fatal("nil poster must refuse")
	}
}
