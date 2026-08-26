package memory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestToolHTTP_TeamFromHeaderNotBody asserts the transport takes the caller tenant from the
// X-Team-Id header (server-authenticated), never from the request body — so a body cannot widen or
// spoof the tenant (INV3 at the edge). The recorded SearchQuery must carry the HEADER team.
func TestToolHTTP_TeamFromHeaderNotBody(t *testing.T) {
	fake := &fakeSearcher{}
	h := NewToolHTTP(NewReadService(fake, NewHashingEmbedder()), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The body tries to smuggle a different team; the handler must ignore any such field and use the header.
	body := `{"project_id":"proj-A","query":"deploy","top_k":5,"team_id":"attacker-team"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/tools/discussion_search", strings.NewReader(body))
	req.Header.Set("X-Team-Id", "team-1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var out searchResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fake.got.SquadID != "team-1" {
		t.Fatalf("SquadID = %q, want team-1 (from header, never the body)", fake.got.SquadID)
	}
	if fake.got.ProjectID == nil || *fake.got.ProjectID != "proj-A" {
		t.Fatalf("ProjectID = %v, want proj-A", fake.got.ProjectID)
	}
}

// TestToolHTTP_WriteScopeFromHeadersNotBody is the write-path edge of WINV1/WINV2: memory_write stamps
// tenancy and authorship from the server headers, never the body. A body that smuggles team_id/principal
// is ignored; the recorded WriteRequest carries the HEADER identity.
func TestToolHTTP_WriteScopeFromHeadersNotBody(t *testing.T) {
	fw := &fakeWriter{}
	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), NewWriteService(fw, NewHashingEmbedder()))
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{"kind":"note","content":"metallb pool is 192.168.1.240/28","project_id":"proj-A","team_id":"attacker","principal":"attacker"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/tools/memory_write", strings.NewReader(body))
	req.Header.Set("X-Team-Id", "team-1")
	req.Header.Set("X-Principal-Id", "agent:coder")
	req.Header.Set("X-Agent-Id", "agent-uuid")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if fw.got.SquadID != "team-1" {
		t.Fatalf("SquadID = %q, want team-1 (from header)", fw.got.SquadID)
	}
	if fw.got.PrincipalID != "agent:coder" {
		t.Fatalf("PrincipalID = %q, want agent:coder (from header)", fw.got.PrincipalID)
	}
	if fw.got.ProjectID == nil || *fw.got.ProjectID != "proj-A" {
		t.Fatalf("ProjectID = %v, want proj-A", fw.got.ProjectID)
	}
}

// TestToolHTTP_WriteRequiresPrincipalHeader asserts a write missing X-Principal-Id (an unauthenticated
// author) is rejected even when the team header is present.
func TestToolHTTP_WriteRequiresPrincipalHeader(t *testing.T) {
	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), NewWriteService(&fakeWriter{}, NewHashingEmbedder()))
	mux := http.NewServeMux()
	h.Mount(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/memory_write", strings.NewReader(`{"content":"x"}`))
	req.Header.Set("X-Team-Id", "team-1")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without X-Principal-Id", rec.Code)
	}
}

// TestToolHTTP_WriteUnmountedWhenNil asserts a read-only deployment (nil write service) does not expose
// memory_write at all.
func TestToolHTTP_WriteUnmountedWhenNil(t *testing.T) {
	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/memory_write", strings.NewReader(`{"content":"x"}`))
	req.Header.Set("X-Team-Id", "team-1")
	req.Header.Set("X-Principal-Id", "p")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (memory_write unmounted)", rec.Code)
	}
}

// TestToolHTTP_RequiresTeamHeader asserts an unauthenticated call (no X-Team-Id) is rejected.
func TestToolHTTP_RequiresTeamHeader(t *testing.T) {
	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/discussion_search",
		strings.NewReader(`{"project_id":"p","query":"q"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without X-Team-Id", rec.Code)
	}
}
