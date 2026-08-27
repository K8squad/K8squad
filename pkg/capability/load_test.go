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

package capability

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMCPConfig(t *testing.T) {
	t.Run("empty path is no demand", func(t *testing.T) {
		eps, err := LoadMCPConfig("")
		require.NoError(t, err)
		assert.Nil(t, eps)
	})

	t.Run("valid IR parses", func(t *testing.T) {
		raw, err := RenderMCPConfigData(scopedEndpoints())
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), MCPConfigFile)
		require.NoError(t, os.WriteFile(path, raw, 0o600))

		eps, err := LoadMCPConfig(path)
		require.NoError(t, err)
		require.Len(t, eps, 1)
		assert.Equal(t, "github-mcp", eps[0].Name)
	})

	t.Run("missing file fails closed", func(t *testing.T) {
		_, err := LoadMCPConfig(filepath.Join(t.TempDir(), "absent.json"))
		require.Error(t, err)
	})

	t.Run("unsupported version fails closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), MCPConfigFile)
		require.NoError(t, os.WriteFile(path, []byte(`{"version":99,"endpoints":[]}`), 0o600))
		_, err := LoadMCPConfig(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version")
	})

	t.Run("malformed document fails closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), MCPConfigFile)
		require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))
		_, err := LoadMCPConfig(path)
		require.Error(t, err)
	})
}
