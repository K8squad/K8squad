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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// toolhttpStubVerifier is a deterministic AuthoringTokenVerifier for the compat
// HTTP surface's token-path tests (ISI-7873): it returns the canned session for
// any bearer, or the canned error, mirroring what the real HS256 verifier does
// per request without needing a distributed signing key.
type toolhttpStubVerifier struct {
	sess AgentSession
	err  error
}

func (v *toolhttpStubVerifier) Verify(bearer string) (AgentSession, error) {
	if v.err != nil {
		return AgentSession{}, v.err
	}
	return v.sess, nil
}

// TestToolHTTP_TokenTenancyFromVerifiedClaims is the ISI-7873 acceptance on the
// read path: a call presenting a verified run capability token is scoped by the
// TOKEN's tenancy and identity — every forged client X-* header is discarded —
// so a warm-pool sandbox caller authenticates without any BFF header. Until
// this landed the compat surface was header-only and this call was the tenancy
// 401 that blinded the party runs.
func TestToolHTTP_TokenTenancyFromVerifiedClaims(t *testing.T) {
	fake := &fakeSearcher{}
	h := NewToolHTTP(NewReadService(fake, NewHashingEmbedder()), nil, nil).
		WithAuthoringTokenAuth(&toolhttpStubVerifier{sess: AgentSession{
			TeamID:    "team-token",
			Principal: "agent:voice",
			AgentID:   "agent-uuid",
			RunID:     "run-42",
			ViaToken:  true,
		}})
	mux := http.NewServeMux()
	h.Mount(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/memory_search", strings.NewReader(`{"query":"deploy"}`))
	req.Header.Set("Authorization", "Bearer run-token")
	// Forged identity headers must be discarded on the token path.
	req.Header.Set("X-Team-Id", "attacker-team")
	req.Header.Set("X-Principal-Id", "attacker")
	req.Header.Set("X-Agent-Id", "attacker-agent")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (verified bearer token authenticates the caller)", rec.Code)
	}
	if fake.got.SquadID != "team-token" {
		t.Fatalf("SquadID = %q, want team-token from the VERIFIED claims, never the forged header", fake.got.SquadID)
	}
}

// TestToolHTTP_TokenWriteScopeFromClaims is the ISI-7873 acceptance on the write
// path (diary_append): tenancy AND authorship come from the verified token
// claims; a forged header cannot author a diary row.
func TestToolHTTP_TokenWriteScopeFromClaims(t *testing.T) {
	fw := &fakeWriter{}
	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), NewWriteService(fw, NewHashingEmbedder()), nil).
		WithAuthoringTokenAuth(&toolhttpStubVerifier{sess: AgentSession{
			TeamID:    "team-token",
			Principal: "agent:voice",
			AgentID:   "agent-uuid",
			RunID:     "run-42",
			ViaToken:  true,
		}})
	mux := http.NewServeMux()
	h.Mount(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/diary_append", strings.NewReader(`{"entry":"rendered the round directive"}`))
	req.Header.Set("Authorization", "Bearer run-token")
	req.Header.Set("X-Team-Id", "attacker-team")
	req.Header.Set("X-Principal-Id", "attacker")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (verified bearer token authorizes the write)", rec.Code)
	}
	if fw.got.SquadID != "team-token" {
		t.Fatalf("SquadID = %q, want team-token from the VERIFIED claims", fw.got.SquadID)
	}
	if fw.got.PrincipalID != "agent:voice" {
		t.Fatalf("PrincipalID = %q, want agent:voice from the VERIFIED claims, never the forged header", fw.got.PrincipalID)
	}
}

// TestToolHTTP_TokenVerifyFailsClosed: a bearer token that does not verify is a
// 401 with NO fallback to header trust — pairing a junk token with forged X-*
// headers must not authenticate anything (the same posture as the MCP edge).
func TestToolHTTP_TokenVerifyFailsClosed(t *testing.T) {
	fake := &fakeSearcher{}
	h := NewToolHTTP(NewReadService(fake, NewHashingEmbedder()), nil, nil).
		WithAuthoringTokenAuth(&toolhttpStubVerifier{err: errors.New("signature mismatch")})
	mux := http.NewServeMux()
	h.Mount(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/memory_search", strings.NewReader(`{"query":"deploy"}`))
	req.Header.Set("Authorization", "Bearer junk-token")
	req.Header.Set("X-Team-Id", "team-1") // present but must NOT rescue a failed token
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (fail closed; no header fallback)", rec.Code)
	}
	if fake.got.SquadID != "" {
		t.Fatalf("SquadID = %q, want empty (no search served on a failed token)", fake.got.SquadID)
	}
}

// TestToolHTTP_BearerIgnoredWithoutVerifier: with no verifier wired (no
// KSQUAD_JWT_SIGNING_KEY) the Authorization header is ignored and the trusted
// BFF header path serves exactly as before — the mode stays inert until the
// signing key is distributed (D2).
func TestToolHTTP_BearerIgnoredWithoutVerifier(t *testing.T) {
	fake := &fakeSearcher{}
	h := NewToolHTTP(NewReadService(fake, NewHashingEmbedder()), nil, nil) // no WithAuthoringTokenAuth
	mux := http.NewServeMux()
	h.Mount(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/memory_search", strings.NewReader(`{"query":"deploy"}`))
	req.Header.Set("Authorization", "Bearer run-token")
	req.Header.Set("X-Team-Id", "team-1")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (header path serves; token path inert without a verifier)", rec.Code)
	}
	if fake.got.SquadID != "team-1" {
		t.Fatalf("SquadID = %q, want team-1 (header path)", fake.got.SquadID)
	}
}

// TestToolHTTP_TenancyRejectionSignal is ISI-7873 work item 3: a tenancy 401
// must tick the bounded ksquad_memory_tenancy_rejections_total counter with
// the closed reason set — the operator-visible signal whose absence let the
// party-run blinding go unnoticed.
func TestToolHTTP_TenancyRejectionSignal(t *testing.T) {
	child := tenancyRejections.WithLabelValues("http", "memory_search", tenancyReasonMissingTeam)
	before := testutil.ToFloat64(child)

	h := NewToolHTTP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/memory_search", strings.NewReader(`{"query":"deploy"}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without any authenticated tenant", rec.Code)
	}
	if got := testutil.ToFloat64(child) - before; got != 1 {
		t.Fatalf("tenancy rejection signal = %v, want exactly one tick (missing_team on memory_search)", got)
	}
}
