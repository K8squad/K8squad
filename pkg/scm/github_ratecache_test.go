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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v57/github"
	"golang.org/x/time/rate"
)

// ghClientOverState points a go-github client at an httptest server THROUGH the
// ISI-5480 governor + ETag transport, so the conditional-request round trip is
// exercised against the real go-github response handling (which treats a raw
// 304 as an error — the transport MUST convert it to the cached 200).
func ghClientOverState(t *testing.T, srv *httptest.Server, state *patState) *GitHubProvider {
	t.Helper()
	client := github.NewClient(&http.Client{
		Transport: &govEtagTransport{base: http.DefaultTransport, state: state},
	})
	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client.BaseURL = u
	return &GitHubProvider{client: client}
}

// A second identical poll sends If-None-Match; the server answers 304; the
// provider still returns the cached snapshot. This is the core ISI-5480 win:
// the 304 does not re-transfer the body and does not burn the primary budget.
func TestEtagTransportServes304FromCache(t *testing.T) {
	var calls, conditional int32
	const body = `[{"number":1,"title":"bug","state":"open","user":{"login":"dev"},"html_url":"http://x"}]`

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			atomic.AddInt32(&conditional, 1)
			w.Header().Set("ETag", `"v1"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	state := &patState{limiter: rate.NewLimiter(rate.Inf, 1), cache: newEtagCache(time.Hour, 100)}
	p := ghClientOverState(t, srv, state)

	for i := 0; i < 2; i++ {
		recs, err := p.fetchIssues(context.Background(), "acme", "app", SnapshotOptions{})
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if len(recs) != 1 || recs[0].ExternalID != "1" {
			t.Fatalf("pass %d: got %+v, want one issue #1", i, recs)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server saw %d calls, want 2 (one full, one conditional)", got)
	}
	if got := atomic.LoadInt32(&conditional); got != 1 {
		t.Fatalf("server saw %d conditional (304) calls, want 1", got)
	}
}

// A changed resource re-validates to a fresh 200 and the cache updates — the
// conditional request is self-correcting, never serving stale bytes.
func TestEtagTransportRefreshesOnChange(t *testing.T) {
	var version atomic.Int32
	version.Store(1)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		v := version.Load()
		etag := fmt.Sprintf(`"v%d"`, v)
		if r.Header.Get("If-None-Match") == etag {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"number":%d,"title":"bug","state":"open","user":{"login":"dev"},"html_url":"http://x"}]`, v)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	state := &patState{limiter: rate.NewLimiter(rate.Inf, 1), cache: newEtagCache(time.Hour, 100)}
	p := ghClientOverState(t, srv, state)

	recs, err := p.fetchIssues(context.Background(), "acme", "app", SnapshotOptions{})
	if err != nil || len(recs) != 1 || recs[0].ExternalID != "1" {
		t.Fatalf("first pass: recs=%+v err=%v", recs, err)
	}
	version.Store(2) // upstream changed → new ETag → fresh 200
	recs, err = p.fetchIssues(context.Background(), "acme", "app", SnapshotOptions{})
	if err != nil || len(recs) != 1 || recs[0].ExternalID != "2" {
		t.Fatalf("after change: recs=%+v err=%v, want issue #2 (not stale #1)", recs, err)
	}
}

// The governor serializes calls against the per-PAT token bucket: N calls
// through a burst-1 limiter take at least (N-1)*interval of real time.
func TestGovernorPacesRequests(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	const interval = 40 * time.Millisecond
	const n = 4
	state := &patState{limiter: rate.NewLimiter(rate.Every(interval), 1), cache: newEtagCache(time.Hour, 10)}
	client := &http.Client{Transport: &govEtagTransport{base: http.DefaultTransport, state: state}}

	start := time.Now()
	for i := 0; i < n; i++ {
		resp, err := client.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		drainClose(resp.Body)
	}
	if elapsed, want := time.Since(start), time.Duration(n-1)*interval; elapsed < want {
		t.Fatalf("elapsed %v < %v — governor is not pacing the PAT", elapsed, want)
	}
}

// A canceled context aborts the governor wait instead of stalling the worker.
func TestGovernorHonoursContextCancel(t *testing.T) {
	// Exhausted burst + ~never-refill rate → Wait blocks until the ctx fires.
	state := &patState{limiter: rate.NewLimiter(rate.Every(time.Hour), 1), cache: newEtagCache(time.Hour, 10)}
	state.limiter.Allow() // drain the single burst token
	tr := &govEtagTransport{base: http.DefaultTransport, state: state}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid/", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("expected a context error from the governor wait, got nil")
	}
}

func TestEtagCacheTTLAndEviction(t *testing.T) {
	stale := newEtagCache(time.Hour, 10)
	stale.put("a", &etagEntry{storedAt: time.Now().Add(-2 * time.Hour)})
	if stale.get("a") != nil {
		t.Fatal("entry past its TTL must be dropped on get")
	}

	c := newEtagCache(time.Hour, 2)
	c.put("x", &etagEntry{etag: "x", storedAt: time.Now().Add(-3 * time.Minute)})
	c.put("y", &etagEntry{etag: "y", storedAt: time.Now().Add(-2 * time.Minute)})
	c.put("z", &etagEntry{etag: "z", storedAt: time.Now().Add(-1 * time.Minute)}) // over cap → evict oldest (x)
	if c.get("x") != nil {
		t.Fatal("oldest entry x should have been evicted at the cap")
	}
	if c.get("y") == nil || c.get("z") == nil {
		t.Fatal("y and z should survive eviction")
	}
}

func TestPATKeyDeterministicAndOpaque(t *testing.T) {
	a := patKey("ghp_secret_token")
	if a != patKey("ghp_secret_token") {
		t.Fatal("patKey must be deterministic")
	}
	if a == patKey("ghp_other_token") {
		t.Fatal("distinct tokens must not collide")
	}
	if strings.Contains(a, "ghp_secret_token") || strings.Contains(a, "secret") {
		t.Fatalf("fingerprint %q leaked the token", a)
	}
	if len(a) != 16 {
		t.Fatalf("fingerprint len = %d, want 16 hex chars", len(a))
	}
}

func TestRegistryStateSharedPerPAT(t *testing.T) {
	r := newPATRegistry(rate.Inf, 1, time.Hour, 10)
	s1 := r.stateFor("tokA")
	s2 := r.stateFor("tokA")
	s3 := r.stateFor("tokB")
	if s1 != s2 {
		t.Fatal("two providers on the same PAT must share one governor + cache")
	}
	if s1 == s3 {
		t.Fatal("distinct PATs must get distinct state")
	}
}

func TestGovernorEnvOverride(t *testing.T) {
	t.Setenv(envPATRate, "2.5")
	t.Setenv(envPATBurst, "7")
	r := newPATRegistryFromEnv()
	if r.rps != rate.Limit(2.5) {
		t.Fatalf("rps = %v, want 2.5 from %s", r.rps, envPATRate)
	}
	if r.burst != 7 {
		t.Fatalf("burst = %d, want 7 from %s", r.burst, envPATBurst)
	}

	t.Setenv(envPATRate, "garbage")
	t.Setenv(envPATBurst, "-3")
	rd := newPATRegistryFromEnv()
	if rd.rps != rate.Limit(defaultPATRequestsPerSecond) || rd.burst != defaultPATBurst {
		t.Fatalf("invalid env should fall back to defaults, got rps=%v burst=%d", rd.rps, rd.burst)
	}
}
