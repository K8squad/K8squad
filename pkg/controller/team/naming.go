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

package team

import (
	"fmt"
	"hash"
	"hash/fnv"
	"strings"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// namespacePrefix is the fixed prefix of every squad namespace. It keeps squad
// namespaces namespaced-prefixed and greppable, and guarantees a derived name
// never collides with a user-chosen plain name.
const namespacePrefix = "ksquad-team-"

// hashLen is the length of the hex-encoded UID hash suffix (fnv-32a). Eight
// hex chars give 32 bits of disambiguation — plenty for namespace-count
// fleets, while leaving most of the 63-char budget to the readable prefix.
const hashLen = 8

// maxNameLength is the DNS-1123 label limit every namespace must respect.
const maxNameLength = 63

// NamespaceNameFor derives the deterministic squad namespace name for a Team
// (story 4.1 AC1): `ksquad-team-<normalized-name-prefix>-<short-hash(uid)>`.
//
// The raw Team name is never used directly: it can exceed 63 chars, contain
// non-DNS-1123 characters, or collide with another Team after truncation. The
// name is normalized (lowercase, everything outside [a-z0-9-] collapsed to a
// single `-`, trimmed), truncated to the remaining budget, and disambiguated
// with a short hash of the Team UID — so two distinct Teams never resolve to
// the same namespace and the mapping is 1:1 and deterministic (AC1, NFR-SCALE1).
// The result is resolved once by the reconciler and recorded on
// status.namespace; later reconciles read it back instead of re-deriving, so a
// Team rename does not strand the original namespace.
func NamespaceNameFor(t *api.Team) string {
	normalized := NormalizeName(t.Name)
	// Fixed layout: prefix + normalized + "-" + hash. The normalized part
	// gets whatever budget remains after the fixed parts.
	budget := maxNameLength - len(namespacePrefix) - 1 - hashLen
	if len(normalized) > budget {
		normalized = normalized[:budget]
	}
	h := shortHash(string(t.UID))
	return fmt.Sprintf("%s%s-%s", namespacePrefix, normalized, h)
}

// NormalizeName maps an arbitrary object name onto a DNS-1123-safe label
// fragment: lowercase, [^a-z0-9-] collapsed to one `-`, leading/trailing `-`
// trimmed. An empty result (all-invalid input) falls back to "team" so the
// derived namespace is always non-empty and structurally valid.
func NormalizeName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "team"
	}
	return out
}

// shortHash renders the first hashLen hex chars of an fnv-32a over the input.
// The UID is the primary input (globally unique, immutable); an absent UID
// (only possible pre-persist, never on a reconciled object) falls back to the
// empty string so the function stays total and deterministic in unit tests.
func shortHash(s string) string {
	var h hash.Hash32 = fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())[:hashLen]
}

// reservedNamespaces are the namespaces a squad namespace must never resolve
// to or provision into (story 4.1 AC7): the ksquad control plane lives in
// ksquad-system, and the kube-* namespaces belong to the cluster itself.
// A Team whose namespace would land here fails closed — condition + error, no
// provisioning.
var reservedNamespaces = map[string]struct{}{
	"ksquad-system":   {},
	"kube-system":     {},
	"kube-public":     {},
	"kube-node-lease": {},
	"default":         {},
}

// IsReservedNamespace reports whether ns is a reserved (fail-closed) namespace.
func IsReservedNamespace(ns string) bool {
	_, reserved := reservedNamespaces[ns]
	return reserved
}
