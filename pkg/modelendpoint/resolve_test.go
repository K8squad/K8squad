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

package modelendpoint

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ns = "squad-alpha"

func newResolver(t *testing.T, objs ...client.Object) *Resolver {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return &Resolver{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()}
}

func endpointSecret(name string, data map[string][]byte) *corev1.Secret {
	if data == nil {
		data = map[string][]byte{}
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: data}
}

func byoAgent(mutate func(*api.AgentSpec)) *api.Agent {
	a := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "amelia", Namespace: ns},
		Spec: api.AgentSpec{
			RuntimeRef:          api.ObjectRef{Name: "claude-stable"},
			RoleRef:             api.ObjectRef{Name: "coder"},
			CredentialSecretRef: api.SecretRef{Name: "amelia-claude-token"},
			Model:               "qwen3:14b",
		},
	}
	if mutate != nil {
		mutate(&a.Spec)
	}
	return a
}

// TestResolveProviderDefaultPosture: an Agent with NO modelEndpointRef
// resolves to the provider-default endpoint — "no BYO endpoint" is a
// first-class value (BaseURL ""), never an error (5.7: the field is
// optional at runtime).
func TestResolveProviderDefaultPosture(t *testing.T) {
	r := newResolver(t)
	ep, err := r.Resolve(context.Background(), byoAgent(nil))
	require.NoError(t, err)
	assert.Equal(t, "qwen3:14b", ep.Model)
	assert.Empty(t, ep.BaseURL, "no ref means provider default, not an error")
	assert.Empty(t, ep.Token)
	assert.Empty(t, ep.SecretName)
	assert.Equal(t, "provider-default/qwen3:14b", ep.String())
}

// TestResolveRefUsesSecretReader is the ISI-5420 seam contract: when a dedicated
// SecretReader is set, ResolveRef reads the endpoint Secret through IT, not the
// primary Reader. The primary Reader here is a ksquad-only fake (no corev1 in its
// scheme) — exactly the apiserver informer cache that fail-closed BYO-endpoint
// agents to a 502 — yet resolution succeeds because the Secret Get rides the
// corev1-capable SecretReader. A nil SecretReader (every other caller) falls back
// to the Reader, so the single-reader seam is unchanged for the webhook/reconciler.
func TestResolveRefUsesSecretReader(t *testing.T) {
	// Primary reader: ksquad CRDs only — a corev1.Secret Get through it errors.
	ksquadOnly := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(ksquadOnly))
	cacheReader := fake.NewClientBuilder().WithScheme(ksquadOnly).Build()

	// Dedicated secret reader: corev1 scheme, holding the endpoint Secret.
	corev1Scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(corev1Scheme))
	secretReader := fake.NewClientBuilder().WithScheme(corev1Scheme).
		WithObjects(endpointSecret("amelia-ollama", map[string][]byte{
			"endpointURL": []byte("http://ollama.svc:11434/v1"),
			"apiToken":    []byte("sekrit"),
		})).Build()

	ref := &api.SecretRef{Name: "amelia-ollama"}

	t.Run("resolves through the dedicated secret reader", func(t *testing.T) {
		r := &Resolver{Reader: cacheReader, SecretReader: secretReader}
		ep, err := r.ResolveRef(context.Background(), ns, ref, "qwen3:14b")
		require.NoError(t, err)
		assert.Equal(t, "http://ollama.svc:11434/v1", ep.BaseURL)
		assert.Equal(t, "sekrit", ep.Token)
		assert.Equal(t, "amelia-ollama", ep.SecretName)
	})

	t.Run("falls back to Reader and fail-closes when SecretReader is nil", func(t *testing.T) {
		r := &Resolver{Reader: cacheReader}
		_, err := r.ResolveRef(context.Background(), ns, ref, "qwen3:14b")
		require.Error(t, err, "a Secret Get through the ksquad-only Reader must fail (pre-fix 502)")
	})
}

// TestResolveBYOEndpoint: the 7.5 happy shape — endpointURL + optional
// apiToken — resolves to the OpenAI-compatible base URL with the model
// served from it, and the Secret name rides along as provenance.
func TestResolveBYOEndpoint(t *testing.T) {
	r := newResolver(t, endpointSecret("amelia-ollama", map[string][]byte{
		"endpointURL": []byte("http://ollama.svc:11434/ "),
		"apiToken":    []byte("sekrit"),
	}))
	a := byoAgent(func(s *api.AgentSpec) { s.ModelEndpointRef = &api.SecretRef{Name: "amelia-ollama"} })

	ep, err := r.Resolve(context.Background(), a)
	require.NoError(t, err)
	assert.Equal(t, "qwen3:14b", ep.Model)
	assert.Equal(t, "http://ollama.svc:11434", ep.BaseURL, "trailing slash trimmed, whitespace stripped")
	assert.Equal(t, "sekrit", ep.Token)
	assert.Equal(t, "amelia-ollama", ep.SecretName)

	rc := ep.RuntimeConfig()
	assert.Equal(t, RuntimeConfig{Model: "qwen3:14b", BaseURL: "http://ollama.svc:11434", Token: "sekrit"}, rc)
	assert.NotContains(t, ep.String(), "sekrit", "String must never render the token")
	assert.Zero(t, ep.ContextWindow, "no contextWindow key declared → 0 (caller falls back to the family default)")
}

// TestResolveContextWindow is the ISI-5540 per-endpoint window: a BYO endpoint
// that declares its true context window surfaces it on Endpoint.ContextWindow
// (so the pre-dispatch budget resolver uses 128K for a hosted deepseek instead
// of the 64K family floor), while a missing / non-integer / non-positive value
// is IGNORED — never a resolution error — so the caller falls back to the
// conservative family default (under-budget safe).
func TestResolveContextWindow(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  int64
	}{
		{"declared 128K", "131072", 131072},
		{"declared 64K", "65536", 65536},
		{"non-integer ignored", "lots", 0},
		{"zero ignored", "0", 0},
		{"negative ignored", "-5", 0},
		{"whitespace-padded integer", " 131072 ", 131072},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newResolver(t, endpointSecret("deepseek-ep", map[string][]byte{
				"endpointURL":   []byte("https://api.deepseek.com/v1"),
				"contextWindow": []byte(tc.value),
			}))
			a := byoAgent(func(s *api.AgentSpec) {
				s.Model = "deepseek-chat"
				s.ModelEndpointRef = &api.SecretRef{Name: "deepseek-ep"}
			})
			ep, err := r.Resolve(context.Background(), a)
			require.NoError(t, err, "a malformed contextWindow must never fail-close the endpoint")
			assert.Equal(t, tc.want, ep.ContextWindow)
		})
	}
}

// TestResolveAcceptsURLAliasAndKeyOverride: the `url` alias satisfies the
// shape; an explicit ref.Key names the URL key itself (SecretRef.Key
// contract).
func TestResolveAcceptsURLAliasAndKeyOverride(t *testing.T) {
	r := newResolver(t, endpointSecret("alias-ep", map[string][]byte{
		"url": []byte("https://vllm.internal:8443/"),
	}))
	a := byoAgent(func(s *api.AgentSpec) { s.ModelEndpointRef = &api.SecretRef{Name: "alias-ep"} })
	ep, err := r.Resolve(context.Background(), a)
	require.NoError(t, err)
	assert.Equal(t, "https://vllm.internal:8443", ep.BaseURL)

	a2 := byoAgent(func(s *api.AgentSpec) {
		s.ModelEndpointRef = &api.SecretRef{Name: "alias-ep", Key: "url"}
	})
	ep2, err := r.Resolve(context.Background(), a2)
	require.NoError(t, err)
	assert.Equal(t, "https://vllm.internal:8443", ep2.BaseURL)
}

// TestResolveFailClosed: every mis-configuration that would strand a Run
// mid-flight is an ErrUnresolved — dangling Secret, missing endpointURL,
// malformed URL, non-http scheme — never a silent provider-default
// fallback (5.7: weak local models must not fail silently).
func TestResolveFailClosed(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		secret  *corev1.Secret
		ref     api.SecretRef
		wantMsg string
	}{
		{"dangling secret", nil, api.SecretRef{Name: "ghost"},
			"read failed"},
		{"missing endpointURL key", endpointSecret("nokey", nil), api.SecretRef{Name: "nokey"},
			`missing "endpointURL" key`},
		{"malformed url", endpointSecret("bad", map[string][]byte{"endpointURL": []byte("not a url")}), api.SecretRef{Name: "bad"},
			"not a valid http(s) URL"},
		{"wrong scheme", endpointSecret("ftp", map[string][]byte{"endpointURL": []byte("ftp://h/x")}), api.SecretRef{Name: "ftp"},
			"not a valid http(s) URL"},
		{"no host", endpointSecret("nohost", map[string][]byte{"endpointURL": []byte("http://")}), api.SecretRef{Name: "nohost"},
			"not a valid http(s) URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{byoAgent(nil)}
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			r := newResolver(t, objs...)
			a := byoAgent(func(s *api.AgentSpec) { s.ModelEndpointRef = &tc.ref })
			_, err := r.Resolve(ctx, a)
			require.Error(t, err)
			var ue *ErrUnresolved
			require.ErrorAs(t, err, &ue, "fail-closed errors are typed so callers can reject vs retry")
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// TestResolveFallbackInheritsPrimaryEndpoint: the FallbackModel contract —
// no endpoint ref of its own means the Agent's OWN endpoint Secret with
// the fallback model name (same wire, second model).
func TestResolveFallbackInheritsPrimaryEndpoint(t *testing.T) {
	r := newResolver(t, endpointSecret("amelia-ollama", map[string][]byte{
		"endpointURL": []byte("http://ollama.svc:11434"),
	}))
	a := byoAgent(func(s *api.AgentSpec) {
		s.ModelEndpointRef = &api.SecretRef{Name: "amelia-ollama"}
		s.FallbackModel = &api.FallbackModel{Model: "llama3:8b"}
	})
	ep, ok, err := r.ResolveFallback(context.Background(), a)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "llama3:8b", ep.Model, "fallback model name, primary's endpoint")
	assert.Equal(t, "http://ollama.svc:11434", ep.BaseURL)
}

// TestResolveFallbackOwnEndpointAndDefaultPostures: a fallback with its
// own Secret resolves independently; no fallback configured → ok=false; a
// fallback with no BYO endpoint anywhere → provider-default endpoint.
func TestResolveFallbackOwnEndpointAndDefaultPostures(t *testing.T) {
	ctx := context.Background()

	r := newResolver(t, endpointSecret("backup-ep", map[string][]byte{
		"endpointURL": []byte("https://backup.example.com"),
	}))
	own := byoAgent(func(s *api.AgentSpec) {
		s.FallbackModel = &api.FallbackModel{Model: "gpt-oss:20b", ModelEndpointRef: &api.SecretRef{Name: "backup-ep"}}
	})
	ep, ok, err := r.ResolveFallback(ctx, own)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "https://backup.example.com", ep.BaseURL)

	none := byoAgent(nil)
	_, ok, err = r.ResolveFallback(ctx, none)
	require.NoError(t, err)
	assert.False(t, ok, "no fallbackModel configured means ok=false — the 2.11 pause path")

	providerDefault := byoAgent(func(s *api.AgentSpec) {
		s.FallbackModel = &api.FallbackModel{Model: "claude-haiku"}
	})
	ep, ok, err = r.ResolveFallback(ctx, providerDefault)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Empty(t, ep.BaseURL, "fallback on the runtime's own provider default")
	assert.Equal(t, "claude-haiku", ep.Model)
}

// TestResolveFallbackFailClosed: a configured-but-unresolvable fallback
// endpoint is an ERROR (never a silent stay-on-throttled-primary).
func TestResolveFallbackFailClosed(t *testing.T) {
	r := newResolver(t)
	a := byoAgent(func(s *api.AgentSpec) {
		s.FallbackModel = &api.FallbackModel{Model: "llama3:8b", ModelEndpointRef: &api.SecretRef{Name: "ghost"}}
	})
	_, _, err := r.ResolveFallback(context.Background(), a)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "ghost"), "names the Secret that failed")
}

// TestResolveCredentialSecretRefPrecedence is the ISI-5519 fix (parent
// ISI-5515, ADR-045 endpoint-URL vs credential split): when an Agent sets
// modelEndpointRef to a URL-only Secret AND credentialSecretRef to the Secret
// holding the real provider token, the resolver reads the token from the
// CREDENTIAL Secret — not the (empty) endpoint Secret — so OPENAI_API_KEY
// carries the sk- key end-to-end instead of defaulting to the literal "ollama"
// (the bmad deepseek 401 root cause). It also pins the backward-compat and
// local/no-auth paths so no existing single-Secret BYO agent regresses.
func TestResolveCredentialSecretRefPrecedence(t *testing.T) {
	ctx := context.Background()

	// AC1: URL-only endpoint Secret + credential Secret holding the real token
	// under apiToken → the credential token wins end-to-end.
	t.Run("credential Secret token overrides empty endpoint Secret (AC1)", func(t *testing.T) {
		r := newResolver(t,
			endpointSecret("deepseek-endpoint", map[string][]byte{
				"endpointURL": []byte("https://api.deepseek.com/v1"),
			}),
			endpointSecret("deepseek-credentials", map[string][]byte{
				"apiToken": []byte("sk-realdeepseekkey"),
			}),
		)
		a := byoAgent(func(s *api.AgentSpec) {
			s.Model = "deepseek-chat"
			s.ModelEndpointRef = &api.SecretRef{Name: "deepseek-endpoint"}
			s.CredentialSecretRef = api.SecretRef{Name: "deepseek-credentials"}
		})
		ep, err := r.Resolve(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, "https://api.deepseek.com/v1", ep.BaseURL)
		assert.Equal(t, "sk-realdeepseekkey", ep.Token, "token resolves from credentialSecretRef, not the endpoint Secret")
		assert.Equal(t, "deepseek-endpoint", ep.SecretName, "provenance stays the ENDPOINT Secret")
		assert.NotContains(t, ep.String(), "sk-realdeepseekkey", "String must never render the token")
	})

	// The `token` alias is honored on the credential Secret exactly as it is on
	// the endpoint Secret (the deepseek-credentials Secret in the field stored
	// the sk- key under `token`).
	t.Run("credential Secret honors the token alias", func(t *testing.T) {
		r := newResolver(t,
			endpointSecret("ep", map[string][]byte{"endpointURL": []byte("https://api.deepseek.com/v1")}),
			endpointSecret("cred", map[string][]byte{"token": []byte("sk-viaalias")}),
		)
		a := byoAgent(func(s *api.AgentSpec) {
			s.ModelEndpointRef = &api.SecretRef{Name: "ep"}
			s.CredentialSecretRef = api.SecretRef{Name: "cred"}
		})
		ep, err := r.Resolve(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, "sk-viaalias", ep.Token)
	})

	// An explicit credentialSecretRef.Key names the token key directly.
	t.Run("credential Secret honors an explicit ref.Key", func(t *testing.T) {
		r := newResolver(t,
			endpointSecret("ep", map[string][]byte{"endpointURL": []byte("https://api.deepseek.com/v1")}),
			endpointSecret("cred", map[string][]byte{"providerKey": []byte("sk-customkey")}),
		)
		a := byoAgent(func(s *api.AgentSpec) {
			s.ModelEndpointRef = &api.SecretRef{Name: "ep"}
			s.CredentialSecretRef = api.SecretRef{Name: "cred", Key: "providerKey"}
		})
		ep, err := r.Resolve(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, "sk-customkey", ep.Token)
	})

	// AC2 backward compat: token in the endpoint Secret only, and the
	// credential Secret carries no model token (its key is credinject's
	// apiKey/claude contract, not apiToken/token) → the endpoint token is kept.
	t.Run("endpoint Secret token kept when credential Secret has no model token (AC2)", func(t *testing.T) {
		r := newResolver(t,
			endpointSecret("single-ep", map[string][]byte{
				"endpointURL": []byte("http://ollama.svc:11434/v1"),
				"apiToken":    []byte("endpoint-token"),
			}),
			endpointSecret("amelia-claude-token", map[string][]byte{
				"apiKey": []byte("sk-ant-irrelevant"),
			}),
		)
		a := byoAgent(func(s *api.AgentSpec) {
			s.ModelEndpointRef = &api.SecretRef{Name: "single-ep"}
			// CredentialSecretRef defaults to amelia-claude-token (byoAgent).
		})
		ep, err := r.Resolve(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, "endpoint-token", ep.Token, "no apiToken/token in the credential Secret ⇒ endpoint token kept")
	})

	// A dangling credentialSecretRef must not fail-closed: the endpoint token
	// (here empty) is preserved, so an endpoint whose own Secret authenticates
	// keeps working and a no-auth one still falls through to the shim default.
	t.Run("unreadable credential Secret preserves endpoint behavior", func(t *testing.T) {
		r := newResolver(t, endpointSecret("ep", map[string][]byte{
			"endpointURL": []byte("http://ollama.svc:11434/v1"),
			"apiToken":    []byte("endpoint-token"),
		}))
		a := byoAgent(func(s *api.AgentSpec) {
			s.ModelEndpointRef = &api.SecretRef{Name: "ep"}
			s.CredentialSecretRef = api.SecretRef{Name: "missing-cred"}
		})
		ep, err := r.Resolve(ctx, a)
		require.NoError(t, err, "a dangling credential Secret is not fail-closed (endpoint already authenticates)")
		assert.Equal(t, "endpoint-token", ep.Token)
	})

	// AC3: a local/no-auth endpoint with neither an endpoint token nor a
	// credential token resolves to an EMPTY token — the shim's "ollama"
	// default is untouched for genuinely local endpoints.
	t.Run("local no-auth endpoint still yields empty token for the ollama fallback (AC3)", func(t *testing.T) {
		r := newResolver(t,
			endpointSecret("local-ollama", map[string][]byte{
				"endpointURL": []byte("http://10.0.0.185:11434/v1"),
			}),
			endpointSecret("amelia-claude-token", map[string][]byte{
				"apiKey": []byte("sk-ant-irrelevant"),
			}),
		)
		a := byoAgent(func(s *api.AgentSpec) {
			s.ModelEndpointRef = &api.SecretRef{Name: "local-ollama"}
		})
		ep, err := r.Resolve(ctx, a)
		require.NoError(t, err)
		assert.Empty(t, ep.Token, "no token anywhere ⇒ empty, so the shim defaults to ollama")
	})

	// The credential token also rides the fallback endpoint (same tier
	// credential, ADR-045) when the fallback carries its own BYO endpoint.
	t.Run("fallback endpoint inherits the credential token", func(t *testing.T) {
		r := newResolver(t,
			endpointSecret("primary-ep", map[string][]byte{"endpointURL": []byte("https://api.deepseek.com/v1")}),
			endpointSecret("fallback-ep", map[string][]byte{"endpointURL": []byte("https://api.deepseek.com/v1")}),
			endpointSecret("deepseek-credentials", map[string][]byte{"apiToken": []byte("sk-shared")}),
		)
		a := byoAgent(func(s *api.AgentSpec) {
			s.ModelEndpointRef = &api.SecretRef{Name: "primary-ep"}
			s.CredentialSecretRef = api.SecretRef{Name: "deepseek-credentials"}
			s.FallbackModel = &api.FallbackModel{Model: "deepseek-reasoner", ModelEndpointRef: &api.SecretRef{Name: "fallback-ep"}}
		})
		ep, ok, err := r.ResolveFallback(ctx, a)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "sk-shared", ep.Token, "fallback BYO endpoint authenticates with the agent's credential token")
	})
}
