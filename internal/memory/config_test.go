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

// Unit coverage for the pure (no-Postgres) surface of the ksquad-memory package: config
// load/validate and the pgvector text encoder. These paths are exercised by the default unit
// lane (`go test ./...`, no DB) — the DB-backed store round trip lives in integration_test.go
// behind the `integration` build tag (ISI-2714 coverage-lane split).
package memory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.NotEmpty(t, cfg.DatabaseURL, "default DSN must be populated so the service can boot zero-config")
	assert.Equal(t, "local-default", cfg.EmbedderModel)
	assert.Equal(t, 8080, cfg.HTTPPort)
	// A zero-config default must be self-consistent (fail-closed Validate passes on it).
	require.NoError(t, cfg.Validate())
}

func TestLoadConfig_EmptyPathReturnsDefaults(t *testing.T) {
	cfg, err := LoadConfig("")
	require.NoError(t, err)
	assert.Equal(t, DefaultConfig(), cfg)
}

func TestLoadConfig_MissingFileReturnsDefaults(t *testing.T) {
	// A non-existent path is not an error: the service can run purely from flags/env.
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	require.NoError(t, err)
	assert.Equal(t, DefaultConfig(), cfg)
}

func TestLoadConfig_FileOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"databaseUrl":"postgres://u:p@db:5432/mem","embedderEndpoint":"http://embed:9000","embedderModel":"m2","httpPort":9090}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "postgres://u:p@db:5432/mem", cfg.DatabaseURL)
	assert.Equal(t, "http://embed:9000", cfg.EmbedderEndpoint)
	assert.Equal(t, "m2", cfg.EmbedderModel)
	assert.Equal(t, 9090, cfg.HTTPPort)
}

func TestLoadConfig_MalformedJSONErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config")
}

func TestConfigValidate(t *testing.T) {
	require.NoError(t, Config{DatabaseURL: "postgres://x"}.Validate())

	err := Config{DatabaseURL: ""}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "databaseUrl is required")
}

func TestEncodeVector(t *testing.T) {
	// pgvector text literal: "[v1,v2,...]" with exact (shortest-round-trip) float rendering.
	assert.Equal(t, "[]", encodeVector(nil))
	assert.Equal(t, "[]", encodeVector([]float32{}))
	assert.Equal(t, "[0.5]", encodeVector([]float32{0.5}))
	assert.Equal(t, "[0.1,0.2,0.3]", encodeVector([]float32{0.1, 0.2, 0.3}))
	assert.Equal(t, "[-1.5,0,2.25]", encodeVector([]float32{-1.5, 0, 2.25}))
}
