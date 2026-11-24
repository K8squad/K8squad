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

package instscore

import (
	"fmt"
	"regexp"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// The sensitive-data detector implements the "common formats" the four
// sensitive-data rules (LOG-003, MET-008, RES-010, SPA-006) enumerate:
// emails, Luhn-valid card numbers, SSN-style national IDs, bearer tokens and
// API-key shapes, password/secret/token key-value patterns, health-data key
// patterns, and known-sensitive attribute keys.
//
// The rules are explicit that their format list is illustrative, not
// exhaustive, and that detecting only the listed formats is not sufficient —
// no offline detector satisfies that fully. This one is deterministic and
// documented: it covers every format the rules name, and the evaluation
// evidence marks the verdict as detector-bound so reports state the bound.

// sensitiveKeyNames are attribute-key names (final path segment, folded to
// lowercase) under which any non-empty value violates the sensitive-data
// rules regardless of format, per the rules' key-name clause.
var sensitiveKeyNames = map[string]bool{
	"password":           true,
	"passwd":             true,
	"secret":             true,
	"token":              true,
	"api_key":            true,
	"apikey":             true,
	"ssn":                true,
	"truststorepassword": true,
	"keystorepassword":   true,
	"diagnosis":          true,
	"patient_id":         true,
	"insurance_id":       true,
	// "user identifiers that can be tied back to a person, such as
	// enduser.id, even when opaque or pseudonymous"
	"enduser.id": true,
}

// userIdentifierKeys are full attribute keys treated as user identifiers
// even when their final segment is not itself sensitive-looking.
var userIdentifierKeys = map[string]bool{
	"enduser.id":   true,
	"enduser.name": true,
	"user.id":      true,
	"user.email":   true,
}

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// digits possibly grouped by spaces/hyphens, 13-19 digits total
	cardRe = regexp.MustCompile(`(?:\d[ \-]?){13,19}`)
	ssnRe  = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	// bearer authorization headers and raw JWTs
	bearerRe = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9_\-\.=+/]{16,}`)
	jwtRe    = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]*`)
	// API-key shapes named by the rules and their vendored examples
	apiKeyRe = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_\-]{8,}|AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{16,}|gho_[A-Za-z0-9]{16,}|xoxb-[A-Za-z0-9\-]{10,}|xoxp-[A-Za-z0-9\-]{10,})`)
	// key-value credential patterns: password=..., secret: ..., token=...,
	// including JSON-style "password":"..." with the closing quote between
	// key and separator.
	credKVRe = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api_key|apikey|truststorepassword|keystorepassword|access_key)\b["']?\s*[:=]\s*\S`)
)

// finding is one sensitive-data match.
type finding struct {
	Where string // e.g. "attribute url.full", "body"
	Kind  string // e.g. "email", "credit card"
}

// describe returns a short human explanation for evidence strings.
func (f finding) describe() string {
	return fmt.Sprintf("%s: %s", f.Where, f.Kind)
}

// scanString checks one string value against the format detectors.
func scanString(s string) (kind string, ok bool) {
	if s == "" {
		return "", false
	}
	if emailRe.MatchString(s) {
		return "email address", true
	}
	if ssnRe.MatchString(s) {
		return "national ID (SSN-shaped)", true
	}
	if bearerRe.MatchString(s) {
		return "bearer token", true
	}
	if jwtRe.MatchString(s) {
		return "JWT", true
	}
	if apiKeyRe.MatchString(s) {
		return "API key", true
	}
	if credKVRe.MatchString(s) {
		return "credential key-value pattern", true
	}
	if card, ok := luhnMatch(s); ok {
		return "credit card number (" + card + "-digit, Luhn-valid)", true
	}
	return "", false
}

// luhnMatch reports whether s contains a 13-19 digit run (spaces/hyphens
// allowed between digits) that passes the Luhn checksum.
func luhnMatch(s string) (string, bool) {
	for _, m := range cardRe.FindAllString(s, -1) {
		digits := make([]byte, 0, len(m))
		for i := 0; i < len(m); i++ {
			if m[i] >= '0' && m[i] <= '9' {
				digits = append(digits, m[i]-'0')
			}
		}
		if len(digits) < 13 || len(digits) > 19 {
			continue
		}
		if luhnValid(digits) {
			return fmt.Sprintf("%d", len(digits)), true
		}
	}
	return "", false
}

func luhnValid(d []byte) bool {
	sum := 0
	double := false
	for i := len(d) - 1; i >= 0; i-- {
		v := int(d[i])
		if double {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		sum += v
		double = !double
	}
	return sum%10 == 0
}

// scanKeyValue checks one attribute key/value pair: the key against the
// sensitive-name and user-identifier lists, then the value's formats.
func scanKeyValue(key string, v *commonpb.AnyValue) (kind string, ok bool) {
	if keyIsSensitive(key) {
		return "sensitive key with non-empty value", true
	}
	return scanAnyValue(v)
}

// keyIsSensitive reports whether an attribute key itself names a sensitive
// field or a user identifier.
func keyIsSensitive(key string) bool {
	if userIdentifierKeys[key] {
		return true
	}
	seg := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		seg = key[i+1:]
	}
	return sensitiveKeyNames[strings.ToLower(seg)]
}

// scanAnyValue checks a value of any OTLP type, recursing into arrays,
// kvlists, and integer decimal forms as the rules require.
func scanAnyValue(v *commonpb.AnyValue) (kind string, ok bool) {
	if v == nil {
		return "", false
	}
	switch x := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return scanString(x.StringValue)
	case *commonpb.AnyValue_IntValue:
		// integers are matched in their decimal form
		return scanString(fmt.Sprintf("%d", x.IntValue))
	case *commonpb.AnyValue_ArrayValue:
		for _, el := range x.ArrayValue.GetValues() {
			if k, ok := scanAnyValue(el); ok {
				return k, true
			}
		}
	case *commonpb.AnyValue_KvlistValue:
		for _, kv := range x.KvlistValue.GetValues() {
			if k, ok := scanKeyValue(kv.GetKey(), kv.GetValue()); ok {
				return k, true
			}
		}
	}
	return "", false
}

// capFindings renders at most limit findings, appending an overflow count so
// evidence stays deterministic and bounded.
func capFindings(fs []finding, limit int) string {
	if len(fs) == 0 {
		return ""
	}
	parts := make([]string, 0, limit+1)
	for i, f := range fs {
		if i == limit {
			break
		}
		parts = append(parts, f.describe())
	}
	if len(fs) > limit {
		parts = append(parts, fmt.Sprintf("… and %d more", len(fs)-limit))
	}
	return strings.Join(parts, "; ")
}
