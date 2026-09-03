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

// Package credential is the Epic 7 per-user credential PLUMBING (stories
// 7.1/7.2/7.3/7.5): the one seam the Run dispatch path calls to turn an
// `Agent` CR into the runtime-native credential injection its sandbox pod
// needs. It composes two lower contracts rather than re-deriving them:
//
//   - pkg/credinject (story 5.4) maps the Agent's per-user BYO credential
//     Secret (7.1) into the runtime-native provider env var — the Claude Code
//     OAuth token (7.2, human-seat) or the second-runtime API key (7.3,
//     service-account). credinject deliberately owns NO endpoint concern.
//
//   - the BYO model-endpoint shape (7.5): when an Agent points at a self-hosted
//     Ollama / OpenAI-compatible endpoint via `spec.modelEndpointRef` (a
//     per-user Secret holding the endpoint URL), this package injects that URL
//     into the runtime's base-URL env — by reference, exactly like the
//     credential — so the shim drives the runtime at that endpoint with no
//     paid provider token and no new AgentRuntime.type / image (ADR-026).
//
// Every value this package emits is env-BY-REFERENCE (a SecretKeySelector,
// never a literal). The control plane never reads the Secret bytes, so the
// "never logs / never persists the credential" discipline (NFR-SEC3) is a
// structural property of the injection, not a rule the caller must remember.
//
// The 7.1 "per-namespace, never cross-squad" AC is likewise structural: a
// SecretKeySelector carries only a LocalObjectReference (a bare name), so the
// kubelet can only resolve it in the sandbox pod's OWN namespace — the squad
// namespace (story 4.1). There is no field on which a cross-namespace Secret
// could even be named, so a credential can never leak across squads.
package credential

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/credinject"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// baseURLEnv is the runtime-native environment variable a shim reads to point
// its provider client at a BYO endpoint (story 7.5). Like the credinject
// table, it is deliberately DATA — an explicit per-runtime row, so adding a
// runtime is a reviewed edit, never a code path, and an unmapped runtime fails
// CLOSED rather than guessing an env name the runtime ignores (which would
// silently send the Run to the provider default, not the operator's endpoint).
//
// The Anthropic-family runtimes read ANTHROPIC_BASE_URL; the OpenAI-compatible
// runtimes — opencode (the natural Ollama host, story 5.8) and codex (OpenAI's
// official Rust CLI, ISI-3647/S6) — read OPENAI_BASE_URL. codex additionally
// materializes a config.toml [model_providers.ksquad-byo] block from the same
// endpoint (capability.RenderCodexConfig); the URL injected here is the
// load-bearing half, the block is a safe superset (arch ISI-3646 D6).
var baseURLEnv = map[string]string{
	api.RuntimeTypeClaudeCode: "ANTHROPIC_BASE_URL",
	api.RuntimeTypeOpenClaw:   "ANTHROPIC_BASE_URL",
	api.RuntimeTypeHermes:     "ANTHROPIC_BASE_URL",
	api.RuntimeTypeOpenCode:   "OPENAI_BASE_URL",
	api.RuntimeTypeCodex:      "OPENAI_BASE_URL",
}

// Resolve builds the complete credential + endpoint injection for one Agent
// running under the given resolved runtime type (Epic 7 plumbing). The caller
// resolves runtimeType from the Agent's RuntimeRef → AgentRuntime.spec.type
// (the dispatch path already loads the AgentRuntime); Resolve keys the
// vendor-neutral mapping off it.
//
// The returned Injection is meant to flow straight into sandbox.PodSpec.Env /
// .Volumes / .Mounts at claim/bind time — no further interpretation, and
// nothing to log.
//
// It fails CLOSED, propagating credinject's fail-closed guarantees, when:
//   - the Agent's credential class / runtime pair has no injection mapping, or
//   - the credential SecretRef names no Secret, or
//   - the Agent sets a modelEndpointRef but the runtime has no base-URL env
//     mapping (an endpoint the runtime could never be pointed at is an
//     operator error, surfaced here rather than silently dropped).
func Resolve(runtimeType string, agent *api.Agent) (credinject.Injection, error) {
	if agent == nil {
		return credinject.Injection{}, fmt.Errorf("credential resolution requires a non-nil Agent")
	}

	// 7.1/7.2/7.3 — the per-user BYO credential Secret → runtime-native
	// provider env, via the story 5.4 contract.
	inj, err := credinject.Inject(runtimeType, credinject.CredentialClass(agent.Spec.CredentialClass), agent.Spec.CredentialSecretRef)
	if err != nil {
		return credinject.Injection{}, fmt.Errorf("agent %q credential injection: %w", agent.Name, err)
	}

	// 7.5 — optional BYO model-endpoint. Absent modelEndpointRef means the
	// runtime's own provider default (a Claude-backed Agent), which needs no
	// endpoint env at all.
	if ref := agent.Spec.ModelEndpointRef; ref != nil {
		endpointEnv, err := endpointInjection(runtimeType, *ref)
		if err != nil {
			return credinject.Injection{}, fmt.Errorf("agent %q endpoint injection: %w", agent.Name, err)
		}
		inj.Env = append(inj.Env, endpointEnv)
	}

	return inj, nil
}

// endpointInjection maps a BYO model-endpoint Secret ref (story 7.5) into the
// runtime's base-URL env var, by reference. The endpoint URL rides the same
// per-user Secret discipline as the credential: only the endpoint's URL key is
// read by the kubelet, never by the control plane.
//
// The optional endpoint token (modelendpoint.KeyAPIToken) is intentionally NOT
// injected here: on the OpenAI/Anthropic wire the endpoint's bearer token and
// the provider credential are the SAME Authorization header, so a second env
// under the provider key name would collide with the 7.3 credential env and
// emit an invalid pod. For a token-guarded BYO endpoint the operator supplies
// that token as the Agent's credential Secret (7.3); for a LAN Ollama endpoint
// no token is needed (7.5). This keeps the URL injection — the load-bearing
// half of 7.5 — unambiguous and collision-free.
func endpointInjection(runtimeType string, ref api.SecretRef) (corev1.EnvVar, error) {
	if ref.Name == "" {
		return corev1.EnvVar{}, fmt.Errorf("modelEndpointRef names no Secret; got empty SecretRef")
	}
	envName, ok := baseURLEnv[runtimeType]
	if !ok {
		return corev1.EnvVar{}, fmt.Errorf("no base-URL env mapping for runtime %q; %s", runtimeType, supportedEndpointRuntimesMsg())
	}
	key := ref.Key
	if key == "" {
		key = modelendpoint.KeyEndpointURL
	}
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
				Key:                  key,
			},
		},
	}, nil
}

// supportedEndpointRuntimesMsg lists the runtimes with a base-URL env mapping,
// for fail-closed error messages.
func supportedEndpointRuntimesMsg() string {
	names := make([]string, 0, len(baseURLEnv))
	for rt := range baseURLEnv {
		names = append(names, rt)
	}
	sort.Strings(names)
	return "runtimes with a BYO-endpoint mapping: [" + strings.Join(names, " ") + "]"
}
