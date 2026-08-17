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
	h := NewToolHTTP(NewReadService(fake, NewHashingEmbedder()))
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

// TestToolHTTP_RequiresTeamHeader asserts an unauthenticated call (no X-Team-Id) is rejected.
func TestToolHTTP_RequiresTeamHeader(t *testing.T) {
	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()))
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
