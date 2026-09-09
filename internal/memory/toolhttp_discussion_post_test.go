package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// fakeDiscussionWriter captures the exact (projectID, teamID, threadID, AuthorContext, body, parentID)
// each call receives, so the tests can prove the tool passes ONLY header-derived identity to the fenced
// store — never anything smuggled through the body (WINV1/WINV2, ISI-4013 AC3). It can also be armed to
// return a specific store error to exercise the status mapping.
type fakeDiscussionWriter struct {
	// captured OpenThread args
	openProject uuid.UUID
	openAuth    discussion.AuthorContext
	openTitle   string
	openBody    string
	openCalled  bool

	// captured PostMessage args
	postProject  uuid.UUID
	postTeam     uuid.UUID
	postThread   uuid.UUID
	postAuth     discussion.AuthorContext
	postBody     string
	postParentID *uuid.UUID
	postCalled   bool

	openErr error
	postErr error
}

func (f *fakeDiscussionWriter) OpenThread(_ context.Context, projectID uuid.UUID, auth discussion.AuthorContext, title, body string) (*discussion.Thread, error) {
	f.openCalled = true
	f.openProject, f.openAuth, f.openTitle, f.openBody = projectID, auth, title, body
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &discussion.Thread{
		ID:        uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		ProjectID: projectID,
		TeamID:    auth.TeamID,
		Title:     title,
		CreatedBy: auth.Principal,
		CreatedAt: time.Unix(1, 0).UTC(),
		Messages: []discussion.Message{{
			ID:              uuid.MustParse("22222222-2222-2222-2222-222222222222"),
			ThreadID:        uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			AuthorPrincipal: auth.Principal,
			AuthorAgentID:   auth.AgentID,
			AuthorRunID:     auth.RunID,
			Body:            body,
			CreatedAt:       time.Unix(1, 0).UTC(),
		}},
	}, nil
}

func (f *fakeDiscussionWriter) PostMessage(_ context.Context, projectID, teamID, threadID uuid.UUID, auth discussion.AuthorContext, body string, parentID *uuid.UUID) (*discussion.Message, error) {
	f.postCalled = true
	f.postProject, f.postTeam, f.postThread, f.postAuth, f.postBody, f.postParentID = projectID, teamID, threadID, auth, body, parentID
	if f.postErr != nil {
		return nil, f.postErr
	}
	return &discussion.Message{
		ID:              uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		ThreadID:        threadID,
		ParentID:        parentID,
		AuthorPrincipal: auth.Principal,
		AuthorAgentID:   auth.AgentID,
		AuthorRunID:     auth.RunID,
		Body:            body,
		CreatedAt:       time.Unix(2, 0).UTC(),
	}, nil
}

func newDiscussionToolServer(t *testing.T, dw DiscussionWriter) *httptest.Server {
	t.Helper()
	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil, dw)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const (
	testTeamID    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	testProjectID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	testThreadID  = "11111111-1111-1111-1111-111111111111"
)

func discussionPostReq(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return res
}

// TestDiscussionPostOpenThenReply is AC1/AC6: opening a thread (no thread_id) calls OpenThread and
// returns the stamped Thread; a follow-up with the returned thread_id calls PostMessage and returns the
// stamped Message. Both carry the agent identity from the headers.
func TestDiscussionPostOpenThenReply(t *testing.T) {
	dw := &fakeDiscussionWriter{}
	srv := newDiscussionToolServer(t, dw)
	hdrs := map[string]string{
		"X-Team-Id":      testTeamID,
		"X-Principal-Id": "agent:amelia",
		"X-Agent-Id":     "agent-uuid-1",
		"X-Run-Id":       "run-uuid-1",
	}

	// Open a thread.
	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"`+testProjectID+`","title":"Deploy plan","body":"first message"}`, hdrs)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("open status = %d, want 200", res.StatusCode)
	}
	if !dw.openCalled || dw.postCalled {
		t.Fatalf("open path should call OpenThread only (open=%v post=%v)", dw.openCalled, dw.postCalled)
	}
	if dw.openTitle != "Deploy plan" || dw.openBody != "first message" {
		t.Fatalf("open title/body = %q/%q", dw.openTitle, dw.openBody)
	}
	if dw.openProject.String() != testProjectID {
		t.Fatalf("open projectID = %s, want %s", dw.openProject, testProjectID)
	}
	if dw.openAuth.TeamID.String() != testTeamID || dw.openAuth.Principal != "agent:amelia" {
		t.Fatalf("open auth tenancy/principal not from headers: %+v", dw.openAuth)
	}
	if dw.openAuth.AgentID == nil || *dw.openAuth.AgentID != "agent-uuid-1" {
		t.Fatalf("open auth agent id not stamped from X-Agent-Id: %+v", dw.openAuth.AgentID)
	}
	if dw.openAuth.RunID == nil || *dw.openAuth.RunID != "run-uuid-1" {
		t.Fatalf("open auth run id not stamped from X-Run-Id: %+v", dw.openAuth.RunID)
	}
	var thread discussion.Thread
	if err := json.NewDecoder(res.Body).Decode(&thread); err != nil {
		t.Fatalf("decode thread: %v", err)
	}
	if thread.ID.String() != testThreadID {
		t.Fatalf("returned thread id = %s, want server-stamped %s", thread.ID, testThreadID)
	}

	// Reply into the returned thread.
	res2 := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"`+testProjectID+`","thread_id":"`+thread.ID.String()+`","body":"a reply"}`, hdrs)
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("reply status = %d, want 200", res2.StatusCode)
	}
	if !dw.postCalled {
		t.Fatalf("reply path should call PostMessage")
	}
	if dw.postThread.String() != testThreadID || dw.postBody != "a reply" {
		t.Fatalf("reply thread/body = %s/%q", dw.postThread, dw.postBody)
	}
	if dw.postTeam.String() != testTeamID {
		t.Fatalf("reply teamID = %s, want %s (from header)", dw.postTeam, testTeamID)
	}
	var msg discussion.Message
	if err := json.NewDecoder(res2.Body).Decode(&msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if msg.AuthorKind() != "agent" {
		t.Fatalf("message AuthorKind = %q, want agent (X-Agent-Id present)", msg.AuthorKind())
	}
}

// TestDiscussionPostReplyWithParent asserts parent_message_id is parsed and threaded through to the
// store as the reply target.
func TestDiscussionPostReplyWithParent(t *testing.T) {
	dw := &fakeDiscussionWriter{}
	srv := newDiscussionToolServer(t, dw)
	parent := "44444444-4444-4444-4444-444444444444"
	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post",
		`{"project_id":"`+testProjectID+`","thread_id":"`+testThreadID+`","body":"nested","parent_message_id":"`+parent+`"}`,
		map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "human:henrik"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if dw.postParentID == nil || dw.postParentID.String() != parent {
		t.Fatalf("parentID = %v, want %s", dw.postParentID, parent)
	}
	// No X-Agent-Id ⇒ human-authored (agent-vs-human is derived, never a flag).
	if dw.postAuth.AgentID != nil {
		t.Fatalf("AgentID should be nil for a human post, got %v", dw.postAuth.AgentID)
	}
}

// TestDiscussionPostForgedAuthorIgnored is AC2: a payload that smuggles author_*/created_by/team_id/
// author_agent_id/author_run_id must have NO effect — the captured AuthorContext comes only from the
// headers, and the malformed extras are silently dropped by the decoder (the request struct has no such
// fields).
func TestDiscussionPostForgedAuthorIgnored(t *testing.T) {
	dw := &fakeDiscussionWriter{}
	srv := newDiscussionToolServer(t, dw)
	body := `{
		"project_id":"` + testProjectID + `",
		"title":"t","body":"b",
		"author_principal":"attacker","author_agent_id":"attacker-agent","author_run_id":"attacker-run",
		"created_by":"attacker","team_id":"cccccccc-cccc-cccc-cccc-cccccccccccc"
	}`
	res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post", body, map[string]string{
		"X-Team-Id":      testTeamID,
		"X-Principal-Id": "agent:amelia",
		"X-Agent-Id":     "agent-uuid-1",
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if dw.openAuth.Principal != "agent:amelia" {
		t.Fatalf("Principal = %q, want the header identity (attacker value must be dropped)", dw.openAuth.Principal)
	}
	if dw.openAuth.TeamID.String() != testTeamID {
		t.Fatalf("TeamID = %s, want the header team (body team_id must be dropped)", dw.openAuth.TeamID)
	}
	if dw.openAuth.AgentID == nil || *dw.openAuth.AgentID != "agent-uuid-1" {
		t.Fatalf("AgentID = %v, want header X-Agent-Id (body author_agent_id must be dropped)", dw.openAuth.AgentID)
	}
	if dw.openAuth.RunID != nil {
		t.Fatalf("RunID = %v, want nil (no X-Run-Id header; body author_run_id must be dropped)", dw.openAuth.RunID)
	}
}

// TestDiscussionPostAuthAndValidation covers the 401/400/404 status mapping.
func TestDiscussionPostAuthAndValidation(t *testing.T) {
	valid := `{"project_id":"` + testProjectID + `","title":"t","body":"b"}`
	cases := []struct {
		name    string
		body    string
		headers map[string]string
		dwErr   func(*fakeDiscussionWriter)
		want    int
	}{
		{"missing team header ⇒ 401", valid, map[string]string{"X-Principal-Id": "p"}, nil, http.StatusUnauthorized},
		{"missing principal header ⇒ 401", valid, map[string]string{"X-Team-Id": testTeamID}, nil, http.StatusUnauthorized},
		{"malformed team header ⇒ 400", valid, map[string]string{"X-Team-Id": "not-a-uuid", "X-Principal-Id": "p"}, nil, http.StatusBadRequest},
		{"malformed project_id ⇒ 400", `{"project_id":"nope","title":"t","body":"b"}`, map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "p"}, nil, http.StatusBadRequest},
		{"malformed thread_id ⇒ 400", `{"project_id":"` + testProjectID + `","thread_id":"nope","body":"b"}`, map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "p"}, nil, http.StatusBadRequest},
		{"malformed parent_message_id ⇒ 400", `{"project_id":"` + testProjectID + `","thread_id":"` + testThreadID + `","body":"b","parent_message_id":"nope"}`, map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "p"}, nil, http.StatusBadRequest},
		{"empty title on open ⇒ 400", `{"project_id":"` + testProjectID + `","body":"b"}`, map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "p"}, func(f *fakeDiscussionWriter) { f.openErr = discussion.ErrEmptyTitle }, http.StatusBadRequest},
		{"empty body ⇒ 400", valid, map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "p"}, func(f *fakeDiscussionWriter) { f.openErr = discussion.ErrEmptyBody }, http.StatusBadRequest},
		{"cross-team reply ⇒ 404", `{"project_id":"` + testProjectID + `","thread_id":"` + testThreadID + `","body":"b"}`, map[string]string{"X-Team-Id": testTeamID, "X-Principal-Id": "p"}, func(f *fakeDiscussionWriter) { f.postErr = discussion.ErrThreadNotFound }, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dw := &fakeDiscussionWriter{}
			if tc.dwErr != nil {
				tc.dwErr(dw)
			}
			srv := newDiscussionToolServer(t, dw)
			res := discussionPostReq(t, srv.URL+"/mcp/tools/discussion_post", tc.body, tc.headers)
			defer res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.want)
			}
		})
	}
}

// TestDiscussionPostUnmountedWhenNil is AC5: a read-only deployment (nil discussion writer) does not
// expose discussion_post at all — the mux answers 404.
func TestDiscussionPostUnmountedWhenNil(t *testing.T) {
	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/discussion_post",
		strings.NewReader(`{"project_id":"`+testProjectID+`","title":"t","body":"b"}`))
	req.Header.Set("X-Team-Id", testTeamID)
	req.Header.Set("X-Principal-Id", "p")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (discussion_post unmounted)", rec.Code)
	}
}

// TestDiscussionPostMethodNotAllowed asserts non-POST is rejected.
func TestDiscussionPostMethodNotAllowed(t *testing.T) {
	srv := newDiscussionToolServer(t, &fakeDiscussionWriter{})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp/tools/discussion_post", nil)
	req.Header.Set("X-Team-Id", testTeamID)
	req.Header.Set("X-Principal-Id", "p")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
}
