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

// ratelimit_parse.go — Story 5.10: the STANDARDIZED rate_limited signal
// PARSER/NORMALIZER (gap ISI-2894, source ISI-2876 alignment review).
//
// The consumer half of rate-limit handling is already real: the 5.11 decision
// core (Switcher.OnRateLimited, fallback.go) acts on a RateLimitedSignal, and
// the 2.11 durable resume timer (pkg/coord/resume.go) honours the signal's
// Retry-After window (resume_at = At + RetryAfter; no Retry-After → exponential
// backoff). What was MISSING for 5.10 was the PRODUCER-side normalization: the
// step that turns a raw provider/A2A "you are being rate limited" response into
// that one canonical RateLimitedSignal, with a single, correct Retry-After
// parse shared by every path.
//
// This file is that normalizer, and it is deliberately TRANSPORT-AGNOSTIC. It
// does not import the A2A wire types (the A2A client — stories 5.1–5.3, ISI-2889
// — is still in flight) and it does not dial anything. It inspects a small
// RawRateLimit view that any producer can populate from its own wire form:
//
//   - the A2A client (5.1–5.3): a task that fails with the provider's 429
//     surfaced through the shim → RawRateLimit{StatusCode: 429, RetryAfter: …};
//   - the model-endpoint HTTP path (BYO / Ollama-compatible, 5.7): a direct
//     429 from the upstream model endpoint.
//
// Both call NormalizeRateLimited and hand the result to OnRateLimited (5.11) /
// coord.Pause (2.11). Keeping the parse here means there is exactly ONE
// Retry-After interpretation in the codebase, unit-tested against the full
// RFC 7231 §7.1.3 grammar, instead of one ad-hoc parse per producer.
//
// # Retry-After grammar (RFC 7231 §7.1.3)
//
// Retry-After has TWO forms and a producer may send either:
//
//	Retry-After: 120                          ; delta-seconds (non-negative int)
//	Retry-After: Wed, 21 Oct 2015 07:28:00 GMT ; HTTP-date (absolute instant)
//
// ParseRetryAfter accepts both. The HTTP-date form is resolved against the
// observation instant (now), so a skewed producer clock never changes how long
// WE wait — we wait until the absolute instant the provider named, measured on
// our own clock, floored at zero (a date already in the past means "you may
// retry now", not "wait a negative amount").
package modelendpoint

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// StatusTooManyRequests is the canonical HTTP status that a provider (or the
// A2A shim relaying one) uses to signal rate limiting. Named locally so a
// producer populating RawRateLimit does not have to reach for net/http just to
// spell the constant.
const StatusTooManyRequests = http.StatusTooManyRequests // 429

// RawRateLimit is the transport-agnostic view of a provider/A2A response that
// the 5.10 normalizer inspects. Each producer fills it from its own wire form;
// the normalizer owns the interpretation so the interpretation lives in exactly
// one place.
type RawRateLimit struct {
	// StatusCode is the upstream HTTP status. 429 (StatusTooManyRequests) is
	// the canonical rate-limit trigger; any other status is not a
	// rate_limited signal and NormalizeRateLimited reports ok=false.
	StatusCode int

	// RetryAfter is the raw, unparsed Retry-After header value exactly as the
	// provider sent it ("120", "Wed, 21 Oct 2015 07:28:00 GMT", or "" when the
	// provider sent no header). NormalizeRateLimited parses it; producers must
	// NOT pre-parse.
	RetryAfter string

	// FromModel is the model the Run was serving from when the limit hit. May
	// be empty — OnRateLimited (5.11) falls back to the Agent's spec model.
	FromModel string

	// ObservedAt is when the limit was observed (the provenance segment
	// boundary and the reference instant for an HTTP-date Retry-After). Zero
	// means "now" — NormalizeRateLimited substitutes the current time so a
	// producer that does not stamp the observation still gets a correct
	// relative window.
	ObservedAt time.Time
}

// NormalizeRateLimited turns a raw provider/A2A response into the standardized
// RateLimitedSignal. ok is true iff raw is a rate_limited signal (StatusCode
// 429); on any other status it returns the zero signal and ok=false, so a
// producer can funnel every failing response through this one gate.
//
// When ok, the returned signal always carries a normalized RetryAfter:
//   - a valid Retry-After (delta-seconds or HTTP-date) → the parsed window;
//   - an absent or malformed Retry-After → 0, which the PauseRun path treats as
//     "no provider window" and hands to coord.Pause's exponential backoff.
//
// Pure and idempotent: the same raw always yields the same signal.
func NormalizeRateLimited(raw RawRateLimit) (RateLimitedSignal, bool) {
	if raw.StatusCode != StatusTooManyRequests {
		return RateLimitedSignal{}, false
	}
	at := raw.ObservedAt
	if at.IsZero() {
		at = time.Now()
	}
	// A missing/malformed header is not an error: it is a 429 with no window,
	// which is exactly the backoff path. Discard the ok flag here on purpose.
	retryAfter, _ := ParseRetryAfter(raw.RetryAfter, at)
	return RateLimitedSignal{
		FromModel:  raw.FromModel,
		At:         at,
		RetryAfter: retryAfter,
	}, true
}

// ParseRetryAfter parses an HTTP Retry-After header value (RFC 7231 §7.1.3)
// into a wait duration relative to now. It accepts both grammar forms:
//
//   - delta-seconds: a non-negative integer number of seconds ("0", "120").
//   - HTTP-date: an absolute instant (any of the three RFC 7231 date formats,
//     via http.ParseTime); the wait is date − now, floored at zero.
//
// ok is false for an empty, negative, or otherwise unparseable value; callers
// that must distinguish "no window" from "zero window" use ok, while producers
// feeding coord.Pause can treat both as the backoff path. A returned duration
// is never negative: a delta of "-5" is malformed (non-negative grammar) and an
// HTTP-date already in the past floors to 0 ("retry now").
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, false
	}

	// delta-seconds: a bare non-negative integer. strconv rejects signs-only,
	// decimals, and overflow; a parsed negative is malformed per the grammar.
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}

	// HTTP-date: absolute instant in any of the three RFC 7231 formats.
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0, true // the named instant has passed: retry now
		}
		return d, true
	}

	return 0, false
}
