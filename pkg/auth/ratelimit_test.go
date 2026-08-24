package auth

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func newTestLimiter(t *testing.T, limit int, window time.Duration) *RateLimiter {
	t.Helper()
	return NewRateLimiter(limit, window)
}

func TestRateLimiter_AllowIsCheckOnly(t *testing.T) {
	r := newTestLimiter(t, 2, time.Minute)
	assert.True(t, r.Allow("1.2.3.4"))
	assert.True(t, r.Allow("1.2.3.4"), "Allow without Failure must never consume budget")
	assert.True(t, r.Allow("1.2.3.4"))
}

func TestRateLimiter_FailuresExhaustThenRecover(t *testing.T) {
	r := newTestLimiter(t, 2, time.Minute)
	r.Failure("1.2.3.4")
	r.Failure("1.2.3.4")
	assert.False(t, r.Allow("1.2.3.4"), "two failures exhaust the window")
	assert.True(t, r.Allow("5.6.7.8"), "other IPs are independent buckets")

	base := time.Now()
	r.now = func() time.Time { return base.Add(2 * time.Minute) }
	assert.True(t, r.Allow("1.2.3.4"), "window slides — old failures age out")
}

func TestRateLimiter_SuccessResetsWindow(t *testing.T) {
	r := newTestLimiter(t, 2, time.Minute)
	r.Failure("1.2.3.4")
	r.Failure("1.2.3.4")
	assert.False(t, r.Allow("1.2.3.4"))
	r.Success("1.2.3.4")
	assert.True(t, r.Allow("1.2.3.4"), "an authentic login clears the brute-force window (PR #90 finding 5)")
}

func TestRateLimiter_Disabled(t *testing.T) {
	r := newTestLimiter(t, 0, time.Minute)
	for i := 0; i < 100; i++ {
		r.Failure("1.2.3.4")
	}
	assert.True(t, r.Allow("1.2.3.4"))
}

func TestParseCIDRs(t *testing.T) {
	assert.Empty(t, ParseCIDRs(""))
	assert.Empty(t, ParseCIDRs("not-an-ip,,"))

	nets := ParseCIDRs("10.0.0.0/8,127.0.0.1,::1")
	assert.Len(t, nets, 3)
	assert.True(t, nets[0].Contains(net.ParseIP("10.1.2.3")))
	assert.True(t, nets[1].Contains(net.ParseIP("127.0.0.1")))
	assert.True(t, nets[2].Contains(net.ParseIP("::1")))
}

func TestClientIP_UntrustedPeerIgnoresXFF(t *testing.T) {
	// PR #90 review finding 1: a spoofed X-Forwarded-For must NOT rotate the
	// limiter bucket when the socket peer is not a trusted proxy.
	trust := ParseCIDRs("10.0.0.0/8")
	xff := "9.9.9.9, 8.8.8.8, 7.7.7.7" // attacker-supplied decoration

	got := ClientIP(nil, xff, "203.0.113.5:44321")
	assert.Equal(t, "203.0.113.5", got, "no trust set: socket address wins, XFF ignored")

	got = ClientIP(trust, xff, "198.51.100.7:9999")
	assert.Equal(t, "198.51.100.7", got, "peer outside the trust set: socket address wins")
}

func TestClientIP_TrustedProxyWalksRightToLeft(t *testing.T) {
	trust := ParseCIDRs("10.0.0.0/8")

	// peer=10.0.0.5 (trusted BFF), XFF chain: client, spoofed-mid, trusted-hop
	got := ClientIP(trust, "203.0.113.9, 8.8.8.8, 10.0.0.5", "10.0.0.5:1234")
	assert.Equal(t, "8.8.8.8", got, "first UNtrusted hop from the right is the client")

	got = ClientIP(trust, "203.0.113.9", "10.0.0.5:1234")
	assert.Equal(t, "203.0.113.9", got, "single-entry chain: the client itself")

	got = ClientIP(trust, "203.0.113.9, 10.0.0.6", "10.0.0.5:1234")
	assert.Equal(t, "203.0.113.9", got, "trusted hops are walked past, leftmost untrusted wins")

	got = ClientIP(trust, "10.0.0.6, 10.0.0.5", "10.0.0.5:1234")
	assert.Equal(t, "10.0.0.5", got, "all-trusted chain: fall back to the socket peer")
}

func TestClientIP_MalformedFallback(t *testing.T) {
	trust := ParseCIDRs("10.0.0.0/8")
	got := ClientIP(trust, "definitely-not-an-ip", "10.0.0.5:1234")
	assert.Equal(t, "10.0.0.5", got, "malformed XFF falls back to the honest socket address")

	got = ClientIP(nil, "", "203.0.113.5")
	assert.Equal(t, "203.0.113.5", got, "remoteAddr without a port passes through")
}
