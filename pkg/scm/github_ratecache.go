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
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ISI-5480 — the durable answer to GitHub's SECONDARY rate limit on a SHARED
// PAT. The reconciler (pkg/controller/reposync) builds a FRESH GitHubProvider
// on every pass, so any state that must be shared across the projects on one
// PAT cannot live on the provider instance — it would reset every reconcile.
// Both knobs below therefore live in a process-global registry keyed by a
// non-reversible PAT fingerprint:
//
//   - a per-PAT token-bucket GOVERNOR paces every outbound call so N projects
//     on one PAT draw from ONE request budget — no single project can exhaust
//     the secondary limit for the others (scope item 3);
//   - a per-PAT ETag / Last-Modified CACHE lets each poll send conditional
//     requests (If-None-Match / If-Modified-Since); a 304 serves the stored
//     snapshot WITHOUT counting against the primary rate limit and without
//     re-transferring the body — the biggest-win, lowest-risk pressure cut
//     (scope item 1).
//
// Neither knob touches a secret on the wire except the conditional-request
// validators, which are opaque GitHub tokens, never our PAT.

const (
	// defaultPATRequestsPerSecond is the sustained outbound call rate per PAT.
	// 5/s = 300/min sits well under GitHub's ~900 points/min REST ceiling and,
	// crucially, SERIALIZES the fan-out so concurrent bursts (the usual
	// secondary-limit trigger) are smoothed across the projects on the PAT.
	defaultPATRequestsPerSecond = 5.0
	// defaultPATBurst absorbs a single snapshot's fan-out (issues + PRs +
	// check-runs + artifacts) without stalling the common case while still
	// capping the instantaneous burst a PAT can emit.
	defaultPATBurst = 10
	// defaultPATCacheTTL bounds how long a conditional-request validator is
	// trusted before a full refetch, so a missed cache-invalidation can never
	// wedge the mirror on stale bytes forever.
	defaultPATCacheTTL = time.Hour
	// defaultPATCacheMaxEntries caps per-PAT cache memory (one entry per list
	// URL / page). Oldest-first eviction keeps the hot working set.
	defaultPATCacheMaxEntries = 2000

	// envPATRate / envPATBurst let ops retune the governor without a rebuild.
	envPATRate  = "KSQUAD_SCM_PAT_RATE"
	envPATBurst = "KSQUAD_SCM_PAT_BURST"

	// fromCacheHeader marks a response served from the ETag cache. go-github
	// reads this exact header (set by gregjones/httpcache) to skip updating its
	// rate-limit bookkeeping from a cached response, so reusing the name keeps
	// that behaviour correct for free.
	fromCacheHeader = "X-From-Cache"
)

// patKey is a non-reversible fingerprint of a PAT, used ONLY as the in-memory
// map key for the shared governor + cache. It never names the token in a log,
// a metric, or on the wire; 8 bytes of SHA-256 is collision-safe for the
// handful of distinct PATs a deployment runs.
func patKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// patState is the shared-per-PAT governor + cache pair.
type patState struct {
	limiter *rate.Limiter
	cache   *etagCache
}

// patRegistry maps PAT fingerprints to their shared state. One instance
// (sharedPATRegistry) is process-global; a fresh one is cheap for tests.
type patRegistry struct {
	mu     sync.Mutex
	states map[string]*patState
	rps    rate.Limit
	burst  int
	ttl    time.Duration
	maxEnt int
}

// newPATRegistry builds a registry with the given governor + cache parameters.
func newPATRegistry(rps rate.Limit, burst int, ttl time.Duration, maxEnt int) *patRegistry {
	return &patRegistry{
		states: map[string]*patState{},
		rps:    rps,
		burst:  burst,
		ttl:    ttl,
		maxEnt: maxEnt,
	}
}

// newPATRegistryFromEnv builds the process-global registry, honouring the
// KSQUAD_SCM_PAT_RATE / KSQUAD_SCM_PAT_BURST ops overrides.
func newPATRegistryFromEnv() *patRegistry {
	rps := rate.Limit(defaultPATRequestsPerSecond)
	if v := os.Getenv(envPATRate); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			rps = rate.Limit(f)
		}
	}
	burst := defaultPATBurst
	if v := os.Getenv(envPATBurst); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			burst = n
		}
	}
	return newPATRegistry(rps, burst, defaultPATCacheTTL, defaultPATCacheMaxEntries)
}

// sharedPATRegistry is the process-global per-PAT governor + cache. Every
// GitHubProvider resolves its state from here so projects on one PAT share it.
var sharedPATRegistry = newPATRegistryFromEnv()

// stateFor returns (creating on first use) the shared state for one PAT.
func (r *patRegistry) stateFor(token string) *patState {
	key := patKey(token)
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.states[key]; ok {
		return st
	}
	st := &patState{
		limiter: rate.NewLimiter(r.rps, r.burst),
		cache:   newEtagCache(r.ttl, r.maxEnt),
	}
	r.states[key] = st
	return st
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
		Status:        strconv.Itoa(e.status) + " " + http.StatusText(e.status),
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

// put stores an entry, evicting the oldest first if a new key would exceed the
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

// govEtagTransport is the innermost provider RoundTripper (it sits below the
// oauth2 auth transport, so it adds conditional headers to the already-signed
// request and makes the real network call). It paces every call against the
// shared per-PAT governor and serves/refreshes the shared per-PAT ETag cache.
type govEtagTransport struct {
	base  http.RoundTripper
	state *patState
}

func (t *govEtagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Governor: pace this call against the PAT's shared token bucket so the
	// projects on one PAT never burst past GitHub's secondary limit together.
	// Wait honours the request context (the 30s client timeout), so a wedged
	// bucket cancels with the pass instead of stalling a worker forever.
	if t.state != nil && t.state.limiter != nil {
		if err := t.state.limiter.Wait(req.Context()); err != nil {
			return nil, err
		}
	}

	cacheable := req.Method == http.MethodGet && t.state != nil && t.state.cache != nil
	var key string
	var cached *etagEntry
	if cacheable {
		key = req.Method + " " + req.URL.String()
		if cached = t.state.cache.get(key); cached != nil {
			// Clone before mutating headers so a caller-shared request (oauth2
			// already cloned, but be defensive) is never altered under it.
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
		drainClose(resp.Body)
		return cached.toResponse(req), nil
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
			t.state.cache.put(key, &etagEntry{
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
