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

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/instscoreingest"
	"github.com/K8squad/K8squad/pkg/telemetry/instscore"
)

// fakeInstScoreReader is the in-memory InstScoreReader for handler tests.
type fakeInstScoreReader struct {
	list    []instscoreingest.ServiceSummary
	detail  map[string]instscoreingest.ServiceDetail
	listErr error
	getErr  error
}

func (f *fakeInstScoreReader) ListServices(context.Context) ([]instscoreingest.ServiceSummary, error) {
	return f.list, f.listErr
}

func (f *fakeInstScoreReader) GetService(_ context.Context, name string) (instscoreingest.ServiceDetail, bool, error) {
	if f.getErr != nil {
		return instscoreingest.ServiceDetail{}, false, f.getErr
	}
	d, ok := f.detail[name]
	return d, ok, nil
}

// seededInstScoreReader builds a two-service fixture shaped like a real
// post-tap deployment: one scored service with pass/fail/not_evaluated
// verdicts (RES-005 pass, LOG-001 fail, SPA-003 not_evaluated — the engine's
// two disclosed exclusions are represented) and one mid-throttle service with
// evidence but no evaluation yet.
func seededInstScoreReader() *fakeInstScoreReader {
	now := time.Now().UTC()
	score := 63.86
	return &fakeInstScoreReader{
		list: []instscoreingest.ServiceSummary{
			{ServiceName: "checkout", Score: &score, Category: "Needs Improvement", Implemented: 24, Total: 26,
				EvaluatedAt: &now, PassCount: 17, FailCount: 5, SkipCount: 2,
				SpanCount: 120, LogCount: 40, MetricCount: 12, ResourceCount: 8,
				FirstBatchAt: &now, LastBatchAt: &now},
			{ServiceName: "just-arrived", Score: nil, PassCount: 0, FailCount: 0, SkipCount: 0,
				SpanCount: 3, LogCount: 0, MetricCount: 0, ResourceCount: 1,
				FirstBatchAt: &now, LastBatchAt: &now},
		},
		detail: map[string]instscoreingest.ServiceDetail{
			"checkout": {
				Summary: instscoreingest.ServiceSummary{ServiceName: "checkout", Score: &score,
					Category: "Needs Improvement", Implemented: 24, Total: 26, EvaluatedAt: &now,
					PassCount: 17, FailCount: 5, SkipCount: 2},
				Verdicts: []instscoreingest.VerdictDetail{
					{RuleID: "RES-005", Status: "pass", Evidence: "service.name present on all resources", Impact: "Critical", EvaluatedAt: now},
					{RuleID: "LOG-001", Status: "fail", Evidence: "debug-level log records observed over 15 days", Impact: "Important", EvaluatedAt: now},
					{RuleID: "SPA-003", Status: "not_evaluated", Evidence: "vendored criteria is an upstream TODO at pin ef080a8a", Impact: "Important", EvaluatedAt: now},
				},
				History: []instscoreingest.HistoryPoint{
					{BucketHour: now.Add(-2 * time.Hour).Truncate(time.Hour), Score: 71.43},
					{BucketHour: now.Add(-time.Hour).Truncate(time.Hour), Score: 63.86},
				},
			},
		},
	}
}

// testInstScoreServer assembles the real router with an admin session and the
// reader wired, mirroring testTeamsServer.
func testInstScoreServer(t *testing.T, reader InstScoreReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:admin", TeamID: uuid.New(), IsAdmin: true},
	}}
	srv := NewServer(Options{
		Authenticator:  NewCookieAuthenticator(resolver),
		Discussion:     discussion.NewHandler(nil),
		InstScoreReads: reader,
	})
	return srv.Handler()
}

// TestInstScoreServicesRoute — the list returns every service with the
// registry disclosure denominator, worst-score ordering preserved from the
// store.
func TestInstScoreServicesRoute(t *testing.T) {
	h := testInstScoreServer(t, seededInstScoreReader())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/v1/instscore/services", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET services: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp InstScoreServicesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RulesTotal != len(instscore.Rules) {
		t.Fatalf("rules_total = %d, want registry size %d", resp.RulesTotal, len(instscore.Rules))
	}
	if len(resp.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(resp.Services))
	}
	if resp.Services[0].Service != "checkout" || resp.Services[0].Score == nil || *resp.Services[0].Score != 63.86 {
		t.Fatalf("first row = %+v, want the scored checkout service", resp.Services[0])
	}
	if resp.Services[1].Service != "just-arrived" || resp.Services[1].Score != nil {
		t.Fatalf("pending row = %+v, want a NULL score", resp.Services[1])
	}
	var raw struct {
		Services []map[string]any `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw decode: %v", err)
	}
	if _, present := raw.Services[1]["score"]; present {
		t.Fatalf("pending score must serialize as an absent key, got %v", raw.Services[1]["score"])
	}
}

// TestInstScoreServiceDetailRoute — the detail merges stored verdicts with
// the registry: guidance on the failed rule, the not_evaluated exclusion,
// every other registry rule appended as not_evaluated, rule-ID order.
func TestInstScoreServiceDetailRoute(t *testing.T) {
	h := testInstScoreServer(t, seededInstScoreReader())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/v1/instscore/services/checkout", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var detail InstScoreServiceDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Service != "checkout" || detail.Category != "Needs Improvement" {
		t.Fatalf("detail summary drift: %+v", detail.InstScoreServiceSummary)
	}
	if len(detail.Verdicts) != len(instscore.Rules) {
		t.Fatalf("verdicts = %d, want the full registry (%d) — missing stored rules must be appended", len(detail.Verdicts), len(instscore.Rules))
	}
	byRule := make(map[string]InstScoreVerdict, len(detail.Verdicts))
	for _, v := range detail.Verdicts {
		byRule[v.Rule] = v
	}
	log001 := byRule["LOG-001"]
	if log001.Status != "fail" || log001.Guidance == "" || log001.Description == "" {
		t.Fatalf("failed rule must carry guidance + description: %+v", log001)
	}
	if log001.Impact != "Important" || len(log001.Target) != 1 || log001.Target[0] != "Log" {
		t.Fatalf("LOG-001 enrichment drift: %+v", log001)
	}
	res005 := byRule["RES-005"]
	if res005.Status != "pass" && res005.Guidance != "" {
		t.Fatalf("passed rule must not carry guidance: %+v", res005)
	}
	spa003 := byRule["SPA-003"]
	if spa003.Status != "not_evaluated" || spa003.Evidence == "" {
		t.Fatalf("SPA-003 exclusion must keep its evidence: %+v", spa003)
	}
	// A stored-verdict-less registry rule (e.g. MET-001) is present as
	// not_evaluated with the explicit evidence.
	met001 := byRule["MET-001"]
	if met001.Status != "not_evaluated" || met001.Evidence != "no stored verdict — the rule registry is newer than this service's last evaluation" {
		t.Fatalf("registry-only rule drift: %+v", met001)
	}
	for i := 1; i < len(detail.Verdicts); i++ {
		if detail.Verdicts[i-1].Rule >= detail.Verdicts[i].Rule {
			t.Fatalf("verdicts not sorted by rule ID: %s >= %s", detail.Verdicts[i-1].Rule, detail.Verdicts[i].Rule)
		}
	}
	if len(detail.History) != 2 || !detail.History[0].BucketHour.Before(detail.History[1].BucketHour) {
		t.Fatalf("history = %+v, want oldest-first", detail.History)
	}
}

// TestInstScoreDetailNotFound — an unobserved service is a plain 404.
func TestInstScoreDetailNotFound(t *testing.T) {
	h := testInstScoreServer(t, seededInstScoreReader())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/v1/instscore/services/never-seen", nil), devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown service: got %d, want 404", rec.Code)
	}
}

// TestInstScoreRoutesRequireAuth — no session ⇒ 401 at both routes (defence
// in depth over BFFAuthz).
func TestInstScoreRoutesRequireAuth(t *testing.T) {
	h := testInstScoreServer(t, seededInstScoreReader())
	for _, path := range []string{"/api/v1/instscore/services", "/api/v1/instscore/services/checkout"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without a session: got %d, want 401", path, rec.Code)
		}
	}
}

// TestInstScoreReaderErrors — store failures surface as 502, not as 500s or
// empty payloads.
func TestInstScoreReaderErrors(t *testing.T) {
	h := testInstScoreServer(t, &fakeInstScoreReader{listErr: errors.New("boom"), getErr: errors.New("boom")})
	for _, path := range []string{"/api/v1/instscore/services", "/api/v1/instscore/services/checkout"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, path, nil), devToken))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("GET %s on a broken reader: got %d, want 502", path, rec.Code)
		}
	}
}

// TestInstScoreNilReader501 — with no read model wired both routes keep the
// documented 501, like the sibling read models.
func TestInstScoreNilReader501(t *testing.T) {
	h := testInstScoreServer(t, nil)
	for _, path := range []string{"/api/v1/instscore/services", "/api/v1/instscore/services/checkout"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, path, nil), devToken))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("GET %s (nil reader): got %d, want 501", path, rec.Code)
		}
	}
}

// TestInstScoreDetailJSONRegistryImpactWins — a stored impact that disagrees
// with the pinned registry resolves to the registry value (the registry is
// authoritative; the store mirrors the engine that wrote it).
func TestInstScoreDetailJSONRegistryImpactWins(t *testing.T) {
	now := time.Now().UTC()
	d := instscoreingest.ServiceDetail{
		Summary: instscoreingest.ServiceSummary{ServiceName: "x"},
		Verdicts: []instscoreingest.VerdictDetail{
			{RuleID: "RES-005", Status: "fail", Evidence: "e", Impact: "Wrong", EvaluatedAt: now},
		},
	}
	out := instScoreDetailJSON(d)
	var v InstScoreVerdict
	for _, cand := range out.Verdicts {
		if cand.Rule == "RES-005" {
			v = cand
		}
	}
	if v.Impact != "Critical" {
		t.Fatalf("impact = %q, want the registry's Critical", v.Impact)
	}
}
