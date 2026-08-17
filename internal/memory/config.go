package memory

import (
	"encoding/json"
	"fmt"
	"os"
)

// EmbeddingDim is the fixed dimension of the default local embedder (§7.1). It MUST match the
// `vector(dim)` column type in db/migrations/0001_memory.sql. The embedder itself is config behind a
// seam (§7.1/AC5) so an air-gapped cluster can swap it; a write whose embedding length != EmbeddingDim
// is rejected (detectable dimension mismatch, never a silent truncation).
const EmbeddingDim = 768

// Config is the ksquad-memory service configuration. DatabaseURL is the shared Postgres DSN (the sole
// store of record, §17.3/ADR-001). EmbedderEndpoint sits behind the §7.1 embedder seam — v1 records it
// in provenance; wiring a live embedder client is not this story's schema substrate.
type Config struct {
	DatabaseURL      string `json:"databaseUrl"`
	EmbedderEndpoint string `json:"embedderEndpoint"`
	EmbedderModel    string `json:"embedderModel"`
	HTTPPort         int    `json:"httpPort"`
}

// DefaultConfig returns the zero-config defaults; env/flags/file override these.
func DefaultConfig() Config {
	return Config{
		// #nosec G101 -- localhost dev/default DSN with the stock Postgres password, not a real credential; env/flags/file override it in any real deployment.
		DatabaseURL:   "postgres://postgres:password@localhost:5432/ksquad?sslmode=disable",
		EmbedderModel: "local-default",
		HTTPPort:      8080,
	}
}

// LoadConfig reads a JSON config file over the defaults. A missing path returns the defaults so the
// service can run purely from flags/env.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate fails closed on an unusable configuration.
func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("databaseUrl is required")
	}
	return nil
}
