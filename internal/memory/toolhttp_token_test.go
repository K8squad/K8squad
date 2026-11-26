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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/K8squad/K8squad/pkg/mcpauthtoken"
)

// toolhttp_token_test.go — the ISI-7873 edge suite for the compat JSON/HTTP shim's
// run-capability-token (sandbox) path (WS-A of ISI-7868). It proves the warm-pool
// defect is closed: a discussion/party pod rides the per-run bearer token (no
// §13-BFF X-Team-Id header), and the header-only shim used to reject it with a
// silent 401. These tests assert the token path now resolves tenancy from the
// VERIFIED token, discards forged X-* headers, fails closed on a junk token, and
// that every rejection emits the operator-visible signal.

// mountTokenHTTP wires a ToolHTTP with the token-auth path enabled over a real
// minter, returning the server and the minter so a test mints tokens the edge
// verifies with the same key.
func mountTokenHTTP(t *testing.T, fake *fakeSearcher, fw *fakeWriter) (*httptest.Server, *mcpauthtoken.Minter) {
	t.Helper()
	minter, err := mcpauthtoken.NewMinter(tokTestKey, time.Hour)
	if err != nil {
		t.Fatalf("new minter: %v", err)
	}
	var write *WriteService
	if fw != nil {
		write = NewWriteService(fw, NewHashingEmbedder())
	}
	h := NewToolHTTP(NewReadService(fake, NewHashingEmbedder()), write, nil).
		WithAuthoringTokenAuth(NewMCPAuthTokenVerifier(minter))
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, minter
}

// TestToolHTTP_TokenPath_TenancyFromToken is the acceptance case: a warm-pool
// memory_search presenting ONLY a bearer run token (no X-Team-Id header) resolves
// its tenant from the VERIFIED token claims — the exact call that returned a silent
// 401 before ISI-7873.
func TestToolHTTP_TokenPath_TenancyFromToken(t *testing.T) {
	fake := &fakeSearcher{}
	srv, minter := mountTokenHTTP(t, fake, nil)

	tok, err := minter.Mint(mcpauthtoken.Claims{TeamID: "team-token", Principal: "agent:party", AgentID: "a1", RunID: "run-9"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/tools/memory_search", strings.NewReader(`{"query":"metallb"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (token tenancy must authenticate the warm-pool read)", res.StatusCode)
	}
	if fake.got.SquadID != "team-token" {
		t.Fatalf("SquadID = %q, want team-token (from the verified token, never a header)", fake.got.SquadID)
	}
}

// TestToolHTTP_TokenPath_ForgedHeadersDiscarded is the security edge: a VALID token
// for team-token paired with a forged X-Team-Id: attacker header must resolve to the
// TOKEN's tenant — the shim discards every inbound X-* identity header on the token
// path, so a sandbox cannot widen past its token by stamping its own headers.
func TestToolHTTP_TokenPath_ForgedHeadersDiscarded(t *testing.T) {
	fake := &fakeSearcher{}
	srv, minter := mountTokenHTTP(t, fake, nil)

	tok, _ := minter.Mint(mcpauthtoken.Claims{TeamID: "team-token", Principal: "agent:party", AgentID: "a1", RunID: "run-9"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/tools/memory_search", strings.NewReader(`{"query":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Team-Id", "attacker")
	req.Header.Set("X-Principal-Id", "attacker")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if fake.got.SquadID != "team-token" {
		t.Fatalf("SquadID = %q, want team-token (forged X-Team-Id must be discarded on the token path)", fake.got.SquadID)
	}
}

// TestToolHTTP_TokenPath_DiaryAppendAuthorFromToken proves the write surface
// (diary_append — one of the four ISI-7873 surfaces) stamps author + tenancy from
// the token, not headers.
func TestToolHTTP_TokenPath_DiaryAppendAuthorFromToken(t *testing.T) {
	fw := &fakeWriter{}
	srv, minter := mountTokenHTTP(t, &fakeSearcher{}, fw)

	tok, _ := minter.Mint(mcpauthtoken.Claims{TeamID: "team-token", Principal: "agent:party", AgentID: "a1", RunID: "run-9"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/tools/diary_append", strings.NewReader(`{"entry":"rolled helm rev 28"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if fw.got.SquadID != "team-token" || fw.got.PrincipalID != "agent:party" {
		t.Fatalf("author = team=%q principal=%q, want team-token/agent:party from the token", fw.got.SquadID, fw.got.PrincipalID)
	}
	if fw.got.AgentID == nil || *fw.got.AgentID != "a1" || fw.got.RunID == nil || *fw.got.RunID != "run-9" {
		t.Fatalf("agent/run linkage not stamped from token: agent=%v run=%v", fw.got.AgentID, fw.got.RunID)
	}
}

// TestToolHTTP_TokenPath_JunkTokenFailsClosed: a presented-but-unverifiable bearer
// token is a 401, NEVER a header-trust fallback — otherwise a sandbox could pair a
// junk token with forged X-* headers. The fake backend must never be reached.
func TestToolHTTP_TokenPath_JunkTokenFailsClosed(t *testing.T) {
	fake := &fakeSearcher{hits: nil}
	srv, _ := mountTokenHTTP(t, fake, nil)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/tools/memory_search", strings.NewReader(`{"query":"x"}`))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	req.Header.Set("X-Team-Id", "attacker") // a junk token must NOT fall back to this
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (junk token must fail closed, no header fallback)", res.StatusCode)
	}
	if fake.got.SquadID != "" {
		t.Fatalf("backend was reached with SquadID=%q; a junk token must never fall back to the forged header", fake.got.SquadID)
	}
}

// TestToolHTTP_HeaderPath_StillServes is the no-regression guard: with the token
// path wired, a request carrying NO bearer still authenticates on the trusted BFF
// header path exactly as before.
func TestToolHTTP_HeaderPath_StillServes(t *testing.T) {
	fake := &fakeSearcher{}
	srv, _ := mountTokenHTTP(t, fake, nil)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/tools/memory_search", strings.NewReader(`{"query":"x"}`))
	req.Header.Set("X-Team-Id", "team-bff")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (header/BFF path must be unchanged)", res.StatusCode)
	}
	if fake.got.SquadID != "team-bff" {
		t.Fatalf("SquadID = %q, want team-bff (header path)", fake.got.SquadID)
	}
}

// TestTenancyRejected_EmitsMetric is acceptance #2 of the parent: a tenancy
// rejection (here, no token + no header) ticks the bounded operator-visible counter
// — a recurrence of the silent-401 now alerts instead of blinding agents silently.
func TestTenancyRejected_EmitsMetric(t *testing.T) {
	// Register on a throwaway registry; RegisterTenancyMetrics is idempotent
	// process-wide (first-call-wins), so this test asserts the EMIT, using the
	// package counter whichever registry won the race.
	reg := prometheus.NewRegistry()
	RegisterTenancyMetrics(reg)

	before := counterValue(t, TenancyTransportHTTP, TenancyReasonMissingTeam)

	fake := &fakeSearcher{}
	// No token verifier wired here: a bare request with no X-Team-Id is the
	// classic missing-tenant 401.
	h := NewToolHTTP(NewReadService(fake, NewHashingEmbedder()), nil, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/tools/memory_search", strings.NewReader(`{"query":"x"}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	after := counterValue(t, TenancyTransportHTTP, TenancyReasonMissingTeam)
	if after != before+1 {
		t.Fatalf("tenancy counter {http,missing_team} = %v, want %v (one rejection recorded)", after, before+1)
	}
}

// counterValue reads the current value of the tenancy counter for one
// {transport,reason} pair off the package metric directly (robust to which
// registry won the idempotent Register race).
func counterValue(t *testing.T, transport, reason string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := tenancyRejectCtr.WithLabelValues(transport, reason).Write(m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
