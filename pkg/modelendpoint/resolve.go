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

// Package modelendpoint is the §10.3 model-endpoint seam (stories 5.7 +
// 5.11, ADR-026): an Agent's model provider is CONFIG, not an
// AgentRuntime.type and not a new image. This package owns the two halves
// of that seam the control plane is load-bearing for:
//
//   - RESOLUTION (resolve.go, 5.7 + 7.5 credential shape). Agent.spec.
//     modelEndpointRef → a per-user Secret (endpoint URL + optional token)
//     → a resolved Endpoint. Fail-closed: a dangling Secret, a missing
//     endpointURL key, or a malformed URL is an error, never a silent
//     fallback to a paid provider (weak local models must not fail
//     silently mid-Run — the 5.7 acceptance).
//
//   - MID-RUN SWITCH (fallback.go, 5.11). On a rate_limited signal the
//     reconciler/shim consults the switch decision core: a configured
//     fallback switches the SAME Run to the fallback model/endpoint
//     (keeping the coordination claim — no re-dispatch), records which
//     model served which portion (Run.status.modelSegments provenance),
//     and meters the activation (13.9). With NO fallback configured the
//     decision is the 2.11 scheduled-timer pause, which already exists
//     (pkg/coord/resume.go).
//
// The runtime-facing half — a shim actually dialing the OpenAI-compatible
// wire — is story 5.8/5.10 (ISI-2114/ISI-2296) and deliberately NOT here:
// this package hands the shim a fully-validated RuntimeConfig and keeps
// zero knowledge of any specific runtime flavor.
package modelendpoint

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// Secret keys of the 7.5 BYO-endpoint credential shape. endpointURL is
// required; apiToken is optional (a LAN Ollama endpoint needs no token).
// The `url`/`token` spellings are accepted aliases so operators are not
// bitten by key spelling — but resolution never guesses past these.
const (
	KeyEndpointURL = "endpointURL"
	KeyAPIToken    = "apiToken"

	aliasURL   = "url"
	aliasToken = "token"
)

// Model-Per-Role (ISI-4430) well-known singleton. The system-default model
// tier lives in ONE ModelConfig named "default" in the operator namespace —
// the floor of the tier-as-a-unit walk (D4). Namespaced-singleton identity is
// a convention the resolver enforces (O3b), not a schema constraint.
const (
	// DefaultModelConfigName is the well-known name of the system-default
	// ModelConfig singleton the resolver reads.
	DefaultModelConfigName = "default"

	// DefaultSystemNamespace is the fallback operator namespace the resolver
	// looks in for the ModelConfig singleton when Resolver.SystemNamespace is
	// unset. Mirrors cmd/operator's defaultSystemNamespace.
	DefaultSystemNamespace = "k8squad-system"
)

// Tier names which of the three Model-Per-Role tiers supplied the effective
// (primary, fallback) pair (ISI-4430 D4). It rides back to callers so S4 can
// stamp provenance ("which tier chose this model") without re-deriving it.
type Tier string

const (
	// TierAgent: the effective model came from Agent.spec.model (highest).
	TierAgent Tier = "agent"
	// TierRole: Agent.spec.model was empty; Role.spec.model supplied it.
	TierRole Tier = "role"
	// TierDefault: neither agent nor role set a model; the ModelConfig
	// singleton's spec.model is the floor (D3 — must always exist).
	TierDefault Tier = "default"
)

// ErrNoModel is the fail-closed sentinel (ISI-4430 D3/D4): no tier — not even
// the system-default ModelConfig — yielded a primary model. Typed so callers
// distinguish a misconfigured install ("no model anywhere") from a transient
// API read error or a dangling endpoint Secret. Returned by ResolveEffective;
// NEVER an empty-model Endpoint.
var ErrNoModel = &ErrUnresolved{Reason: "no tier (agent, role, or system-default ModelConfig) supplies a model — fail-closed (ISI-4430 D3)"}

// Endpoint is one resolved model endpoint (arch §10.3): a base URL riding
// an OpenAI-compatible (or Ollama-native) wire, an optional bearer token,
// and the model name served from it. BaseURL empty means the runtime's own
// provider default (a Claude-backed agent with no modelEndpointRef) — the
// seam treats "no BYO endpoint" as a first-class value, not an error.
type Endpoint struct {
	// Model is the model name to serve from this endpoint.
	Model string

	// BaseURL is the OpenAI-compatible endpoint root ("" = provider
	// default). Validated at resolution: http/https scheme + non-empty
	// host.
	BaseURL string

	// Token is the optional bearer token. NEVER rendered into logs or
	// String(); it crosses the seam to the shim only via RuntimeConfig.
	Token string

	// SecretName records WHICH per-user Secret this endpoint resolved from
	// (provenance — 5.11 attribution and 8.8 dashboard indicators key off
	// it). Empty when no Secret was involved.
	SecretName string
}

// RuntimeConfig is the validated, runtime-facing injection payload: the
// shim (5.8) writes exactly this into the runtime's model config — base
// URL, model, token. It exists as its own type so the token's blast radius
// is named: only this struct and the shim may hold it.
type RuntimeConfig struct {
	Model   string
	BaseURL string
	Token   string
}

// RuntimeConfig renders the injection payload for the shim seam.
func (e Endpoint) RuntimeConfig() RuntimeConfig {
	return RuntimeConfig{Model: e.Model, BaseURL: e.BaseURL, Token: e.Token}
}

// String renders the endpoint for logs/metrics with the token redacted —
// an endpoint URL and model are operator-visible facts; the credential is
// not. The #nosec discipline: nothing in this package ever formats
// e.Token.
func (e Endpoint) String() string {
	if e.BaseURL == "" {
		return fmt.Sprintf("provider-default/%s", e.Model)
	}
	return fmt.Sprintf("%s/%s", e.BaseURL, e.Model)
}

// ErrUnresolved names the fail-closed resolution failures. Typed (not
// sentinel strings) so the webhook and the reconciler can distinguish
// "misconfigured Agent" (reject/hold) from "API read error" (retry).
type ErrUnresolved struct {
	// SecretNamespace/SecretName identify the Secret that failed to
	// resolve (empty when the failure is not Secret-shaped).
	SecretNamespace string
	SecretName      string
	// Reason is the operator-facing failure cause.
	Reason string
	// Err is the underlying cause when one exists (read error, URL parse
	// error), for wrapping.
	Err error
}

func (e *ErrUnresolved) Error() string {
	if e.SecretName != "" {
		return fmt.Sprintf("modelendpoint: Secret %s/%s: %s", e.SecretNamespace, e.SecretName, e.Reason)
	}
	return fmt.Sprintf("modelendpoint: %s", e.Reason)
}

func (e *ErrUnresolved) Unwrap() error { return e.Err }

// Resolver resolves an Agent's primary and fallback endpoints against the
// Kubernetes API through a controller-runtime reader (the webhook passes
// its admission reader; the reconciler its manager client — the seam stays
// unit-testable against a fake).
type Resolver struct {
	// Reader reads Agents/Roles/ModelConfig/Secrets. In production it is the
	// manager's CACHE-backed client, so ResolveEffective's ModelConfig Get is
	// served from the informer cache (the "cached Get of the singleton" seam),
	// not a live API round-trip; tests pass a fake client.
	Reader client.Reader

	// SystemNamespace is where the ModelConfig "default" singleton lives. Empty
	// means DefaultSystemNamespace ("k8squad-system"). Wired from the
	// operator's POD_NAMESPACE so the seam stays overridable in tests.
	SystemNamespace string
}

// systemNamespace returns the configured operator namespace or the default.
func (r *Resolver) systemNamespace() string {
	if r.SystemNamespace != "" {
		return r.SystemNamespace
	}
	return DefaultSystemNamespace
}

// tierInput is one Model-Per-Role tier's raw resolution inputs (ISI-4430 D4):
// its model name, its optional fallback, the endpoint Secret its credentials
// resolve through, and the namespace those Secrets live in. resolvePrimary /
// resolveFallback turn it into Endpoints — the SINGLE place endpoint+credential
// resolution happens, so agent-only (Resolve/ResolveFallback) and tier-as-a-unit
// (ResolveEffective) can never drift.
type tierInput struct {
	tier        Tier
	model       string
	fallback    *api.FallbackModel
	endpointRef *api.SecretRef
	namespace   string
}

// resolvePrimary turns a tier's model+endpointRef into its primary Endpoint. No
// endpointRef means the provider-default endpoint (BaseURL "") — "no BYO
// endpoint" is a first-class value, not an error.
func (r *Resolver) resolvePrimary(ctx context.Context, t tierInput) (Endpoint, error) {
	if t.endpointRef == nil {
		return Endpoint{Model: t.model}, nil
	}
	return r.ResolveRef(ctx, t.namespace, t.endpointRef, t.model)
}

// resolveFallback turns a tier's fallback into an Endpoint. The fallback carries
// its own endpoint Secret, or — when unset — rides the SAME tier's primary
// endpoint (same wire, second model) per the FallbackModel contract. A lower
// tier's endpoint is NEVER grafted here: fallback stays inside the winning tier
// (D4). ok is false when this tier configures no fallback.
func (r *Resolver) resolveFallback(ctx context.Context, t tierInput) (endpoint Endpoint, ok bool, err error) {
	if t.fallback == nil {
		return Endpoint{}, false, nil
	}
	ref := t.fallback.ModelEndpointRef
	if ref == nil {
		ref = t.endpointRef
	}
	if ref == nil {
		return Endpoint{Model: t.fallback.Model}, true, nil
	}
	ep, err := r.ResolveRef(ctx, t.namespace, ref, t.fallback.Model)
	if err != nil {
		return Endpoint{}, true, err
	}
	return ep, true, nil
}

// agentTier is the agent-tier resolution inputs (highest tier).
func agentTier(agent *api.Agent) tierInput {
	return tierInput{
		tier:        TierAgent,
		model:       agent.Spec.Model,
		fallback:    agent.Spec.FallbackModel,
		endpointRef: agent.Spec.ModelEndpointRef,
		namespace:   agent.Namespace,
	}
}

// Resolve returns the Agent's primary model endpoint (agent tier only — the
// pre-Model-Per-Role seam kept for callers that have no Role/default context;
// S4 moves them to ResolveEffective). An Agent with no modelEndpointRef
// resolves to the provider-default endpoint (BaseURL "", the Agent's own
// model), so callers get ONE type for both postures.
func (r *Resolver) Resolve(ctx context.Context, agent *api.Agent) (Endpoint, error) {
	return r.resolvePrimary(ctx, agentTier(agent))
}

// ResolveFallback returns the Agent's fallback endpoint (5.11), agent tier
// only. The fallback carries its own endpoint Secret, or — when unset —
// resolves against the Agent's OWN endpoint Secret. ok is false when the Agent
// configures no fallback at all.
func (r *Resolver) ResolveFallback(ctx context.Context, agent *api.Agent) (endpoint Endpoint, ok bool, err error) {
	return r.resolveFallback(ctx, agentTier(agent))
}

// ResolveEffective is the Model-Per-Role (ISI-4430) core seam: it walks the
// three model tiers TIER-AS-A-UNIT (D4) and returns the effective (primary,
// fallback) pair from the HIGHEST tier whose model is non-empty —
// Agent.spec.model → Role.spec.model → the ModelConfig "default" singleton. A
// lower tier's fallback is NEVER grafted onto a higher tier's primary: the pair
// is taken as a unit from the winning tier.
//
// Endpoint/credential resolution follows the WINNING tier (D5): the agent tier
// uses Agent.spec.modelEndpointRef; the role tier uses the provider/operator
// default endpoint (role-level modelEndpointRef is O1, deferred — a role-tier
// model has no BYO endpoint of its own in v1); the default tier uses
// ModelConfig.spec.modelEndpointRef. In every tier an unset fallback endpoint
// rides that tier's own primary endpoint.
//
// FAIL-CLOSED (D3): if no tier — not even the system-default ModelConfig —
// yields a primary model, it returns ErrNoModel and a zero primary Endpoint,
// never an empty-model Endpoint. A transient ModelConfig read error (not
// NotFound) propagates as itself so the caller can retry.
//
// tier reports which tier won (for S4 provenance). ok reports whether a
// fallback was resolved for the winning tier.
func (r *Resolver) ResolveEffective(ctx context.Context, agent *api.Agent, role *api.Role) (primary Endpoint, fallback Endpoint, tier Tier, ok bool, err error) {
	win, err := r.winningTier(ctx, agent, role)
	if err != nil {
		return Endpoint{}, Endpoint{}, "", false, err
	}

	primary, err = r.resolvePrimary(ctx, win)
	if err != nil {
		return Endpoint{}, Endpoint{}, win.tier, false, err
	}
	fallback, ok, err = r.resolveFallback(ctx, win)
	if err != nil {
		return Endpoint{}, Endpoint{}, win.tier, false, err
	}
	return primary, fallback, win.tier, ok, nil
}

// winningTier selects the highest tier with a non-empty model (D4). It only
// reads the ModelConfig singleton when agent and role both fall through, so the
// common path costs no extra API read. Returns ErrNoModel when nothing supplies
// a model (fail-closed).
func (r *Resolver) winningTier(ctx context.Context, agent *api.Agent, role *api.Role) (tierInput, error) {
	if agent != nil && agent.Spec.Model != "" {
		return agentTier(agent), nil
	}
	if role != nil && role.Spec.Model != "" {
		// O1 deferred: no role-level endpointRef in v1 — the role-tier model
		// uses the provider/operator default endpoint. Its fallback may still
		// carry its own Secret, resolved in role.Namespace.
		return tierInput{
			tier:      TierRole,
			model:     role.Spec.Model,
			fallback:  role.Spec.FallbackModel,
			namespace: role.Namespace,
		}, nil
	}

	mc, found, err := r.defaultModelConfig(ctx)
	if err != nil {
		return tierInput{}, err
	}
	if found && mc.Spec.Model != "" {
		return tierInput{
			tier:        TierDefault,
			model:       mc.Spec.Model,
			fallback:    mc.Spec.FallbackModel,
			endpointRef: mc.Spec.ModelEndpointRef,
			namespace:   mc.Namespace,
		}, nil
	}
	return tierInput{}, ErrNoModel
}

// defaultModelConfig reads the well-known ModelConfig singleton ("default" in
// the operator namespace). found=false on NotFound (an install without the
// default CR yet — the caller decides whether that is fail-closed); a genuine
// read error propagates for retry.
func (r *Resolver) defaultModelConfig(ctx context.Context) (mc api.ModelConfig, found bool, err error) {
	key := client.ObjectKey{Namespace: r.systemNamespace(), Name: DefaultModelConfigName}
	if err := r.Reader.Get(ctx, key, &mc); err != nil {
		if apierrors.IsNotFound(err) {
			return api.ModelConfig{}, false, nil
		}
		return api.ModelConfig{}, false, &ErrUnresolved{
			Reason: fmt.Sprintf("system-default ModelConfig %s/%s read failed: %v", key.Namespace, key.Name, err),
			Err:    err,
		}
	}
	return mc, true, nil
}

// ResolveRef reads one endpoint Secret and validates its shape (7.5):
// endpointURL (or the url alias) present + parseable http(s) URL with a
// host; apiToken (or token alias) optional. ref.Key, when set, names the
// URL key itself (the SecretRef.Key contract: "empty means the
// consumer-defined default key"). model is the model name the caller wants
// served from this endpoint (the Agent's own or its fallback's).
func (r *Resolver) ResolveRef(ctx context.Context, namespace string, ref *api.SecretRef, model string) (Endpoint, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := r.Reader.Get(ctx, key, &secret); err != nil {
		return Endpoint{}, &ErrUnresolved{
			SecretNamespace: namespace,
			SecretName:      ref.Name,
			Reason:          fmt.Sprintf("endpoint Secret read failed: %v", err),
			Err:             err,
		}
	}

	urlKey := KeyEndpointURL
	if ref.Key != "" {
		urlKey = ref.Key
	}
	rawURL := secretKey(&secret, urlKey, aliasURL)
	if rawURL == "" {
		return Endpoint{}, &ErrUnresolved{
			SecretNamespace: namespace,
			SecretName:      ref.Name,
			Reason:          fmt.Sprintf("missing %q key (BYO endpoint Secret needs an endpointURL; arch §11 / story 7.5 shape)", urlKey),
		}
	}

	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Endpoint{}, &ErrUnresolved{
			SecretNamespace: namespace,
			SecretName:      ref.Name,
			Reason:          fmt.Sprintf("endpointURL %q is not a valid http(s) URL with a host", rawURL),
			Err:             err,
		}
	}

	return Endpoint{
		Model:      model,
		BaseURL:    strings.TrimRight(parsed.String(), "/"),
		Token:      secretKey(&secret, KeyAPIToken, aliasToken),
		SecretName: ref.Name,
	}, nil
}

// secretKey returns the Secret data at key, falling back to alias when the
// canonical key is absent. Empty string when neither is set.
func secretKey(s *corev1.Secret, key, alias string) string {
	if v, ok := s.Data[key]; ok {
		return strings.TrimSpace(string(v))
	}
	if v, ok := s.Data[alias]; ok {
		return strings.TrimSpace(string(v))
	}
	return ""
}
