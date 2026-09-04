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

// Package credinject implements the credential injection contract (story 5.4,
// arch §7.3, AD-9, FR-G1, NFR-SEC3): the seam that maps a generic per-user BYO
// credential Secret (arch §11) into the runtime-native form a shim expects —
// e.g. CLAUDE_CODE_OAUTH_TOKEN for a Claude-Code human seat vs ANTHROPIC_API_KEY
// for a service-account key.
//
// Two properties are load-bearing and enforced by construction here:
//
//  1. No-log / no-persist (NFR-SEC3). Injection is BY REFERENCE. The contract
//     never reads the Secret's bytes: it emits a corev1.EnvVar whose value is a
//     SecretKeySelector, so the kubelet materialises the credential straight
//     into the sandbox container and the control plane never handles the
//     plaintext. The operator cannot log or persist what it never reads — the
//     "never logs the credential" AC is a structural guarantee, not a
//     discipline the shim must remember.
//
//  2. Credential class (the gap ISI-2890 closes). A credential is either a
//     HUMAN-SEAT credential — an interactive OAuth token bound to a person's
//     subscription seat (Claude Code OAuth, §7.2 / zero-touch 7.7), whose
//     refresh/concurrency lifecycle is human-owned — or a SERVICE-ACCOUNT
//     credential — a long-lived API key / provider token, no interactive OAuth,
//     rotation = Secret update (§7.3, second-runtime story). The class selects
//     the runtime-native env var, and it is the vendor-neutral axis the core
//     reasons about instead of hardcoding one provider's shape.
//
// The mapping is a small, explicit, per-(runtime, class) table — deliberately
// data, so adding a runtime or a class is a reviewed table edit, never a code
// path. An unmapped (runtime, class) pair fails CLOSED: the contract refuses to
// guess an env var name, because guessing wrong would silently inject a
// credential under a name the runtime ignores (a Run that authenticates as
// nobody) or, worse, under a name a different tool reads.
package credinject

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// CredentialClass is the human-seat vs service-account axis (story 5.4). It is
// the vendor-neutral property the injection contract keys on, distinct from the
// runtime flavor (which decides the concrete env var NAME) and from the Secret
// key (which decides WHERE inside the Secret the material lives).
type CredentialClass string

const (
	// ClassHumanSeat is an interactive OAuth token bound to a human's
	// subscription seat (Claude Code OAuth subscription, §7.2; lifecycle is the
	// zero-touch controller model, 7.7). Refresh/concurrency is human-owned.
	ClassHumanSeat CredentialClass = "human-seat"

	// ClassServiceAccount is a long-lived API key / provider token with no
	// interactive OAuth (§7.3, second-runtime story). Rotation = Secret update.
	ClassServiceAccount CredentialClass = "service-account"
)

// DefaultClass is the class assumed when an Agent leaves spec.credentialClass
// unset. Service-account (a plain API key) is the safe default: it is the
// broadest, least-privileged credential shape, and an agent that actually holds
// a human OAuth seat must say so explicitly (the human-seat lifecycle in Epic 7
// is opt-in, not something to infer).
const DefaultClass = ClassServiceAccount

// KnownClasses lists every valid credential class, sorted, for validation and
// error messages. It is the single source of truth the webhook enum reads.
func KnownClasses() []CredentialClass {
	return []CredentialClass{ClassHumanSeat, ClassServiceAccount}
}

// ValidateClass reports whether c is a known credential class. An empty class
// is valid and resolves to DefaultClass (see Resolve); callers that want to
// reject an unset value should check for "" separately.
func ValidateClass(c CredentialClass) error {
	if c == "" || c == ClassHumanSeat || c == ClassServiceAccount {
		return nil
	}
	names := make([]string, 0, len(KnownClasses()))
	for _, k := range KnownClasses() {
		names = append(names, string(k))
	}
	return fmt.Errorf("unknown credential class %q; must be one of [%s]", c, strings.Join(names, " "))
}

// Resolve normalises an unset class to the default. It is the one place the
// "empty means default" rule lives, so admission, card generation and the
// dispatch path all agree.
func Resolve(c CredentialClass) CredentialClass {
	if c == "" {
		return DefaultClass
	}
	return c
}

// binding is one row of the injection table: for a (runtime, class) pair, the
// runtime-native env var name and the default Secret key to read when the
// Agent's SecretRef leaves Key empty.
type binding struct {
	// envVar is the runtime-native environment variable name the shim reads.
	envVar string
	// defaultKey is the Secret data key used when SecretRef.Key is empty.
	defaultKey string
}

// table is the credential injection contract, keyed by (runtime type, class).
// It is intentionally explicit and vendor-scoped: the core hardcodes no
// provider's 429 shape (§10.1) and, symmetrically, hardcodes no provider's
// credential env name outside this reviewed table. Adding a runtime or class =
// a row here, in review.
//
// The runtime type strings are the api.RuntimeType* constants (§5.3).
var table = map[string]map[CredentialClass]binding{
	api.RuntimeTypeClaudeCode: {
		// Claude Code with a subscription seat: the OAuth token env the CLI
		// reads (story 5.4 worked example).
		ClassHumanSeat: {envVar: "CLAUDE_CODE_OAUTH_TOKEN", defaultKey: "token"},
		// Claude Code with a raw Anthropic API key (no OAuth seat).
		ClassServiceAccount: {envVar: "ANTHROPIC_API_KEY", defaultKey: "apiKey"},
	},
	api.RuntimeTypeOpenClaw: {
		// OpenClaw is an API-key runtime (§7.3): no interactive OAuth path.
		ClassServiceAccount: {envVar: "ANTHROPIC_API_KEY", defaultKey: "apiKey"},
	},
	api.RuntimeTypeHermes: {
		ClassServiceAccount: {envVar: "ANTHROPIC_API_KEY", defaultKey: "apiKey"},
	},
	api.RuntimeTypeOpenCode: {
		// opencode speaks the OpenAI-compatible wire (ADR-026); the provider
		// key rides the OpenAI-standard env. A BYO Ollama endpoint needs no
		// credential — that path resolves via pkg/modelendpoint, not here.
		ClassServiceAccount: {envVar: "OPENAI_API_KEY", defaultKey: "apiKey"},
	},
	// Codex (ChatGPT/OpenAI Rust CLI) is a service-account-only runtime in v1
	// (ISI-3647 arch §3.4/§5 item 4, seam H, D1): the BYO OpenAI key rides the
	// OpenAI-standard OPENAI_API_KEY env, injected by reference so the control
	// plane never reads the bytes (NFR-SEC1). The human-seat auth.json branch
	// is the ToS-gated S9 fast-follow, deliberately absent here so a human-seat
	// class on codex fails CLOSED until that story lands. Keyed on the literal
	// "codex" — the value the RuntimeTypeCodex const (added by the S2 runtime
	// adapter story) will hold — so this credential row ships independently of
	// S2 with zero behavioural difference once the const lands.
	"codex": {
		ClassServiceAccount: {envVar: "OPENAI_API_KEY", defaultKey: "apiKey"},
	},
}

// Injection is the runtime-native materialisation of one credential. Today the
// contract emits environment variables (the story 5.4 shape); Volumes/Mounts
// are carried so a future file-based credential (e.g. a ~/.claude credentials
// file) slots into the same return type without changing callers.
type Injection struct {
	// Env are the environment variables to add to the sandbox agent container.
	// Every entry's value is a SecretKeySelector — never a literal — so the
	// credential material is injected by the kubelet, never read by the
	// control plane.
	Env []corev1.EnvVar
	// Volumes are pod-level volumes backing any file-based credential mounts.
	Volumes []corev1.Volume
	// Mounts are the agent-container mounts for file-based credentials.
	Mounts []corev1.VolumeMount
}

// Inject maps the Agent's generic BYO credential Secret into the runtime-native
// form for the given runtime type and credential class (story 5.4). It returns
// the env-by-reference injection — the control plane never touches the secret
// bytes.
//
// It fails CLOSED when:
//   - runtimeType is unknown to the contract, or
//   - the (runtime, class) pair has no mapping (e.g. a human-seat OAuth class
//     on an API-key-only runtime like OpenClaw), or
//   - the SecretRef names no Secret.
//
// Failing closed here means a mis-declared Agent is rejected at admission
// (the webhook calls Inject) rather than dispatched to a sandbox that would
// authenticate as nobody.
func Inject(runtimeType string, class CredentialClass, ref api.SecretRef) (Injection, error) {
	if ref.Name == "" {
		return Injection{}, fmt.Errorf("credential injection requires a Secret name; got empty SecretRef")
	}
	class = Resolve(class)
	byClass, ok := table[runtimeType]
	if !ok {
		return Injection{}, fmt.Errorf("no credential injection mapping for runtime %q; %s", runtimeType, supportedRuntimesMsg())
	}
	b, ok := byClass[class]
	if !ok {
		return Injection{}, fmt.Errorf("runtime %q does not support credential class %q; %s", runtimeType, class, supportedClassesMsg(runtimeType))
	}
	key := ref.Key
	if key == "" {
		key = b.defaultKey
	}
	env := corev1.EnvVar{
		Name: b.envVar,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
				Key:                  key,
			},
		},
	}
	return Injection{Env: []corev1.EnvVar{env}}, nil
}

// supportedRuntimesMsg lists the runtimes the contract maps, for fail-closed
// error messages.
func supportedRuntimesMsg() string {
	names := make([]string, 0, len(table))
	for rt := range table {
		names = append(names, rt)
	}
	sort.Strings(names)
	return "supported runtimes: [" + strings.Join(names, " ") + "]"
}

// supportedClassesMsg lists the classes a given runtime maps, for fail-closed
// error messages (e.g. "OpenClaw supports only service-account").
func supportedClassesMsg(runtimeType string) string {
	byClass := table[runtimeType]
	names := make([]string, 0, len(byClass))
	for c := range byClass {
		names = append(names, string(c))
	}
	sort.Strings(names)
	return fmt.Sprintf("runtime %q supports credential classes: [%s]", runtimeType, strings.Join(names, " "))
}
