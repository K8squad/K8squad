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
)

// ghClientOverCache points a go-github client at an httptest server THROUGH the
// ISI-5497 ETag transport, so the conditional-request round trip is exercised
// against the real go-github response handling (which treats a raw 304 as an
// error — the transport MUST convert it to the cached 200).
func ghClientOverCache(t *testing.T, srv *httptest.Server, cache *etagCache) *GitHubProvider {
	t.Helper()
	client := github.NewClient(&http.Client{
		Transport: &etagTransport{base: http.DefaultTransport, cache: cache},
	})
	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client.BaseURL = u
	return &GitHubProvider{client: client}
}

// A second identical poll sends If-None-Match; the server answers 304; the
// provider still returns the cached snapshot. This is the core ISI-5497 win:
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

	p := ghClientOverCache(t, srv, newEtagCache(time.Hour, 100))

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

	p := ghClientOverCache(t, srv, newEtagCache(time.Hour, 100))

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

// Last-Modified-only resources (no ETag) still go conditional: the transport
// replays If-Modified-Since and serves the stored body on 304.
func TestEtagTransportUsesLastModified(t *testing.T) {
	const lastMod = "Wed, 21 Oct 2026 07:28:00 GMT"
	var conditional int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") == lastMod {
			atomic.AddInt32(&conditional, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", lastMod)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"number":9,"title":"x","state":"open","user":{"login":"d"},"html_url":"http://x"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := ghClientOverCache(t, srv, newEtagCache(time.Hour, 100))
	for i := 0; i < 2; i++ {
		if _, err := p.fetchIssues(context.Background(), "acme", "app", SnapshotOptions{}); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&conditional); got != 1 {
		t.Fatalf("If-Modified-Since conditional count = %d, want 1", got)
	}
}

// A 200 with NO validator is a pure pass-through: nothing is cached, so the next
// poll is unconditional (and the transport never fabricates a stale body).
func TestEtagTransportPassesThroughUnvalidatedResponse(t *testing.T) {
	var conditional int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			atomic.AddInt32(&conditional, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"number":1,"title":"x","state":"open","user":{"login":"d"},"html_url":"http://x"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := ghClientOverCache(t, srv, newEtagCache(time.Hour, 100))
	for i := 0; i < 2; i++ {
		if _, err := p.fetchIssues(context.Background(), "acme", "app", SnapshotOptions{}); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&conditional); got != 0 {
		t.Fatalf("sent %d conditional requests for an unvalidated resource, want 0", got)
	}
}

// The 304 path carries the FRESH X-RateLimit-* headers from the live 304 onto
// the served 200, so an outer transport's headroom gauge (GH-3) reads the real
// remaining budget, not the value frozen in the cached body.
func TestEtagTransport304CarriesFreshRateHeaders(t *testing.T) {
	cache := newEtagCache(time.Hour, 10)
	key := "GET http://example.invalid/x"
	cache.put(key, &etagEntry{
		etag:     `"v1"`,
		status:   http.StatusOK,
		header:   http.Header{"X-Ratelimit-Remaining": {"4000"}},
		body:     []byte("cached"),
		storedAt: time.Now(),
	})

	var remaining atomic.Int64
	remaining.Store(-1)
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("If-None-Match") != `"v1"` {
			t.Fatalf("expected conditional request, headers=%v", req.Header)
		}
		h := http.Header{}
		h.Set("X-RateLimit-Remaining", "4321") // fresh value on the 304
		return &http.Response{StatusCode: http.StatusNotModified, Header: h, Body: http.NoBody}, nil
	})
	// The rate tracker sits ABOVE the etag transport in the live chain, observing
	// whatever response the etag transport returns; wire it the same way here so
	// the carried-over header is what feeds the gauge.
	tr := &rateTrackingTransport{base: &etagTransport{base: base, cache: cache}, remaining: &remaining}

	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("served status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "4321" {
		t.Fatalf("served X-RateLimit-Remaining = %q, want 4321 (fresh from the 304)", got)
	}
	if got := remaining.Load(); got != 4321 {
		t.Fatalf("rate tracker observed %d, want 4321 from the carried-over header", got)
	}
}

// Writes (non-GET) are never cached or made conditional — they must always hit
// the server.
func TestEtagTransportDoesNotCacheWrites(t *testing.T) {
	var conditional int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("If-None-Match") != "" {
			atomic.AddInt32(&conditional, 1)
		}
		h := http.Header{"ETag": {`"w1"`}}
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: http.NoBody}, nil
	})
	tr := &etagTransport{base: base, cache: newEtagCache(time.Hour, 10)}
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/x", nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		drainClose(resp.Body)
	}
	if got := atomic.LoadInt32(&conditional); got != 0 {
		t.Fatalf("a write sent %d conditional requests, want 0", got)
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

func TestPATFingerprintDeterministicAndOpaque(t *testing.T) {
	a := patFingerprint("ghp_secret_token")
	if a != patFingerprint("ghp_secret_token") {
		t.Fatal("patFingerprint must be deterministic")
	}
	if a == patFingerprint("ghp_other_token") {
		t.Fatal("distinct tokens must not collide")
	}
	if strings.Contains(a, "ghp_secret_token") || strings.Contains(a, "secret") {
		t.Fatalf("fingerprint %q leaked the token", a)
	}
	if len(a) != 16 {
		t.Fatalf("fingerprint len = %d, want 16 hex chars", len(a))
	}
}

func TestETagCacheRegistrySharedPerPAT(t *testing.T) {
	r := newETagCacheRegistry(time.Hour, 10)
	c1 := r.cacheFor("tokA")
	c2 := r.cacheFor("tokA")
	c3 := r.cacheFor("tokB")
	if c1 != c2 {
		t.Fatal("two providers on the same PAT must share one cache")
	}
	if c1 == c3 {
		t.Fatal("distinct PATs must get distinct caches")
	}
	if r.cacheFor("") != nil {
		t.Fatal("an empty token (anonymous provider) must get no cache")
	}
}

// roundTripFunc adapts a function to http.RoundTripper for transport-level tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
