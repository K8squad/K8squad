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
	"net/http"
	"testing"
)

// GitHubWebhookExtractor must satisfy the provider-agnostic seam (Story 11.5).
var _ WebhookExtractor = GitHubWebhookExtractor{}

// TestGitHubExtractorSignatureHeaderOnly asserts the signature is read from
// X-Hub-Signature-256 and returned as the bare hex digest, and that an
// absent/malformed header is a drop — never an empty pass.
func TestGitHubExtractorSignatureHeaderOnly(t *testing.T) {
	ext := GitHubWebhookExtractor{}

	good := http.Header{}
	// 64 hex chars is the SHA-256 shape ParseSignatureHeader enforces.
	digest := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	good.Set("X-Hub-Signature-256", "sha256="+digest)
	got, err := ext.Signature(good)
	if err != nil {
		t.Fatalf("Signature() unexpected error: %v", err)
	}
	if got != digest {
		t.Fatalf("Signature() = %q, want %q", got, digest)
	}

	for name, h := range map[string]http.Header{
		"absent":     {},
		"unprefixed": {"X-Hub-Signature-256": {digest}},
		"wrong-alg":  {"X-Hub-Signature-256": {"sha1=" + digest}},
	} {
		if _, err := ext.Signature(h); err == nil {
			t.Errorf("%s: Signature() expected error, got nil", name)
		}
	}
}

// TestGitHubExtractorEventHeaderThenPayload asserts Event prefers the
// X-GitHub-Event header and falls back to a body probe only when it is absent.
func TestGitHubExtractorEventHeaderThenPayload(t *testing.T) {
	ext := GitHubWebhookExtractor{}

	withHeader := http.Header{}
	withHeader.Set("X-GitHub-Event", "pull_request")
	// Body is ignored when the header is present.
	if got := ext.Event(withHeader, []byte(`{"zen":"be nice"}`)); got != "pull_request" {
		t.Fatalf("Event() header path = %q, want %q", got, "pull_request")
	}

	cases := map[string]struct {
		body []byte
		want string
	}{
		"ping":         {[]byte(`{"zen":"keep it simple"}`), "ping"},
		"pr":           {[]byte(`{"action":"opened","pull_request":{"title":"x"}}`), "pull_request/opened"},
		"unstructured": {[]byte(`{"foo":"bar"}`), "unknown"},
		"garbage":      {[]byte(`not json`), "unknown"},
	}
	for name, tc := range cases {
		if got := ext.Event(http.Header{}, tc.body); got != tc.want {
			t.Errorf("%s: Event() payload path = %q, want %q", name, got, tc.want)
		}
	}
}

// TestRegistryResolvesGitHubExtractor asserts the v1 GitHub extractor is
// pre-registered and that an unknown provider is a surfaced error, not a
// silent fall-through to GitHub.
func TestRegistryResolvesGitHubExtractor(t *testing.T) {
	r := NewProviderRegistry()

	ext, err := r.ExtractorFor("github")
	if err != nil {
		t.Fatalf("ExtractorFor(github) unexpected error: %v", err)
	}
	if _, ok := ext.(GitHubWebhookExtractor); !ok {
		t.Fatalf("ExtractorFor(github) = %T, want GitHubWebhookExtractor", ext)
	}

	if _, err := r.ExtractorFor("gitlab"); err == nil {
		t.Fatal("ExtractorFor(gitlab) expected error for unregistered provider, got nil")
	}
}

// TestRegisterWebhookExtractorDropIn asserts a new provider's inbound path
// lands with a single Register call — the "new impl + config, no handler
// rewrite" discipline Story 11.5 locks.
func TestRegisterWebhookExtractorDropIn(t *testing.T) {
	r := NewProviderRegistry()
	r.RegisterWebhookExtractor("gitlab", stubExtractor{event: "Merge Request Hook"})

	ext, err := r.ExtractorFor("gitlab")
	if err != nil {
		t.Fatalf("ExtractorFor(gitlab) after register: %v", err)
	}
	if got := ext.Event(http.Header{}, nil); got != "Merge Request Hook" {
		t.Fatalf("drop-in extractor Event() = %q, want %q", got, "Merge Request Hook")
	}
}

type stubExtractor struct{ event string }

func (s stubExtractor) Signature(http.Header) (string, error) { return "", nil }
func (s stubExtractor) Event(http.Header, []byte) string      { return s.event }
