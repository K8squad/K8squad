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

package toolcred

// binding.go — the tool-credential BINDING REGISTRY (ISI-5811, WS-9 Credential
// Vault design §3.1). It is a sibling to the aux-credential INJECTION seam in
// this same package (toolcred.go, ISI-3565): that file answers "for this VCS
// purpose, which env names?" by reference; this registry answers the broader
// vault question "for this credential TYPE (github, dynatrace, terraform, …),
// which Secret data key must be materialized, how is it consumed (an MCP header
// vs a toolchain env var vs a mounted file), and under which var/header?".
//
// The same two load-bearing properties hold, by construction:
//
//  1. Data, not code. The binding set is a small, explicit, reviewed table —
//     adding a credential type is a table edit in review, never a new code path.
//     A well-formed table is a structural invariant: a malformed row (missing a
//     consumption carrier, a duplicate type) panics at package init, so the
//     binary never starts against a broken credential vocabulary.
//
//  2. Fail CLOSED on an unknown type. LookupBinding refuses to guess for a type
//     it does not map — exactly as credinject.DefaultSecretKey refuses an
//     unmapped (runtime, class), and as Inject refuses an unknown Purpose above.
//     Guessing a Secret name/key would materialize a credential a consumer never
//     reads (a Run that authenticates as nobody). An unmapped type therefore
//     cannot be saved (S2) nor surfaced as consumable (S1).
//
// Scope note (S1): this is data-only plus pure helpers. It touches no Secret
// value and performs no Kubernetes I/O — the materialization write path (S2) and
// the go-live gate check (S5) consume this registry but live elsewhere.

import (
	"fmt"
	"sort"
	"strings"
)

// AnnotationCredentialType is the Skill/MCPServer annotation a [sec]-gated
// authored CR carries to declare which credential TYPE it consumes (design
// §3.1 Option A — the recommended, CRD-schema-free declaration). S6 wires the
// annotation onto the WS-2/3/4/5 skills; this registry defines the type
// vocabulary that annotation's value must name. The envelope-trust rule
// (skill_types.go §Permissions / D8) still holds: the annotation is authorized
// by the registrar on the authored CR, never self-declared by a fetched body.
const AnnotationCredentialType = "ksquad.io/credential-type" // #nosec G101 -- this is an annotation KEY name, not a credential value

// Consumption is how a materialized credential reaches the consuming tool. It
// is a closed enum — a row whose Consumption is none of these is a malformed
// table entry (ValidateBindings panics at init).
type Consumption string

const (
	// ConsumptionMCPHeader injects the credential as an MCP request header
	// (HeaderName), e.g. Authorization or xi-api-key on an MCPServer call.
	ConsumptionMCPHeader Consumption = "mcp-header"

	// ConsumptionToolchainEnv injects the credential as an environment variable
	// (EnvVar) the toolchain CLI reads, e.g. GITHUB_TOKEN for gh.
	ConsumptionToolchainEnv Consumption = "toolchain-env"

	// ConsumptionFile materializes the credential as a mounted file. No seed
	// type uses it in v1; it is carried so a future file-based credential slots
	// into the same registry without a schema change (mirrors the Injection
	// Volumes/Mounts forward-carry above).
	ConsumptionFile Consumption = "file"
)

// Binding is one row of the registry: the static, per-TYPE contract for a tool
// credential. It carries NO Secret value and NO project-specific state — the
// per-project materialized Secret NAME is a pure function of (project, type),
// not a table field (see MaterializedSecretName), so the table stays a reusable
// vocabulary rather than a per-project map.
type Binding struct {
	// Type is the credential type key (the annotation value), e.g. "github".
	Type string
	// DisplayName is the human label the project-settings dropdown renders.
	DisplayName string
	// DataKey is the Secret data key the consuming skill/MCPServer reads (the
	// WRITE side stores the material under this key, exactly as
	// credinject.DefaultSecretKey pins the model-credential key). Required.
	DataKey string
	// Consumption selects the injection shape (header | env | file). Required.
	Consumption Consumption
	// EnvVar is the environment variable name, set iff Consumption is
	// toolchain-env (e.g. GITHUB_TOKEN). Empty otherwise.
	EnvVar string
	// HeaderName is the MCP request header name, set iff Consumption is
	// mcp-header (e.g. Authorization, xi-api-key). Empty otherwise.
	HeaderName string
	// ProbeKind names the credentialtest probe family used to validate the
	// credential, or ProbeNone when no probe is wired yet (S3 wires probes; v1
	// ships the vocabulary with "none" rather than fabricating a probe).
	ProbeKind string
	// RequiredByGate reports whether a skill declaring this type is withheld
	// from a Run until the credential is materialized (the go-live gate signal,
	// S5). All v1 seed types are hard requirements.
	RequiredByGate bool
}

// ProbeNone is the ProbeKind for a type with no credentialtest probe wired yet.
const ProbeNone = "none"

// bindingTable is the tool-credential binding registry, keyed by Type. It is the
// single source of truth for every tool-credential Secret name/key/consumption.
// Adding a type is a reviewed row here, never a code change in a consumer. The
// name is bindingTable (not table) because this package's aux-credential seam
// already owns `table` (purpose → env names, toolcred.go).
//
// Seed set (design §3.1): the credential types the WS-2/3/4/5 gated skills
// consume. The DataKey / EnvVar / HeaderName values match each tool's native
// credential shape so the materialized Secret authenticates the real consumer.
var bindingTable = map[string]Binding{
	// ── Platform / infra tools (WS-2, WS-3) ─────────────────────────────────
	"github": {
		Type: "github", DisplayName: "GitHub",
		DataKey: "token", Consumption: ConsumptionToolchainEnv,
		EnvVar: "GITHUB_TOKEN", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	"dynatrace": {
		Type: "dynatrace", DisplayName: "Dynatrace",
		DataKey: "api-token", Consumption: ConsumptionToolchainEnv,
		EnvVar: "DT_API_TOKEN", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	"terraform": {
		Type: "terraform", DisplayName: "Terraform Cloud",
		DataKey: "token", Consumption: ConsumptionToolchainEnv,
		EnvVar: "TF_TOKEN", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	"ansible": {
		Type: "ansible", DisplayName: "Ansible",
		DataKey: "token", Consumption: ConsumptionToolchainEnv,
		EnvVar: "ANSIBLE_TOKEN", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	"proxmox-api": {
		Type: "proxmox-api", DisplayName: "Proxmox API",
		DataKey: "token", Consumption: ConsumptionToolchainEnv,
		EnvVar: "PROXMOX_API_TOKEN", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	// ── Publishing / social (WS-4, WS-5) ────────────────────────────────────
	"linkedin-publish": {
		Type: "linkedin-publish", DisplayName: "LinkedIn (publish)",
		DataKey: "token", Consumption: ConsumptionMCPHeader,
		HeaderName: "Authorization", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	"x-publish": {
		Type: "x-publish", DisplayName: "X / Twitter (publish)",
		DataKey: "token", Consumption: ConsumptionMCPHeader,
		HeaderName: "Authorization", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	"blog-publish": {
		Type: "blog-publish", DisplayName: "Blog (publish)",
		DataKey: "token", Consumption: ConsumptionMCPHeader,
		HeaderName: "Authorization", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	// ── Generative providers (WS-3) ─────────────────────────────────────────
	"gemini": {
		Type: "gemini", DisplayName: "Google Gemini",
		DataKey: "api-key", Consumption: ConsumptionToolchainEnv,
		EnvVar: "GEMINI_API_KEY", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	"fal.ai": {
		Type: "fal.ai", DisplayName: "fal.ai",
		DataKey: "api-key", Consumption: ConsumptionToolchainEnv,
		EnvVar: "FAL_KEY", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	"heygen": {
		Type: "heygen", DisplayName: "HeyGen",
		DataKey: "api-key", Consumption: ConsumptionMCPHeader,
		HeaderName: "X-Api-Key", ProbeKind: ProbeNone, RequiredByGate: true,
	},
	"elevenlabs": {
		Type: "elevenlabs", DisplayName: "ElevenLabs",
		DataKey: "api-key", Consumption: ConsumptionMCPHeader,
		HeaderName: "xi-api-key", ProbeKind: ProbeNone, RequiredByGate: true,
	},
}

// LookupBinding returns the binding for a credential type and whether it is
// mapped. It fails CLOSED — ok=false — for a type the registry does not know, so
// no caller can materialize or surface a credential under an unreviewed
// name/key. (Named LookupBinding, not Lookup, to stay distinct from this
// package's purpose-axis helpers.)
func LookupBinding(credType string) (Binding, bool) {
	b, ok := bindingTable[credType]
	return b, ok
}

// IsKnownType reports whether credType is a mapped credential type (the
// fail-closed predicate a write path or endpoint gates on before trusting a
// type).
func IsKnownType(credType string) bool {
	_, ok := bindingTable[credType]
	return ok
}

// BindingTypes returns every mapped credential type, sorted, for validation,
// dropdown seeding, and fail-closed error messages. The returned slice is a
// fresh copy — callers may not mutate the registry through it.
func BindingTypes() []string {
	out := make([]string, 0, len(bindingTable))
	for t := range bindingTable {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Bindings returns every binding, sorted by Type — a fresh copy of the registry
// for a caller that needs the full metadata set (e.g. an admin audit view).
func Bindings() []Binding {
	out := make([]Binding, 0, len(bindingTable))
	for _, t := range BindingTypes() {
		out = append(out, bindingTable[t])
	}
	return out
}

// MaterializedSecretName is the deterministic per-project Secret name a
// credential of credType materializes into within the project's Team namespace
// (design §4: proj-<project>-cred-<type>). It lives WITH the registry so name
// derivation has one source of truth across the write path (S2), the read
// projection (S1), and the go-live gate (S5) — none of them invents the name.
//
// It returns ok=false for an unknown type (fail closed — no name for a type the
// registry does not map) and normalizes project/type into the DNS-1123 label
// shape a Secret name must satisfy (lowercase; non-alphanumerics collapsed to a
// single '-'; trimmed). The '.' in a type like "fal.ai" becomes '-' so the name
// stays a valid Secret name ("proj-p-cred-fal-ai").
func MaterializedSecretName(project, credType string) (string, bool) {
	if !IsKnownType(credType) {
		return "", false
	}
	name := fmt.Sprintf("proj-%s-cred-%s", dns1123(project), dns1123(credType))
	return name, true
}

// dns1123 lowercases s and collapses every run of non-[a-z0-9] into a single
// '-', trimming leading/trailing '-', so the result is a valid DNS-1123 label
// fragment. An empty/all-separator input yields "" (the caller's fmt stitches a
// stable surrounding shape, so the overall name never collapses to empty for a
// non-empty project+type).
func dns1123(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		isAlnum := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if isAlnum {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// ValidateBindings reports the first structural defect in the registry: a
// key/Type mismatch, a missing DataKey, an unknown Consumption, or a Consumption
// whose required carrier (EnvVar for toolchain-env, HeaderName for mcp-header) is
// missing or whose wrong-axis carrier is set. It is the single well-formedness
// contract a test asserts and init enforces.
func ValidateBindings() error {
	for key, b := range bindingTable {
		if b.Type != key {
			return fmt.Errorf("toolcred: binding key %q does not match Type %q", key, b.Type)
		}
		if b.DataKey == "" {
			return fmt.Errorf("toolcred: type %q has an empty DataKey", key)
		}
		switch b.Consumption {
		case ConsumptionToolchainEnv:
			if b.EnvVar == "" {
				return fmt.Errorf("toolcred: type %q is toolchain-env but has no EnvVar", key)
			}
			if b.HeaderName != "" {
				return fmt.Errorf("toolcred: type %q is toolchain-env but sets HeaderName %q", key, b.HeaderName)
			}
		case ConsumptionMCPHeader:
			if b.HeaderName == "" {
				return fmt.Errorf("toolcred: type %q is mcp-header but has no HeaderName", key)
			}
			if b.EnvVar != "" {
				return fmt.Errorf("toolcred: type %q is mcp-header but sets EnvVar %q", key, b.EnvVar)
			}
		case ConsumptionFile:
			if b.EnvVar != "" || b.HeaderName != "" {
				return fmt.Errorf("toolcred: type %q is file but sets EnvVar/HeaderName", key)
			}
		default:
			return fmt.Errorf("toolcred: type %q has unknown Consumption %q", key, b.Consumption)
		}
		if b.ProbeKind == "" {
			return fmt.Errorf("toolcred: type %q has an empty ProbeKind (use %q)", key, ProbeNone)
		}
	}
	return nil
}

// init enforces the well-formedness invariant at package load: a malformed
// registry is a programming error, so the binary fails to start rather than
// serve a broken credential vocabulary (fail closed, structurally).
func init() {
	if err := ValidateBindings(); err != nil {
		panic(err)
	}
}
