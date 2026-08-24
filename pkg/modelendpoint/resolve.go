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
	Reader client.Reader
}

// Resolve returns the Agent's primary model endpoint. An Agent with no
// modelEndpointRef resolves to the provider-default endpoint (BaseURL "",
// the Agent's own model) — the common paid-provider shape — so callers get
// ONE type for both postures.
func (r *Resolver) Resolve(ctx context.Context, agent *api.Agent) (Endpoint, error) {
	ref := agent.Spec.ModelEndpointRef
	if ref == nil {
		return Endpoint{Model: agent.Spec.Model}, nil
	}
	return r.ResolveRef(ctx, agent.Namespace, ref, agent.Spec.Model)
}

// ResolveFallback returns the Agent's fallback endpoint (5.11). The
// fallback carries its own endpoint Secret, or — when its
// modelEndpointRef is unset — resolves against the Agent's OWN endpoint
// Secret (same wire, second model) per the FallbackModel contract. ok is
// false when the Agent configures no fallback at all.
func (r *Resolver) ResolveFallback(ctx context.Context, agent *api.Agent) (endpoint Endpoint, ok bool, err error) {
	fb := agent.Spec.FallbackModel
	if fb == nil {
		return Endpoint{}, false, nil
	}
	ref := fb.ModelEndpointRef
	if ref == nil {
		ref = agent.Spec.ModelEndpointRef
	}
	if ref == nil {
		// No BYO endpoint anywhere: the fallback is a second model on the
		// runtime's own provider default.
		return Endpoint{Model: fb.Model}, true, nil
	}
	ep, err := r.ResolveRef(ctx, agent.Namespace, ref, fb.Model)
	if err != nil {
		return Endpoint{}, true, err
	}
	return ep, true, nil
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
