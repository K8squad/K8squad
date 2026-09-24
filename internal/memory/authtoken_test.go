/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/mcpauthtoken"
)

// authtoken_test.go — the ADR-0024a S3/D2 token-auth edge suite (ISI-4869). It
// proves the security boundary the story demands: on the token (sandbox) path
// identity and capabilities come ONLY from the verified run capability token and
// every inbound X-* header is discarded, while the header (BFF) path is
// unchanged. These run over the REAL resolvers (Header + Token) and a real
// minter — only the coord backend is faked.

var tokTestKey = []byte("0123456789abcdef0123456789abcdef") // 32-byte HS256 key

// mountTokenMCP wires the authoring edge with BOTH the header resolver (BFF path)
// and the token-auth (sandbox) path enabled over a real minter, returning the
// mux and the minter so tests mint tokens the edge will verify with the same key.
func mountTokenMCP(t *testing.T, author WorkItemAuthor, dispatcher WorkItemDispatcher) (*http.ServeMux, *mcpauthtoken.Minter) {
	t.Helper()
	minter, err := mcpauthtoken.NewMinter(tokTestKey, time.Hour)
	if err != nil {
		t.Fatalf("new minter: %v", err)
	}
	mux := http.NewServeMux()
	NewToolMCP(nil, nil, nil).
		WithWorkItemAuthor(author, dispatcher, NewHeaderCapabilityResolver()).
		WithAuthoringTokenAuth(NewMCPAuthTokenVerifier(minter)).
		Mount(mux)
	return mux, minter
}

const createBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"work_item_create","arguments":{"parent_id":"p1","title":"t"}}}`

func createWithAssigneeBody(assignee string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"work_item_create","arguments":{"parent_id":"p1","title":"t","assignee_agent_id":"` + assignee + `"}}}`
}

// TestMCP_TokenPath_ForgedHeadersCannotSelfGrant is THE negative test (AC): a
// sandbox presenting a VALID token that carries NO work_item.author capability,
// paired with a forged `X-Agent-Capabilities: work_item.author` header (and
// forged identity headers), cannot self-grant — the token path strips every X-*
// header, capabilities come only from the (unprivileged) token, so the author
// gate denies and coord is never touched.
func TestMCP_TokenPath_ForgedHeadersCannotSelfGrant(t *testing.T) {
	author := &fakeAuthor{}
	mux, minter := mountTokenMCP(t, author, nil)

	// Token grants NOTHING (empty capabilities) but is otherwise fully bound.
	tok, err := minter.Mint(mcpauthtoken.Claims{
		TeamID:    "team-token",
		Principal: "quill",
		AgentID:   "agent-token",
		RunID:     "run-token",
		// Capabilities intentionally empty — no work_item.author.
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	resp := rpcCall(t, mux, map[string]string{
		"Authorization":        "Bearer " + tok,
		"X-Agent-Capabilities": "work_item.author", // forged — must be ignored
		"X-Team-Id":            "team-forged",      // forged
		"X-Agent-Id":           "evil",             // forged
		"X-Run-Id":             "evil-run",         // forged
		"X-Principal-Id":       "evil",             // forged
	}, createBody)

	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}
	text, isErr := resultContentText(t, resp.Result)
	if !isErr {
		t.Fatalf("forged X-Agent-Capabilities self-grant SUCCEEDED — security boundary breached: %s", text)
	}
	if !strings.Contains(text, "capability denied") {
		t.Fatalf("expected capability-denied, got: %s", text)
	}
	if author.createCalled {
		t.Fatalf("coord was touched despite denial — create must not run when capability is denied")
	}
}

// TestMCP_TokenPath_GrantsAndIdentityFromToken proves the positive path: a token
// carrying work_item.author authorizes the call, and the identity/tenancy used
// downstream come from the VERIFIED token — never the forged headers sent
// alongside it.
func TestMCP_TokenPath_GrantsAndIdentityFromToken(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "child-1"}}
	dispatcher := &fakeDispatcher{}
	mux, minter := mountTokenMCP(t, author, dispatcher)

	tok, err := minter.Mint(mcpauthtoken.Claims{
		TeamID:       "team-token",
		Principal:    "quill",
		AgentID:      "agent-token",
		RunID:        "run-token",
		Capabilities: []string{WorkItemAuthorCapability},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Send with a forged X-Team-Id / identity to prove they are ignored; use the
	// create-and-assign variant so the team threads into the dispatch input where
	// we can assert it.
	resp := rpcCall(t, mux, map[string]string{
		"Authorization":        "Bearer " + tok,
		"X-Team-Id":            "team-forged",
		"X-Agent-Id":           "evil",
		"X-Run-Id":             "evil-run",
		"X-Principal-Id":       "evil",
		"X-Agent-Capabilities": "work_item.author",
	}, createWithAssigneeBody("impl-agent"))

	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("token-granted create should succeed")
	}
	if !author.createCalled {
		t.Fatalf("create was not called")
	}
	if author.createIn.AgentName != "agent-token" || author.createIn.RunID != "run-token" || author.createIn.Principal != "quill" {
		t.Fatalf("create identity not from token: %+v", author.createIn)
	}
	if !dispatcher.called {
		t.Fatalf("assign was not dispatched")
	}
	if dispatcher.in.TeamID != "team-token" {
		t.Fatalf("dispatch team not from token (forged header leaked?): got %q want team-token", dispatcher.in.TeamID)
	}
	if dispatcher.in.AgentName != "agent-token" || dispatcher.in.RunID != "run-token" {
		t.Fatalf("dispatch identity not from token: %+v", dispatcher.in)
	}
}

// TestMCP_HeaderPath_BFFRegression proves the trusted BFF header path is
// unchanged even when the token-auth mode is wired: a request with NO
// Authorization header is authenticated from the BFF-stamped X-* headers exactly
// as before.
func TestMCP_HeaderPath_BFFRegression(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "child-1"}}
	mux, _ := mountTokenMCP(t, author, nil)

	resp := rpcCall(t, mux, map[string]string{
		// no Authorization → header (BFF) path
		"X-Team-Id":            "team-bff",
		"X-Agent-Id":           "pm-john",
		"X-Run-Id":             "run-bff",
		"X-Principal-Id":       "agent:john",
		"X-Agent-Capabilities": "work_item.author",
	}, createBody)

	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("BFF header-path create should still succeed (regression)")
	}
	if !author.createCalled || author.createIn.AgentName != "pm-john" || author.createIn.RunID != "run-bff" {
		t.Fatalf("BFF header identity not honored: %+v", author.createIn)
	}
}

// TestMCP_TokenPath_InvalidTokenIs401NoFallback proves an invalid bearer token
// fails CLOSED to 401 and does NOT fall back to header trust — a sandbox cannot
// pair a junk token with forged capability headers to reach the header path.
func TestMCP_TokenPath_InvalidTokenIs401NoFallback(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "child-1"}}
	mux, _ := mountTokenMCP(t, author, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, MCPEndpoint, strings.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	req.Header.Set("X-Team-Id", "team-forged")
	req.Header.Set("X-Agent-Id", "evil")
	req.Header.Set("X-Run-Id", "evil-run")
	req.Header.Set("X-Principal-Id", "evil")
	req.Header.Set("X-Agent-Capabilities", "work_item.author")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token should 401 (no header fallback), got %d body=%q", rec.Code, rec.Body.String())
	}
	if author.createCalled {
		t.Fatalf("coord was touched on an invalid-token request — header fallback must not happen")
	}
}

// TestMCP_TokenAuthInertWhenUnwired proves the mode is inert until the signing
// key is distributed: with no token verifier wired, an Authorization header is
// IGNORED and the request is served on the header (BFF) path — behaviour is
// exactly as before S3.
func TestMCP_TokenAuthInertWhenUnwired(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "child-1"}}
	mux := http.NewServeMux()
	// WithWorkItemAuthor only — no WithAuthoringTokenAuth.
	NewToolMCP(nil, nil, nil).
		WithWorkItemAuthor(author, nil, NewHeaderCapabilityResolver()).
		Mount(mux)

	resp := rpcCall(t, mux, map[string]string{
		"Authorization":        "Bearer whatever", // ignored — no verifier wired
		"X-Team-Id":            "team-bff",
		"X-Agent-Id":           "pm-john",
		"X-Run-Id":             "run-bff",
		"X-Principal-Id":       "agent:john",
		"X-Agent-Capabilities": "work_item.author",
	}, createBody)

	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("with token-auth unwired, the header path should serve normally")
	}
	if !author.createCalled {
		t.Fatalf("header path should have authorized the create")
	}
}

// --- unit-level ------------------------------------------------------------

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc", // case-insensitive scheme
		"Bearer  abc": "abc", // trimmed
		"Basic abc":   "",
		"":            "",
		"Bearer":      "",
	}
	for in, want := range cases {
		if got := bearerToken(in); got != want {
			t.Fatalf("bearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTokenCapabilityResolver_RefusesNonTokenSession(t *testing.T) {
	r := NewTokenCapabilityResolver()
	// A session with the capability but NOT via a token must be refused (defense
	// in depth: header-sourced caps can never satisfy the token-path gate).
	if ok, _ := r.HasWorkItemAuthor(context.Background(), AgentSession{Capabilities: []string{WorkItemAuthorCapability}, ViaToken: false}); ok {
		t.Fatalf("token resolver granted a non-token session")
	}
	// Via a token WITH the cap → granted.
	if ok, _ := r.HasWorkItemAuthor(context.Background(), AgentSession{Capabilities: []string{WorkItemAuthorCapability}, ViaToken: true}); !ok {
		t.Fatalf("token resolver denied a token session holding the capability")
	}
	// Via a token WITHOUT the cap → denied.
	if ok, _ := r.HasWorkItemAuthor(context.Background(), AgentSession{ViaToken: true}); ok {
		t.Fatalf("token resolver granted a token session with no capabilities")
	}
}
