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
