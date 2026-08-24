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

package modelendpoint

import (
	"context"
	"testing"
	"time"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ref is a fixed observation instant so HTTP-date arithmetic is deterministic.
var ref = time.Date(2015, time.October, 21, 7, 0, 0, 0, time.UTC)

// TestParseRetryAfterDeltaSeconds (5.10 grammar form 1): a bare non-negative
// integer is delta-seconds.
func TestParseRetryAfterDeltaSeconds(t *testing.T) {
	d, ok := ParseRetryAfter("120", ref)
	require.True(t, ok)
	assert.Equal(t, 120*time.Second, d)
}

// TestParseRetryAfterZeroSeconds: "0" is valid delta-seconds ("retry now"),
// distinct from an absent header — ok must be true.
func TestParseRetryAfterZeroSeconds(t *testing.T) {
	d, ok := ParseRetryAfter("0", ref)
	require.True(t, ok)
	assert.Equal(t, time.Duration(0), d)
}

// TestParseRetryAfterHTTPDateFuture (5.10 grammar form 2): an HTTP-date is
// resolved to a wait relative to the observation instant.
func TestParseRetryAfterHTTPDateFuture(t *testing.T) {
	// ref + 28m00s.
	d, ok := ParseRetryAfter("Wed, 21 Oct 2015 07:28:00 GMT", ref)
	require.True(t, ok)
	assert.Equal(t, 28*time.Minute, d)
}

// TestParseRetryAfterHTTPDatePastFloorsToZero: a date already elapsed means
// "retry now" (0), not a negative wait — but still a recognized value (ok).
func TestParseRetryAfterHTTPDatePastFloorsToZero(t *testing.T) {
	d, ok := ParseRetryAfter("Wed, 21 Oct 2015 06:59:00 GMT", ref)
	require.True(t, ok)
	assert.Equal(t, time.Duration(0), d)
}

// TestParseRetryAfterAlternateDateFormats: RFC 7231 permits three date
// formats; http.ParseTime accepts all three and we must too.
func TestParseRetryAfterAlternateDateFormats(t *testing.T) {
	for _, v := range []string{
		"Wed, 21 Oct 2015 07:28:00 GMT",     // IMF-fixdate (RFC 1123)
		"Wednesday, 21-Oct-15 07:28:00 GMT", // RFC 850
		"Wed Oct 21 07:28:00 2015",          // ANSI C asctime
	} {
		d, ok := ParseRetryAfter(v, ref)
		require.Truef(t, ok, "value %q should parse", v)
		assert.Equalf(t, 28*time.Minute, d, "value %q", v)
	}
}

// TestParseRetryAfterWhitespaceTrimmed: leading/trailing whitespace around an
// otherwise valid value is tolerated.
func TestParseRetryAfterWhitespaceTrimmed(t *testing.T) {
	d, ok := ParseRetryAfter("  90  ", ref)
	require.True(t, ok)
	assert.Equal(t, 90*time.Second, d)
}

// TestParseRetryAfterInvalid: empty, negative, and unparseable values are all
// "no window" (ok=false, zero duration) — the caller's backoff path.
func TestParseRetryAfterInvalid(t *testing.T) {
	for _, v := range []string{
		"",              // absent
		"   ",           // whitespace only
		"-5",            // negative delta-seconds is malformed grammar
		"soon",          // not a number, not a date
		"12.5",          // delta-seconds is an integer, not a decimal
		"120s",          // no unit suffix in the grammar
		"not-a-date GMT",
	} {
		d, ok := ParseRetryAfter(v, ref)
		assert.Falsef(t, ok, "value %q should be rejected", v)
		assert.Equalf(t, time.Duration(0), d, "value %q", v)
	}
}

// TestNormalizeRateLimited429WithDeltaSeconds (5.10 core AC): a 429 with a
// delta-seconds Retry-After normalizes to a RateLimitedSignal carrying the
// parsed window, the from-model, and the observation instant.
func TestNormalizeRateLimited429WithDeltaSeconds(t *testing.T) {
	sig, ok := NormalizeRateLimited(RawRateLimit{
		StatusCode: StatusTooManyRequests,
		RetryAfter: "45",
		FromModel:  "qwen3:14b",
		ObservedAt: ref,
	})
	require.True(t, ok)
	assert.Equal(t, "qwen3:14b", sig.FromModel)
	assert.Equal(t, ref, sig.At)
	assert.Equal(t, 45*time.Second, sig.RetryAfter)
}

// TestNormalizeRateLimited429WithHTTPDate: the HTTP-date form flows through the
// normalizer relative to ObservedAt.
func TestNormalizeRateLimited429WithHTTPDate(t *testing.T) {
	sig, ok := NormalizeRateLimited(RawRateLimit{
		StatusCode: StatusTooManyRequests,
		RetryAfter: "Wed, 21 Oct 2015 07:28:00 GMT",
		ObservedAt: ref,
	})
	require.True(t, ok)
	assert.Equal(t, 28*time.Minute, sig.RetryAfter)
}

// TestNormalizeRateLimited429NoHeader: a 429 without a Retry-After is still a
// rate_limited signal (ok), but with RetryAfter=0 — the backoff path.
func TestNormalizeRateLimited429NoHeader(t *testing.T) {
	sig, ok := NormalizeRateLimited(RawRateLimit{
		StatusCode: StatusTooManyRequests,
		FromModel:  "gpt-4o",
		ObservedAt: ref,
	})
	require.True(t, ok)
	assert.Equal(t, time.Duration(0), sig.RetryAfter)
	assert.Equal(t, "gpt-4o", sig.FromModel)
}

// TestNormalizeRateLimitedNon429: any non-429 status is not a rate_limited
// signal — ok=false and the zero signal, so producers can gate every response.
func TestNormalizeRateLimitedNon429(t *testing.T) {
	for _, code := range []int{200, 400, 401, 500, 503} {
		sig, ok := NormalizeRateLimited(RawRateLimit{StatusCode: code, RetryAfter: "10"})
		assert.Falsef(t, ok, "status %d must not be rate_limited", code)
		assert.Equal(t, RateLimitedSignal{}, sig)
	}
}

// TestNormalizeRateLimitedZeroObservedAtDefaultsToNow: a producer that does not
// stamp ObservedAt still gets a sane (non-zero) At near now.
func TestNormalizeRateLimitedZeroObservedAtDefaultsToNow(t *testing.T) {
	before := time.Now()
	sig, ok := NormalizeRateLimited(RawRateLimit{StatusCode: StatusTooManyRequests, RetryAfter: "5"})
	require.True(t, ok)
	assert.False(t, sig.At.IsZero())
	assert.WithinDuration(t, before, sig.At, 5*time.Second)
	assert.Equal(t, 5*time.Second, sig.RetryAfter)
}

// TestNormalizeRateLimitedFeedsOnRateLimited (5.10 → 5.11 seam): the normalized
// signal is exactly what the existing 5.11 decision core consumes, proving the
// producer/consumer contract is closed end-to-end without any A2A transport.
func TestNormalizeRateLimitedFeedsOnRateLimited(t *testing.T) {
	r := newResolver(t) // no fallback secret configured
	s := &Switcher{Resolver: r}
	a := byoAgent(func(spec *api.AgentSpec) {
		spec.Model = "qwen3:14b"
	})

	sig, ok := NormalizeRateLimited(RawRateLimit{
		StatusCode: StatusTooManyRequests,
		RetryAfter: "60",
		FromModel:  "qwen3:14b",
		ObservedAt: ref,
	})
	require.True(t, ok)

	plan, err := s.OnRateLimited(context.Background(), a, sig)
	require.NoError(t, err)
	// No fallback configured → the 2.11 pause path, which honours the window
	// carried by the normalized signal.
	assert.Equal(t, ActionPauseRun, plan.Action)
	assert.Equal(t, "qwen3:14b", plan.From)
}
