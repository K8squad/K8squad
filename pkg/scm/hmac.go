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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// sigHeaderRe matches the GitHub X-Hub-Signature-256 shape: "sha256=<hex>".
var sigHeaderRe = regexp.MustCompile(`^sha256=([a-fA-F0-9]{64})$`)

// ComputeHMACSHA256 returns the lowercase hex HMAC-SHA256 of payload under
// secret. This is the one canonical implementation every webhook-verification
// path uses (story 11.1 AC4, D8/NFR-SEC8) — a second, divergent copy of this
// function is how a verify/parse skew bug is born.
func ComputeHMACSHA256(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// ParseSignatureHeader parses the "sha256=<hex>" webhook signature header,
// returning the bare hex digest. An absent or malformed header is an error —
// the caller must treat the delivery as unverifiable and drop it, never
// fall back to an unsigned parse (story 11.1 AC4).
func ParseSignatureHeader(header string) (string, error) {
	m := sigHeaderRe.FindStringSubmatch(strings.TrimSpace(header))
	if m == nil {
		return "", fmt.Errorf("invalid signature header format")
	}
	return strings.ToLower(m[1]), nil
}

// VerifyHMAC reports whether signature (bare hex digest) is the HMAC-SHA256
// of payload under secret. Comparison is constant time (hmac.Equal) — a
// webhook verifier is on the attacker-timing path.
func VerifyHMAC(payload []byte, secret, signature string) bool {
	if secret == "" || signature == "" || len(payload) == 0 {
		return false
	}
	expected := ComputeHMACSHA256(payload, secret)
	return hmac.Equal([]byte(expected), []byte(signature))
}
