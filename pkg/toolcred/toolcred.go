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

// Package toolcred implements the auxiliary-credential injection seam
// (ISI-3565, decision doc isi-3564-gh-token-seam-decision.md §4): a
// first-class, NON-MODEL VCS token materialised BY REFERENCE into a Run's
// agent container so a local gh/git CLI can authenticate.
//
// It is a deliberate SIBLING to pkg/credinject, not an extension of it.
// credinject binds the ONE model credentialSecretRef to the ONE
// runtime-native model env, keyed on (runtime, credential class) — that is
// the model credential's lifecycle axis. A VCS token is a SECOND,
// independent credential a different local tool consumes, so it gets its own
// slot and its own reviewed table rather than overloading credentialClass
// (which would break the "one credential → one runtime-native model env"
// invariant, decision §2 Option 2 literal, rejected).
//
// Two properties are load-bearing and enforced by construction, mirroring
// credinject:
//
//  1. By-reference / no-persist. Injection never reads the Secret's bytes:
//     it emits corev1.EnvVar values that are SecretKeySelectors, so the
//     kubelet materialises the token straight into the sandbox container and
//     the control plane never handles the plaintext.
//
//  2. Fail closed on an unmapped purpose. Users declare a PURPOSE
//     (github-token), never an arbitrary env var name — letting a Secret
//     pick its own env name would let it shadow PATH or a model-credential
//     key. The purpose→env-names mapping is a small, explicit, reviewed
//     table (data, not a code path); an unknown purpose refuses to guess.
package toolcred

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// Purpose is the caller-tool axis the aux-credential seam keys on: WHICH
// local tool consumes the token, and therefore WHICH env var name(s) it must
// land under. It is orthogonal to credinject's CredentialClass (which is the
// MODEL credential's human-seat vs service-account lifecycle axis).
type Purpose string

const (
	// PurposeGitHub is a GitHub VCS token for a local gh/git CLI. gh reads
	// GH_TOKEN natively (and drives git via `gh auth setup-git`); many
	// Actions-shaped tools read GITHUB_TOKEN — so the seam injects BOTH from
	// the one Secret (decision §4 step 1).
	PurposeGitHub Purpose = "github-token"
)

// defaultKey is the Secret data key read when a ToolCredential's SecretRef
// leaves Key empty. It matches the credinject/capability "token" convention
// so an aux-credential Secret has the same default shape as an MCP one.
const defaultKey = "token"

// table is the auxiliary-credential injection contract, keyed by purpose:
// the ordered list of env var names the token is injected under. It is
// intentionally data — adding a purpose (or an env alias) is a reviewed table
// edit, never a new code path — and it is the single source of truth both
// Inject and KnownPurposes read.
//
// The env-name lists are fixed by the seam, never by the user: a caller may
// only pick a purpose from this table, so a Secret can never be projected
// under an operator-chosen name that would shadow PATH or a model key.
var table = map[Purpose][]string{
	PurposeGitHub: {"GH_TOKEN", "GITHUB_TOKEN"},
}

// KnownPurposes lists every valid aux-credential purpose, sorted, for
// validation and error messages. It is the single source of truth the
// webhook enum check reads (symmetric to credinject.KnownClasses).
func KnownPurposes() []Purpose {
	names := make([]Purpose, 0, len(table))
	for p := range table {
		names = append(names, p)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// ValidatePurpose reports whether p is a known aux-credential purpose.
// Unlike credinject.ValidateClass, the empty purpose is INVALID: there is no
// safe default tool to inject a token for, so an unset purpose must be
// rejected rather than guessed (fail closed).
func ValidatePurpose(p Purpose) error {
	if _, ok := table[p]; ok {
		return nil
	}
	return fmt.Errorf("unknown tool-credential purpose %q; %s", p, supportedPurposesMsg())
}

// Injection is the runtime-native materialisation of one aux credential.
// Today the seam emits environment variables (by reference); Volumes/Mounts
// are carried so a future file-based aux credential (e.g. a ~/.netrc or a
// git-credentials file) slots into the same return type without changing
// callers — the same forward-compat shape credinject.Injection uses.
type Injection struct {
	// Env are the environment variables to add to the sandbox agent
	// container. Every entry's value is a SecretKeySelector — never a
	// literal — so the token is injected by the kubelet, never read by the
	// control plane.
	Env []corev1.EnvVar
	// Volumes are pod-level volumes backing any file-based aux mounts.
	Volumes []corev1.Volume
	// Mounts are the agent-container mounts for file-based aux credentials.
	Mounts []corev1.VolumeMount
}

// Inject maps one aux-credential (purpose + Secret ref) into its
// by-reference env materialisation. Every env var in the returned Injection
// reads the SAME Secret key (the ref's Key, or the "token" default) — for
// github-token that means GH_TOKEN and GITHUB_TOKEN both point at the one
// token in the one Secret.
//
// It fails CLOSED when:
//   - purpose is unknown to the contract (would strand a Run with a token
//     under an env name no tool reads), or
//   - the SecretRef names no Secret.
//
// Failing closed here means a mis-declared ToolCredential is rejected at
// admission (the webhook calls ValidatePurpose; assembly calls Inject)
// rather than dispatched to a sandbox whose gh/git authenticates as nobody.
func Inject(purpose Purpose, ref api.SecretRef) (Injection, error) {
	names, ok := table[purpose]
	if !ok {
		return Injection{}, fmt.Errorf("no tool-credential injection mapping for purpose %q; %s", purpose, supportedPurposesMsg())
	}
	if ref.Name == "" {
		return Injection{}, fmt.Errorf("tool-credential injection for purpose %q requires a Secret name; got empty SecretRef", purpose)
	}
	key := ref.Key
	if key == "" {
		key = defaultKey
	}
	env := make([]corev1.EnvVar, 0, len(names))
	for _, name := range names {
		env = append(env, corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
					Key:                  key,
				},
			},
		})
	}
	return Injection{Env: env}, nil
}

// supportedPurposesMsg lists the purposes the contract maps, for fail-closed
// error messages.
func supportedPurposesMsg() string {
	known := KnownPurposes()
	names := make([]string, 0, len(known))
	for _, p := range known {
		names = append(names, string(p))
	}
	return "supported purposes: [" + strings.Join(names, " ") + "]"
}
