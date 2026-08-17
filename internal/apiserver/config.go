package apiserver

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the ksquad-apiserver host configuration. DatabaseURL is the shared Postgres DSN
// (store of record, §17.3/ADR-001) the discussion surface and read models query. SessionCookie
// overrides the forwarded session-cookie name to match a BFF running with a non-default
// KSQUAD_SESSION_COOKIE.
type Config struct {
	DatabaseURL   string `json:"databaseUrl"`
	HTTPPort      int    `json:"httpPort"`
	SessionCookie string `json:"sessionCookie"`
}

// DefaultConfig returns the zero-config defaults; env/flags/file override these.
func DefaultConfig() Config {
	return Config{
		// #nosec G101 -- localhost dev/default DSN with the stock Postgres password, not a real credential; env/flags/file override it in any real deployment.
		DatabaseURL:   "postgres://postgres:password@localhost:5432/ksquad?sslmode=disable",
		HTTPPort:      8080,
		SessionCookie: SessionCookieName,
	}
}

// LoadConfig reads a JSON config file over the defaults. A missing path returns the defaults so
// the host can run purely from flags/env (mirrors internal/memory.LoadConfig).
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
	if cfg.SessionCookie == "" {
		cfg.SessionCookie = SessionCookieName
	}
	return cfg, nil
}

// Validate fails closed on an unusable configuration.
func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("databaseUrl is required")
	}
	if c.HTTPPort <= 0 || c.HTTPPort > 65535 {
		return fmt.Errorf("httpPort %d out of range", c.HTTPPort)
	}
	return nil
}
