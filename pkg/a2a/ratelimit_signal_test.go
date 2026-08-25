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

package a2a

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRateLimitedIsPauseNotTerminal locks the story 5.10 contract: a provider
// 429 is a first-class pause (Run→Paused with a resume window), NOT a terminal
// failure. If someone later adds TaskRateLimited to IsTerminal's switch, the
// 2.11 resume timer would never re-drive the Run and a rate-limited task would
// dead-end — this is the check that fails loudly if that regresses.
func TestRateLimitedIsPauseNotTerminal(t *testing.T) {
	if TaskRateLimited.IsTerminal() {
		t.Fatal("TaskRateLimited must be non-terminal (a pause signal like auth-required), not terminal")
	}
	if TaskRateLimited != "rate-limited" {
		t.Fatalf("wire value drifted: got %q, want %q", TaskRateLimited, "rate-limited")
	}
	if EventRateLimited != "rate-limited" {
		t.Fatalf("event value drifted: got %q, want %q", EventRateLimited, "rate-limited")
	}
}

// TestRateLimitedPayloadCarriesRawRetryAfter asserts the wire preserves the
// Retry-After header byte-for-byte in BOTH RFC 7231 grammar forms and when
// absent. The core (modelendpoint.NormalizeRateLimited) is the single parser;
// if the wire pre-parsed or mangled the value, that single-interpretation
// invariant would break.
func TestRateLimitedPayloadCarriesRawRetryAfter(t *testing.T) {
	for _, raw := range []string{"120", "Wed, 21 Oct 2015 07:28:00 GMT", ""} {
		in := RateLimitedPayload{Provider: "openai", Model: "gpt-4o", RetryAfter: raw}
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal(%q): %v", raw, err)
		}
		var out RateLimitedPayload
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal(%q): %v", raw, err)
		}
		if out.RetryAfter != raw {
			t.Fatalf("Retry-After mangled on the wire: %q -> %q", raw, out.RetryAfter)
		}
		if out.Model != "gpt-4o" || out.Provider != "openai" {
			t.Fatalf("provenance lost: %+v", out)
		}
	}
	// The header field is named retryAfter (what the consumer wiring reads).
	b, _ := json.Marshal(RateLimitedPayload{RetryAfter: "1"})
	if !strings.Contains(string(b), `"retryAfter":"1"`) {
		t.Fatalf("unexpected JSON shape: %s", b)
	}
}

// TestRateLimitedCapabilityFlag guards the honest-default contract: a runtime
// that does not emit the standardized signal advertises RateLimited=false, so
// the core keeps treating an opaque failure as a failure.
func TestRateLimitedCapabilityFlag(t *testing.T) {
	if (Capabilities{}).RateLimited {
		t.Fatal("RateLimited must default to false (honest gap for shims that do not detect 429s)")
	}
	b, err := json.Marshal(Capabilities{RateLimited: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"rateLimited":true`) {
		t.Fatalf("capability not advertised on the Agent Card: %s", b)
	}
}
