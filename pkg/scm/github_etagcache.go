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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sync"
	"time"
)

// ISI-5497 (scope item 1 of ISI-5480) — the conditional-request cache that
// keeps GitHub's SECONDARY rate limit off the back of a SHARED PAT.
//
// GitHub answers a list request carrying a matching If-None-Match /
// If-Modified-Since validator with 304 Not Modified, and a 304 DOES NOT count
// against the primary rate limit (and sheds the secondary-limit pressure that a
// shared PAT polled every 300s across N projects keeps tripping — see
// [[github-rest-secondary-ratelimit]]). The biggest-win, lowest-risk pressure
// cut in the ISI-5480 plan is therefore simply: remember the validator GitHub
// stamped on the last 200 for each list URL, replay it on the next poll, and
// serve the stored body back when the server says 304.
//
// The reconciler (pkg/controller/reposync) builds a FRESH GitHubProvider on
// EVERY pass, so the cache cannot live on the provider instance — it would reset
// each reconcile and never see a second poll of the same URL. It therefore lives
// in a process-global registry keyed by a non-reversible PAT fingerprint, so the
// N projects that share one PAT share one cache and every poll after the first
// can go conditional.
//
// Scope boundary: this file is ETag/conditional-requests ONLY. The per-PAT
// token-bucket GOVERNOR (scope item 3) is ISI-5498 and lands separately; it
// composes as a second transport (or folds into this registry's patState)
// without touching the cache contract here. Durable persistence of the cache in
// the scm mirror store (so an operator restart does not re-burst a full fetch)
// is a documented follow-up: it needs either raw-body persistence or a
// not-modified-aware ApplySnapshot (a 304 that yielded zero records must never
// be read as "every record was deleted"), both gated on the parent plan-confirm.
//
// Nothing here touches a secret on the wire except the conditional-request
// validators, which are opaque GitHub tokens, never our PAT.

const (
	// defaultETagCacheTTL bounds how long a stored validator is trusted before a
	// full refetch, so a missed cache-invalidation can never wedge the mirror on
	// stale bytes forever. A poll is a fallback feed, not realtime, so an hour is
	// comfortably tighter than any staleness the mirror already tolerates.
	defaultETagCacheTTL = time.Hour
	// defaultETagCacheMaxEntries caps per-PAT cache memory (one entry per list
	// URL / pagination page). Oldest-first eviction keeps the hot working set;
	// 2000 covers a large multi-repo PAT's list surface with margin.
	defaultETagCacheMaxEntries = 2000

	// fromCacheHeader marks a response served from the ETag cache. go-github
	// reads this exact header name (the one gregjones/httpcache sets) to skip
	// updating its rate-limit bookkeeping from a cached response, so reusing it
	// keeps that behaviour correct for free.
	fromCacheHeader = "X-From-Cache"
)

// patFingerprint is a non-reversible fingerprint of a PAT, used ONLY as the
// in-memory map key for the shared cache. It never names the token in a log, a
// metric, or on the wire; 8 bytes of SHA-256 is collision-safe for the handful
// of distinct PATs a deployment runs.
func patFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// etagCacheRegistry maps PAT fingerprints to their shared per-PAT cache. One
// instance (sharedETagCacheRegistry) is process-global; a fresh one is cheap for
// tests so they never share state with each other or the live registry.
type etagCacheRegistry struct {
	mu     sync.Mutex
	caches map[string]*etagCache
	ttl    time.Duration
	maxEnt int
}

func newETagCacheRegistry(ttl time.Duration, maxEnt int) *etagCacheRegistry {
	return &etagCacheRegistry{
		caches: map[string]*etagCache{},
		ttl:    ttl,
		maxEnt: maxEnt,
	}
}

// sharedETagCacheRegistry is the process-global per-PAT conditional-request
// cache. Every GitHubProvider resolves its cache from here so projects on one
// PAT share it across the reconciler's per-pass provider churn.
var sharedETagCacheRegistry = newETagCacheRegistry(defaultETagCacheTTL, defaultETagCacheMaxEntries)

// cacheFor returns (creating on first use) the shared cache for one PAT. An
// empty token (anonymous provider) gets no cache — there is no stable per-caller
// key to share under, and an unauthenticated poll is not the shared-PAT pressure
// this cache exists to relieve.
func (r *etagCacheRegistry) cacheFor(token string) *etagCache {
	if token == "" {
		return nil
	}
	key := patFingerprint(token)
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.caches[key]; ok {
		return c
	}
	c := newEtagCache(r.ttl, r.maxEnt)
	r.caches[key] = c
	return c
}

// etagEntry is one cached conditional-request response: the validators to
// replay and the full 200 to serve when the server answers 304.
type etagEntry struct {
	etag         string
	lastModified string
	status       int
	header       http.Header
	body         []byte
	storedAt     time.Time
}

// toResponse rebuilds the stored 200 for a 304 hit. It stamps fromCacheHeader
// and hands back a fresh body reader so go-github parses it exactly as it would
// a live 200 — including the original pagination (Link) header, so a 304 on
// page 1 still walks to page 2.
func (e *etagEntry) toResponse(req *http.Request) *http.Response {
	h := e.header.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set(fromCacheHeader, "1")
	return &http.Response{
		StatusCode:    e.status,
		Status:        http.StatusText(e.status),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(e.body)),
		ContentLength: int64(len(e.body)),
		Request:       req,
	}
}

// etagCache is a per-PAT, TTL-bounded, oldest-first-evicting store of
// conditional-request responses keyed by "METHOD URL".
type etagCache struct {
	mu         sync.Mutex
	entries    map[string]*etagEntry
	ttl        time.Duration
	maxEntries int
}

func newEtagCache(ttl time.Duration, maxEntries int) *etagCache {
	return &etagCache{entries: map[string]*etagEntry{}, ttl: ttl, maxEntries: maxEntries}
}

// get returns a live entry, dropping it if past its TTL.
func (c *etagCache) get(key string) *etagEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil
	}
	if c.ttl > 0 && time.Since(e.storedAt) > c.ttl {
		delete(c.entries, key)
		return nil
	}
	return e
}

// put stores an entry, evicting the oldest first if a NEW key would exceed the
// cap (replacing an existing key never grows the map, so it skips eviction).
func (c *etagCache) put(key string, e *etagEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxEntries > 0 && len(c.entries) >= c.maxEntries {
		if _, exists := c.entries[key]; !exists {
			c.evictOldestLocked()
		}
	}
	c.entries[key] = e
}

func (c *etagCache) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, v := range c.entries {
		if first || v.storedAt.Before(oldest) {
			oldestKey, oldest, first = k, v.storedAt, false
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// etagTransport is an inner provider RoundTripper: it sits BELOW the oauth2 auth
// transport, so it adds conditional-request headers to the already-signed
// request and makes the real network call, then serves/refreshes the shared
// per-PAT ETag cache around it. It is otherwise a pure pass-through.
type etagTransport struct {
	base  http.RoundTripper
	cache *etagCache
}

func (t *etagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only idempotent reads are conditional-request cacheable. A write (the
	// outbound issue edit / comment / status) must always hit the server.
	cacheable := req.Method == http.MethodGet && t.cache != nil
	var key string
	var cached *etagEntry
	if cacheable {
		key = req.Method + " " + req.URL.String()
		if cached = t.cache.get(key); cached != nil {
			// Clone before mutating headers so a caller-shared request (oauth2
			// already cloned upstream, but be defensive) is never altered under it.
			req = req.Clone(req.Context())
			if cached.etag != "" {
				req.Header.Set("If-None-Match", cached.etag)
			}
			if cached.lastModified != "" {
				req.Header.Set("If-Modified-Since", cached.lastModified)
			}
		}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	// 304 → serve the stored 200. go-github's CheckResponse treats a raw 304 as
	// an error and never reads the body, so the conversion MUST happen here. A
	// 304 does not count against GitHub's primary rate limit — this is the win.
	if cacheable && cached != nil && resp.StatusCode == http.StatusNotModified {
		served := cached.toResponse(req)
		// The 304 itself carried FRESH X-RateLimit-* headers; carry them onto the
		// served response so the headroom gauge an outer transport feeds (GH-3)
		// reads the real remaining budget, not the value frozen in the cached 200.
		for _, h := range []string{"X-RateLimit-Remaining", "X-RateLimit-Limit", "X-RateLimit-Reset"} {
			if v := resp.Header.Get(h); v != "" {
				served.Header.Set(h, v)
			}
		}
		drainClose(resp.Body)
		return served, nil
	}

	// Fresh 200 carrying a validator → cache it (buffer the body and hand the
	// caller a fresh reader). Responses without a validator are passed through
	// untouched — no validator means no conditional request is possible.
	if cacheable && resp.StatusCode == http.StatusOK {
		etag := resp.Header.Get("ETag")
		lastMod := resp.Header.Get("Last-Modified")
		if etag != "" || lastMod != "" {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr != nil {
				return nil, rerr
			}
			t.cache.put(key, &etagEntry{
				etag:         etag,
				lastModified: lastMod,
				status:       resp.StatusCode,
				header:       resp.Header.Clone(),
				body:         body,
				storedAt:     time.Now(),
			})
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
	}

	return resp, nil
}

// drainClose discards and closes a 304's (empty) body so the connection can be
// reused, bounding the read so a misbehaving server cannot stream unboundedly.
func drainClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4<<10))
	_ = rc.Close()
}
