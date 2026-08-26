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

package scm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v57/github"
)

// parseRepoURL must yield owner/repo from the last two path segments — the
// hostname is NOT the owner. This table pins the regression where
// https://github.com/acme/app parsed to owner="github.com", repo="acme",
// silently retargeting every provider API call at the wrong repository.
func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		in    string
		owner string
		repo  string
		err   bool
	}{
		{in: "https://github.com/acme/app", owner: "acme", repo: "app"},
		{in: "github.com/acme/app", owner: "acme", repo: "app"},
		{in: "https://github.com/acme/app.git", owner: "acme", repo: "app"},
		{in: "git@github.com:acme/app.git", owner: "acme", repo: "app"},
		{in: "https://github.com/acme/app/", owner: "acme", repo: "app"},
		{in: "https://ghe.corp/api/v3/acme/app", owner: "acme", repo: "app"},
		{in: "https://token@github.com/acme/app", owner: "acme", repo: "app"},
		{in: "https://github.com/acme", err: true},            // no repo segment
		{in: "acme/app", err: true},                           // bare owner/repo is ambiguous
		{in: "", err: true},                                   // nothing
		{in: "https://github.com/acme/app extra", err: true},  // trailing junk
		{in: "https://github.com/acme/app.tar.gz", err: true}, // not a repo name
	}
	for _, tc := range cases {
		owner, repo, err := parseRepoURL(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseRepoURL(%q) = %q/%q, want error", tc.in, owner, repo)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRepoURL(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if owner != tc.owner || repo != tc.repo {
			t.Errorf("parseRepoURL(%q) = %q/%q, want %q/%q", tc.in, owner, repo, tc.owner, tc.repo)
		}
	}
}

// VerifyWebhookDelivery is the exported AC4 gate: a genuine signature
// verifies, and nothing computable WITHOUT the secret does. This pins the
// regression where the digest was hex(payload) — a complete authentication
// bypass. The GitHub header name is provider knowledge exercised here, at
// the provider, not in the ingress (story 11.5).
func TestVerifyWebhookDeliveryRejectsForgedSignatures(t *testing.T) {
	p := &GitHubProvider{}
	ctx := context.Background()
	payload := []byte(`{"action":"opened"}`)
	secret := "whsec-per-project"

	headers := func(sig string) http.Header {
		h := http.Header{}
		if sig != "" {
			h.Set("X-Hub-Signature-256", sig)
		}
		return h
	}

	genuine := "sha256=" + ComputeHMACSHA256(payload, secret)
	if !p.VerifyWebhookDelivery(ctx, headers(genuine), payload, secret) {
		t.Fatal("genuine signature rejected")
	}

	// The old bypass: hex(payload) presented as a signature.
	forged := "sha256=" + fmt.Sprintf("%x", payload)
	if p.VerifyWebhookDelivery(ctx, headers(forged), payload, secret) {
		t.Fatal("BYPASS CONFIRMED: forged signature accepted without the secret")
	}

	// Right shape, wrong secret.
	wrongSecret := "sha256=" + ComputeHMACSHA256(payload, "wrong-secret")
	if p.VerifyWebhookDelivery(ctx, headers(wrongSecret), payload, secret) {
		t.Fatal("signature under a different secret accepted")
	}

	// Malformed / absent headers, empty secret, empty payload, nil headers.
	for _, sig := range []string{"", "sha256=deadbeef", "not-a-signature", "md5=abc"} {
		if p.VerifyWebhookDelivery(ctx, headers(sig), payload, secret) {
			t.Fatalf("malformed signature %q accepted", sig)
		}
	}
	if p.VerifyWebhookDelivery(ctx, headers(genuine), payload, "") {
		t.Fatal("empty secret must never verify")
	}
	if p.VerifyWebhookDelivery(ctx, headers(genuine), nil, secret) {
		t.Fatal("empty payload must never verify")
	}
	if p.VerifyWebhookDelivery(ctx, nil, payload, secret) {
		t.Fatal("nil headers must never verify")
	}
}

// ParseWebhookEvent normalizes the provider's event header, and — when the
// header is absent — probes the (already verified) payload for logging
// attribution only. Unparseable deliveries stay "unknown", never an error.
func TestParseWebhookEvent(t *testing.T) {
	p := &GitHubProvider{}
	ctx := context.Background()

	headers := func(k, v string) http.Header {
		h := http.Header{}
		if k != "" {
			h.Set(k, v)
		}
		return h
	}

	// Header present: used verbatim, delivery id carried along.
	h := headers("X-GitHub-Event", "issues")
	h.Set("X-GitHub-Delivery", "d-1234")
	ev, err := p.ParseWebhookEvent(ctx, h, []byte(`{}`))
	if err != nil || ev == nil {
		t.Fatalf("header event: err=%v ev=%v", err, ev)
	}
	if ev.Type != "issues" || ev.DeliveryID != "d-1234" {
		t.Fatalf("header event: got %+v", ev)
	}

	// Header absent: payload probe recognizes ping / pull_request / issue.
	cases := []struct {
		payload string
		want    string
	}{
		{`{"zen":"keep it simple"}`, "ping"},
		{`{"action":"opened","pull_request":{"title":"x"}}`, "pull_request/opened"},
		{`{"action":"closed","issue":{"title":"y"}}`, "issue/closed"},
		{`{"something":"else"}`, "unknown"},
		{`not json`, "unknown"},
	}
	for _, tc := range cases {
		ev, err := p.ParseWebhookEvent(ctx, headers("", ""), []byte(tc.payload))
		if err != nil || ev == nil {
			t.Fatalf("payload %q: err=%v ev=%v", tc.payload, err, ev)
		}
		if ev.Type != tc.want {
			t.Fatalf("payload %q: got type %q, want %q", tc.payload, ev.Type, tc.want)
		}
	}
}

// newTestGitHubProvider points a provider at an httptest server so the
// fetchers' wire behaviour (pagination, filtering, call counts) is exercised
// against the real go-github client.
func newTestGitHubProvider(t *testing.T, mux *http.ServeMux) (*GitHubProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := github.NewClient(srv.Client())
	baseURL, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	client.BaseURL = baseURL
	return &GitHubProvider{client: client}, srv
}

func issueJSON(number int, state, title, actor string, isPR bool) string {
	b, _ := json.Marshal(map[string]interface{}{
		"number":   number,
		"title":    title,
		"state":    state,
		"user":     map[string]string{"login": actor},
		"html_url": fmt.Sprintf("https://github.com/acme/app/issues/%d", number),
	})
	if isPR {
		return string(b[:len(b)-1]) + `,"pull_request":{"url":"x"}}`
	}
	return string(b)
}

// fetchIssues paginates at PerPage=100 via Link headers, stops when there
// is no next page, and skips pull requests (they are mirrored once, as PRs,
// by fetchPullRequests — GitHub returns PRs on the issues endpoint).
func TestFetchIssuesPaginationAndPRFilter(t *testing.T) {
	var issueCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&issueCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("issues per_page = %q, want 100", r.URL.Query().Get("per_page"))
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/app/issues?page=2>; rel="next"`, "http://"+r.Host))
			fmt.Fprintf(w, `[%s,%s]`,
				issueJSON(1, "open", "bug", "dev", false),
				issueJSON(2, "open", "pr-as-issue", "dev", true))
		case "2":
			fmt.Fprintf(w, `[%s]`, issueJSON(3, "closed", "old", "dev", false))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	})

	p, _ := newTestGitHubProvider(t, mux)
	records, err := p.fetchIssues(context.Background(), "acme", "app", SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&issueCalls); got != 2 {
		t.Fatalf("expected exactly 2 paginated calls, got %d", got)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 issue records (PR-as-issue skipped), got %d: %+v", len(records), records)
	}
	for _, rec := range records {
		if rec.Kind != RecordTypeIssue {
			t.Errorf("record %s has kind %q", rec.ExternalID, rec.Kind)
		}
		if rec.ExternalID == "2" {
			t.Error("pull request leaked into the issue mirror (double-mirror)")
		}
	}
}

// fetchArtifacts makes exactly ONE paginated pass over the repo-wide
// ListArtifacts endpoint — the old code repeated the identical call once
// per workflow run.
func TestFetchArtifactsSinglePass(t *testing.T) {
	var artifactCalls int32
	var runCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/actions/artifacts", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&artifactCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"total_count":1,"artifacts":[{"id":11,"name":"logs","size_in_bytes":42,"archive_download_url":"http://x"}]}`)
	})
	mux.HandleFunc("/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&runCalls, 1)
		fmt.Fprintf(w, `{"total_count":5,"workflow_runs":[{},{},{},{},{}]}`)
	})

	p, _ := newTestGitHubProvider(t, mux)
	records, err := p.fetchArtifacts(context.Background(), "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&artifactCalls); got != 1 {
		t.Fatalf("ListArtifacts must be called exactly once (repo-wide), got %d", got)
	}
	if got := atomic.LoadInt32(&runCalls); got != 0 {
		t.Fatalf("ListWorkflowRuns must not be called at all, got %d", got)
	}
	if len(records) != 1 || records[0].ExternalID != "11" {
		t.Fatalf("artifact record wrong: %+v", records)
	}
}

// fetchCheckRuns scopes to the default branch + open PR heads instead of
// fanning out per commit, and dedupes check runs visible on several refs.
func TestFetchCheckRunsScopedToLiveRefs(t *testing.T) {
	var commitListCalls int32
	var checkRunCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"default_branch":"main"}`)
	})
	mux.HandleFunc("/repos/acme/app/commits", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&commitListCalls, 1)
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "open" {
			t.Errorf("PR head discovery must list open PRs, got state=%q", r.URL.Query().Get("state"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"number":7,"head":{"sha":"prheadsha"},"state":"open"}]`)
	})
	mux.HandleFunc("/repos/acme/app/commits/main/check-runs", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&checkRunCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total_count":1,"check_runs":[{"id":101,"name":"ci","status":"completed","conclusion":"success"}]}`)
	})
	mux.HandleFunc("/repos/acme/app/commits/prheadsha/check-runs", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&checkRunCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		// Same check-run id as on main: must be deduped, not mirrored twice.
		fmt.Fprint(w, `{"total_count":1,"check_runs":[{"id":101,"name":"ci","status":"completed","conclusion":"success"}]}`)
	})

	p, _ := newTestGitHubProvider(t, mux)
	records, err := p.fetchCheckRuns(context.Background(), "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&commitListCalls); got != 0 {
		t.Fatalf("commit-history walk must be gone, got %d ListCommits calls", got)
	}
	if len(records) != 1 || records[0].ExternalID != "101" {
		t.Fatalf("check-run records wrong (dedup?): %+v", records)
	}
}

// Snapshot maps a parsed URL onto the right owner/repo paths end to end.
func TestSnapshotTargetsParsedOwnerRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"default_branch":"main"}`)
	})
	for _, path := range []string{
		"/repos/acme/app/issues",
		"/repos/acme/app/pulls",
		"/repos/acme/app/actions/artifacts",
		"/repos/acme/app/commits/main/check-runs",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch path {
			case "/repos/acme/app/issues", "/repos/acme/app/pulls":
				fmt.Fprint(w, `[]`)
			case "/repos/acme/app/actions/artifacts":
				fmt.Fprint(w, `{"total_count":0,"artifacts":[]}`)
			default:
				fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
			}
		})
	}
	mux.HandleFunc("/repos/github.com/acme", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("provider hit hostname-as-owner path %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})

	p, _ := newTestGitHubProvider(t, mux)
	if _, err := p.Snapshot(context.Background(), "https://github.com/acme/app", SnapshotOptions{}); err != nil {
		t.Fatalf("snapshot against parsed owner/repo: %v", err)
	}
}
