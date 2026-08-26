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

// Package runtimes is the runtime-adapter seam of the KSquad shim (arch §7.5,
// stories 5.5 + 5.8). A Runtime declares one coding-agent flavor's honest
// capability set (fed onto the Agent Card, story 5.2), its credential shape
// (story 5.4), and how a Run is turned into a native CLI invocation — while
// the generic shim engine (pkg/shim) owns the A2A task lifecycle, SSE
// sequencing and conformance semantics. Adding a runtime is therefore a new
// file in this package plus a Register call: zero core change (FR-D3).
//
// The v1 shim set registered here is OpenClaw + Hermes (story 5.5) and
// opencode (story 5.8). Claude Code remains Phase 2 (epics §5, note on 5.5).
package runtimes

import (
	"fmt"
	"sort"
	"sync"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// CredentialShape is the runtime-native form a generic per-user Secret is
// mapped into (story 5.4, arch §7.3). The shim knows only the shape and the
// destination env var — never persisting or logging the raw secret.
type CredentialShape string

const (
	// ShapeAPIKey maps the credential into an API-key env var (the default
	// for OpenAI-compatible / BYO-endpoint runtimes, story 5.7).
	ShapeAPIKey CredentialShape = "api-key"
	// ShapeOAuthToken maps the credential into an OAuth-token env var (the
	// Claude-family subscription flow, story 7.2 — Phase 2 for the shim set).
	// #nosec G101 -- this is a credential-shape label, not a hardcoded secret.
	ShapeOAuthToken CredentialShape = "oauth-token"
)

// LaunchContext is the resolved, per-Run input a Runtime turns into an
// ExecSpec. Credential is the raw secret value the reconciler env-injected
// (arch §7.3); an adapter maps it into ExecSpec.Env and MUST NOT log it.
type LaunchContext struct {
	// Envelope is the transported context envelope (spec §8.5): system
	// context + concrete work instruction. Passed to the CLI via env, never
	// argv, so it cannot leak through the process table.
	Envelope a2a.Envelope
	// ModelRoute is the resolved model-provider seam (story 5.7). When
	// Endpoint is set the runtime rides the OpenAI-compatible wire to a BYO
	// endpoint; otherwise it uses Model against the runtime's fixed vendor.
	ModelRoute a2a.ModelRoute
	// Model is the resolved model id when no BYO endpoint is set
	// (Agent.spec.model). ModelRoute.Model takes precedence when present.
	Model string
	// Credential is the raw per-user secret value (arch §7.3). Empty when the
	// Agent Card advertised no credential requirement. NEVER logged.
	Credential string
	// WorkDir is the sandbox working directory the CLI runs in.
	WorkDir string
}

// ExecSpec is the native process a shim launches for one Run. Env carries the
// mapped credential and model route; the engine passes it to os/exec verbatim.
type ExecSpec struct {
	Path string
	Args []string
	Env  []string
	// WorkDir is the sandbox working directory the CLI runs in (from
	// LaunchContext.WorkDir); empty means the process's current directory.
	WorkDir string
}

// Runtime is the adapter contract every conformant coding-agent flavor
// implements (arch §7.5). It is declarative + pure: no I/O, no logging — the
// engine owns process execution, so an adapter is trivially unit-testable and
// its capability claims can be asserted by the conformance suite (story 5.6).
type Runtime interface {
	// Type is the conformant runtime flavor (api/v1alpha1 RuntimeType*).
	Type() string
	// CLIVersion is the pinned coding-agent CLI revision the adapter targets
	// (ADR-017 reproducibility). Advertised on the Agent Card.
	CLIVersion() string
	// Capabilities is the honest, first-class capability set (story 5.2, F15):
	// a gap is advertised, never discovered as a mid-Run failure.
	Capabilities() a2a.Capabilities
	// DefaultModel is the runtime's default model + its context-window budget
	// authority (spec §6.2); Agent.spec.model overrides the id, not the window.
	DefaultModel() a2a.ModelInfo
	// CredentialShape is the native form the generic Secret maps to (5.4).
	CredentialShape() CredentialShape
	// Command builds the native ExecSpec for a Run, mapping the credential and
	// model route into ExecSpec.Env. It MUST NOT log the credential (NFR-SEC3).
	Command(lc LaunchContext) (ExecSpec, error)
}

var (
	regMu    sync.RWMutex
	registry = map[string]Runtime{}
)

// Register adds rt to the process-wide registry keyed on rt.Type(). It panics
// on a duplicate or empty type — both are programmer errors caught at init.
func Register(rt Runtime) {
	regMu.Lock()
	defer regMu.Unlock()
	t := rt.Type()
	if t == "" {
		panic("runtimes: Register called with empty runtime type")
	}
	if _, dup := registry[t]; dup {
		panic(fmt.Sprintf("runtimes: duplicate runtime registration for %q", t))
	}
	registry[t] = rt
}

// Get returns the registered runtime for the given type, or an error naming
// the type if none is registered (fail-closed: the shim refuses to serve an
// unknown flavor rather than defaulting silently).
func Get(runtimeType string) (Runtime, error) {
	regMu.RLock()
	defer regMu.RUnlock()
	rt, ok := registry[runtimeType]
	if !ok {
		return nil, fmt.Errorf("runtimes: no shim registered for runtime type %q", runtimeType)
	}
	return rt, nil
}

// Registered returns the sorted set of registered runtime types. Used by the
// conformance suite and cmd/shim to enumerate the v1 shim set.
func Registered() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// resolveModel returns the model id a Run should drive, preferring a BYO
// endpoint's model (story 5.7) over the Agent's fixed model, over the
// runtime's default.
func resolveModel(rt Runtime, lc LaunchContext) string {
	switch {
	case lc.ModelRoute.Model != "":
		return lc.ModelRoute.Model
	case lc.Model != "":
		return lc.Model
	default:
		return rt.DefaultModel().ID
	}
}

// envelopeEnv returns the context-envelope env pair every v1 CLI reads its
// system context + work instruction from. Passing via env (not argv) keeps the
// prompt out of the process table.
func envelopeEnv(lc LaunchContext) []string {
	return []string{
		"KSQUAD_SYSTEM_CONTEXT=" + lc.Envelope.SystemContext,
		"KSQUAD_INPUT=" + lc.Envelope.Input,
	}
}

// modelRouteEnv maps a resolved BYO model endpoint (story 5.7) onto the
// OpenAI-compatible env the v1 runtimes honor. Empty when no endpoint is set
// (the runtime then uses its own vendor wire + native key). The Ollama lane
// (story 5.6) supplies an empty token, which becomes the conventional
// "ollama" placeholder key so the OpenAI client library still authenticates.
func modelRouteEnv(mr a2a.ModelRoute) []string {
	if mr.Endpoint == "" {
		return nil
	}
	key := mr.Token
	if key == "" {
		key = "ollama"
	}
	return []string{
		"OPENAI_BASE_URL=" + mr.Endpoint,
		"OPENAI_API_KEY=" + key,
	}
}
