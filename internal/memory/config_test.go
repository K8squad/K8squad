package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// DefaultConfig pins the zero-config boot shape: stock local DSN, local
// deterministic embedder model, standard HTTP port.
func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.DatabaseURL == "" {
		t.Fatal("default DatabaseURL must be set (shared-DSN store of record)")
	}
	if cfg.EmbedderModel != "local-default" {
		t.Fatalf("default EmbedderModel = %q, want local-default", cfg.EmbedderModel)
	}
	if cfg.HTTPPort != 8080 {
		t.Fatalf("default HTTPPort = %d, want 8080", cfg.HTTPPort)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Run("empty path runs purely from defaults", func(t *testing.T) {
		cfg, err := LoadConfig("")
		if err != nil {
			t.Fatalf("empty path: %v", err)
		}
		if cfg != DefaultConfig() {
			t.Fatalf("empty path must return the defaults, got %+v", cfg)
		}
	})

	t.Run("missing file degrades to defaults", func(t *testing.T) {
		cfg, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
		if err != nil {
			t.Fatalf("missing file must not fail: %v", err)
		}
		if cfg != DefaultConfig() {
			t.Fatalf("missing file must return the defaults, got %+v", cfg)
		}
	})

	t.Run("valid file overrides defaults field by field", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		body := []byte(`{"databaseUrl":"postgres://cfg:5432/ksquad","httpPort":9090}`)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("valid file: %v", err)
		}
		if cfg.DatabaseURL != "postgres://cfg:5432/ksquad" {
			t.Fatalf("databaseUrl override not applied: %+v", cfg)
		}
		if cfg.HTTPPort != 9090 {
			t.Fatalf("httpPort override not applied: %+v", cfg)
		}
		if cfg.EmbedderModel != "local-default" {
			t.Fatalf("unset fields must keep defaults, got %+v", cfg)
		}
	})

	t.Run("malformed JSON fails closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("malformed config JSON must return an error")
		}
	})

	t.Run("unreadable path fails closed", func(t *testing.T) {
		// A directory path: ReadFile errors with something other than
		// not-exist, which must surface instead of silently booting defaults.
		if _, err := LoadConfig(t.TempDir()); err == nil {
			t.Fatal("unreadable config path must return an error")
		}
	})
}

// Validate fails closed on a config that cannot boot (no store of record).
func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("empty databaseUrl must fail validation")
	}
	if err := (Config{DatabaseURL: "postgres://postgres:pg@localhost:5432/ksquad"}).Validate(); err != nil {
		t.Fatalf("valid config must pass, got %v", err)
	}
}
