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
	"testing"
)

// RFC 4231 test-case-ish known answers keep the HMAC implementation honest:
// if ComputeHMACSHA256 drifts, every webhook verification path silently
// breaks (story 11.1 AC4).
func TestComputeHMACSHA256KnownAnswers(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		secret   string
		expected string
	}{
		{
			name:     "rfc4231 tc2",
			payload:  "what do ya want for nothing?",
			secret:   "Jefe",
			expected: "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
		},
		{
			name:     "empty payload",
			payload:  "",
			secret:   "s",
			expected: "64eca07cce67929c357d63d0a4aec207e774800403298914fc04e88ce02ac49f",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeHMACSHA256([]byte(tc.payload), tc.secret); got != tc.expected {
				t.Fatalf("HMAC(%q,%q) = %s, want %s", tc.secret, tc.payload, got, tc.expected)
			}
		})
	}
}

func TestParseSignatureHeader(t *testing.T) {
	digest := ComputeHMACSHA256([]byte("body"), "secret")
	got, err := ParseSignatureHeader("sha256=" + digest)
	if err != nil {
		t.Fatalf("valid header rejected: %v", err)
	}
	if got != digest {
		t.Fatalf("parsed digest %s, want %s", got, digest)
	}
	// Uppercase hex normalizes to lowercase.
	if _, err := ParseSignatureHeader("sha256=" + upper(digest)); err != nil {
		t.Fatalf("uppercase hex rejected: %v", err)
	}
	for _, bad := range []string{"", "sha1=abc", "sha256=", "sha256=xyz", "sha256=" + digest + "extra"} {
		if _, err := ParseSignatureHeader(bad); err == nil {
			t.Fatalf("malformed header %q accepted", bad)
		}
	}
}

func TestVerifyHMAC(t *testing.T) {
	payload := []byte(`{"zen":"keep it simple"}`)
	secret := "whsec"
	digest := ComputeHMACSHA256(payload, secret)

	if !VerifyHMAC(payload, secret, digest) {
		t.Fatal("good signature rejected")
	}
	// A single flipped bit, a wrong secret, an empty secret, an empty
	// signature and an empty payload must all fail (AC4: no fallback path).
	if VerifyHMAC(payload, "other", digest) {
		t.Fatal("wrong secret accepted")
	}
	if VerifyHMAC(append(payload, '!'), secret, digest) {
		t.Fatal("tampered payload accepted")
	}
	if VerifyHMAC(payload, "", digest) {
		t.Fatal("empty secret accepted")
	}
	if VerifyHMAC(payload, secret, "") {
		t.Fatal("empty signature accepted")
	}
	if VerifyHMAC(nil, secret, digest) {
		t.Fatal("empty payload accepted")
	}
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'f' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
