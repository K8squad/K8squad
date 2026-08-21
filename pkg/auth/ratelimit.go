package auth

import (
	"net"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Per-IP failed-login rate limiting (15.1): default 5 FAILED attempts / 15 min,
// configurable via values/env. In-process sliding window — the counter is a
// coarse brute-force brake, not a durable store (ADR-001: no new dependency).
//
// Only FAILURES consume budget: a successful login resets the IP's window so a
// legitimate user (or a shared BFF egress IP, see ClientIP) is never locked out
// by correct logins (PR #90 review finding 5).
// ============================================================================

// RateLimiter is a sliding-window counter keyed by client IP.
type RateLimiter struct {
	mu        sync.Mutex
	hits      map[string][]time.Time
	limit     int
	window    time.Duration
	now       func() time.Time
	lastSweep time.Time
}

// NewRateLimiter builds a limiter allowing `limit` FAILED attempts per `window`.
// Non-positive limits disable limiting (limit <= 0 ⇒ Allow always true) — used by
// tests and by an explicit operator opt-out, never the default.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	if window <= 0 {
		window = 15 * time.Minute
	}
	return &RateLimiter{
		hits:   make(map[string][]time.Time),
		limit:  limit,
		window: window,
		now:    time.Now,
	}
}

// Allow reports whether an attempt from ip is currently permitted WITHOUT
// recording it — the caller records the outcome via Failure (bad credentials)
// or Success (authentic login, clears the window).
func (r *RateLimiter) Allow(ip string) bool {
	if r == nil || r.limit <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-r.window)
	alive := 0
	for _, h := range r.hits[ip] {
		if h.After(cutoff) {
			alive++
		}
	}
	r.sweep(r.now(), cutoff)
	return alive < r.limit
}

// Failure records one failed attempt for ip.
func (r *RateLimiter) Failure(ip string) {
	if r == nil || r.limit <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	cutoff := now.Add(-r.window)
	hits := r.hits[ip][:0]
	for _, h := range r.hits[ip] {
		if h.After(cutoff) {
			hits = append(hits, h)
		}
	}
	r.hits[ip] = append(hits, now)
}

// Success clears ip's failure window — authentic logins never consume the
// brute-force budget.
func (r *RateLimiter) Success(ip string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.hits, ip)
}

// sweep drops stale keys so an idle limiter retains nothing forever.
func (r *RateLimiter) sweep(now, cutoff time.Time) {
	if now.Sub(r.lastSweep) <= r.window {
		return
	}
	r.lastSweep = now
	for k, v := range r.hits {
		alive := v[:0]
		for _, h := range v {
			if h.After(cutoff) {
				alive = append(alive, h)
			}
		}
		if len(alive) == 0 {
			delete(r.hits, k)
		} else {
			r.hits[k] = alive
		}
	}
}

// ParseCIDRs parses a comma-separated list of IPs / CIDR prefixes into the
// trust set ClientIP consumes ("10.0.0.0/8,127.0.0.1"). Bare IPs become /32 (/128).
// An empty string yields an empty set — trust NOTHING by default.
func ParseCIDRs(list string) []*net.IPNet {
	var nets []*net.IPNet
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			if ip := net.ParseIP(part); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			}
			continue
		}
		if _, ipnet, err := net.ParseCIDR(part); err == nil {
			nets = append(nets, ipnet)
		}
	}
	return nets
}

// ClientIP extracts the client address for the login limiter (PR #90 review
// finding 1). X-Forwarded-For is honored ONLY when the socket peer (remoteAddr)
// is inside the trusted proxy set: the chain is then walked RIGHT→LEFT past
// trusted hops, and the first untrusted entry is the client. An untrusted (or
// absent) proxy means the XFF list is attacker-controlled decoration — the
// socket address is used instead. An empty trust set trusts no one.
func ClientIP(trusted []*net.IPNet, xff string, remoteAddr string) string {
	peer := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		peer = host
	}
	peerIP := net.ParseIP(peer)
	if len(trusted) > 0 && peerIP != nil && xff != "" && containsIP(trusted, peerIP) {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip == nil {
				return peer // malformed entry — fall back to the honest socket address
			}
			if !containsIP(trusted, ip) {
				return ip.String()
			}
		}
		return peer // every hop trusted (e.g. our own egress): use the socket peer
	}
	return peer
}

func containsIP(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
