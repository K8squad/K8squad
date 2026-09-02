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

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

func TestInjectGitHubTokenByReference(t *testing.T) {
	inj, err := Inject(PurposeGitHub, api.SecretRef{Name: "github-writepath-token"})
	require.NoError(t, err)

	// Both env names land, in table order, each a SecretKeySelector pointing
	// at the SAME Secret+key — never a literal value.
	require.Len(t, inj.Env, 2)
	assert.Equal(t, "GH_TOKEN", inj.Env[0].Name)
	assert.Equal(t, "GITHUB_TOKEN", inj.Env[1].Name)
	for _, e := range inj.Env {
		assert.Empty(t, e.Value, "aux credential must be by-reference, never a literal value")
		require.NotNil(t, e.ValueFrom)
		require.NotNil(t, e.ValueFrom.SecretKeyRef)
		assert.Equal(t, "github-writepath-token", e.ValueFrom.SecretKeyRef.Name)
		// Default Secret key when SecretRef.Key is empty.
		assert.Equal(t, "token", e.ValueFrom.SecretKeyRef.Key)
	}
	assert.Empty(t, inj.Volumes)
	assert.Empty(t, inj.Mounts)
}

func TestInjectHonoursExplicitSecretKey(t *testing.T) {
	inj, err := Inject(PurposeGitHub, api.SecretRef{Name: "gh", Key: "pat"})
	require.NoError(t, err)
	require.Len(t, inj.Env, 2)
	for _, e := range inj.Env {
		assert.Equal(t, "pat", e.ValueFrom.SecretKeyRef.Key)
	}
}

func TestInjectFailsClosedOnUnknownPurpose(t *testing.T) {
	_, err := Inject(Purpose("gitlab-token"), api.SecretRef{Name: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gitlab-token")
	assert.Contains(t, err.Error(), "github-token", "error names the supported purposes")
}

func TestInjectFailsClosedOnEmptyPurpose(t *testing.T) {
	_, err := Inject(Purpose(""), api.SecretRef{Name: "x"})
	require.Error(t, err)
}

func TestInjectFailsClosedOnEmptySecretName(t *testing.T) {
	_, err := Inject(PurposeGitHub, api.SecretRef{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Secret name")
}

func TestValidatePurpose(t *testing.T) {
	require.NoError(t, ValidatePurpose(PurposeGitHub))

	// Empty is invalid — there is no safe default aux tool to guess.
	require.Error(t, ValidatePurpose(Purpose("")))

	err := ValidatePurpose(Purpose("nope"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github-token")
}

func TestKnownPurposesSortedAndComplete(t *testing.T) {
	got := KnownPurposes()
	require.Equal(t, []Purpose{PurposeGitHub}, got)

	// Every known purpose validates and injects — the table is the single
	// source of truth for both.
	for _, p := range got {
		require.NoError(t, ValidatePurpose(p))
		inj, err := Inject(p, api.SecretRef{Name: "s"})
		require.NoError(t, err)
		assert.NotEmpty(t, inj.Env)
	}
}
