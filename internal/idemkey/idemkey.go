// Package idemkey implements an in-process replay cache for the
// Idempotency-Key header (ISI-7907 WS-C fast-follow hardening).
//
// A client retrying a timed-out POST (double submit, network flap) resends
// the same Idempotency-Key; the server answers with the FIRST response
// instead of writing a second comment or discussion message — and firing a
// second mention dispatch. The cache is deliberately process-local: scopes
// are (principal, target, key), the window is minutes, and a restart
// forgets everything, which matches a retry that lands after the window.
package idemkey

import (
	"net/http"
	"sync"
	"time"
)

// Header is the wire header a client sends to opt in to replay protection.
const Header = "Idempotency-Key"

// ReplayHeader marks a response that was replayed from the cache rather
// than freshly written, so clients and tests can tell the two apart.
const ReplayHeader = "X-Idempotent-Replay"

// DefaultWindow is how long a stored response stays replayable.
const DefaultWindow = 10 * time.Minute

// MaxKeyLen bounds a client-supplied Idempotency-Key so a hostile or buggy
// client cannot pin unbounded memory in the scope map. A handler should reject
// a longer key with 400 before scoping/Put; the limit lives here as one source.
const MaxKeyLen = 255

// ReplayCache maps a scoped Idempotency-Key to the stored (status, body)
// of the first response. Safe for concurrent use; zero background work.
type ReplayCache struct {
	mu      sync.Mutex
	window  time.Duration
	entries map[string]replayEntry

	// lastSweep amortizes expiry: at most one sweep per window/2 Puts.
	lastSweep time.Time
}

type replayEntry struct {
	status    int
	body      []byte
	expiresAt time.Time
}

// NewReplayCache returns a cache with the given replay window; a
// non-positive window falls back to DefaultWindow.
func NewReplayCache(window time.Duration) *ReplayCache {
	if window <= 0 {
		window = DefaultWindow
	}
	return &ReplayCache{
		window:  window,
		entries: make(map[string]replayEntry),
	}
}

// KeyOf scopes a raw key to (principal, target) so the same key string on
// different endpoints, or from different principals, can never collide.
func KeyOf(principal, target, key string) string {
	return principal + "|" + target + "|" + key
}

// Get returns the stored response for scope when present and unexpired.
func (c *ReplayCache) Get(scope string) (status int, body []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, present := c.entries[scope]
	if !present || time.Now().After(e.expiresAt) {
		return 0, nil, false
	}
	return e.status, e.body, true
}

// Put stores the response for scope. First write wins: a later Put for the
// same unexpired scope is a no-op, so the replayed body is always the
// response the first caller actually got. Expired entries are replaced, and
// Put lazily sweeps expired entries at most once per window/2.
func (c *ReplayCache) Put(scope string, status int, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if e, present := c.entries[scope]; present && now.Before(e.expiresAt) {
		return
	}
	if now.Sub(c.lastSweep) >= c.window/2 {
		for k, e := range c.entries {
			if now.After(e.expiresAt) {
				delete(c.entries, k)
			}
		}
		c.lastSweep = now
	}
	c.entries[scope] = replayEntry{
		status:    status,
		body:      append([]byte(nil), body...),
		expiresAt: now.Add(c.window),
	}
}

// ServeJSON writes a stored or fresh JSON response. When replay is true it
// also sets X-Idempotent-Replay: true.
func ServeJSON(w http.ResponseWriter, status int, body []byte, replay bool) {
	if replay {
		w.Header().Set(ReplayHeader, "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
