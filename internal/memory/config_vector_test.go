package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ISI-7286 (L5 green-up): the config surface and the pgvector wire encoding
// were the remaining zero-coverage functions in internal/memory. Pin their
// contracts so the ratchet floor stays earned by tests, not by lowering it.

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.DatabaseURL == "" {
		t.Fatal("DefaultConfig: databaseUrl default is empty")
	}
	if cfg.EmbedderModel == "" {
		t.Fatal("DefaultConfig: embedderModel default is empty")
	}
	if cfg.HTTPPort == 0 {
		t.Fatal("DefaultConfig: httpPort default is zero")
	}
}

func TestLoadConfig(t *testing.T) {
	t.Run("empty path returns defaults", func(t *testing.T) {
		cfg, err := LoadConfig("")
		if err != nil {
			t.Fatalf("LoadConfig empty path: %v", err)
		}
		if cfg != DefaultConfig() {
			t.Fatalf("LoadConfig empty path = %+v, want defaults", cfg)
		}
	})

	t.Run("missing file returns defaults", func(t *testing.T) {
		cfg, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
		if err != nil {
			t.Fatalf("LoadConfig missing file: %v", err)
		}
		if cfg.DatabaseURL == "" {
			t.Fatal("LoadConfig missing file: defaults not applied")
		}
	})

	t.Run("unreadable path errors", func(t *testing.T) {
		// A directory read fails with a non-NotExist error: must surface.
		if _, err := LoadConfig(t.TempDir()); err == nil {
			t.Fatal("LoadConfig on directory: want error, got nil")
		}
	})

	t.Run("invalid json errors", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(p, []byte("not-json"), 0o600); err != nil {
			t.Fatalf("write bad config: %v", err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Fatal("LoadConfig invalid json: want error, got nil")
		}
	})

	t.Run("valid file overrides defaults", func(t *testing.T) {
		want, err := json.Marshal(map[string]any{
			"embedderModel": "remote-x",
			"httpPort":      9999,
		})
		if err != nil {
			t.Fatalf("marshal override fixture: %v", err)
		}
		p := filepath.Join(t.TempDir(), "cfg.json")
		if err := os.WriteFile(p, want, 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := LoadConfig(p)
		if err != nil {
			t.Fatalf("LoadConfig valid file: %v", err)
		}
		if cfg.EmbedderModel != "remote-x" || cfg.HTTPPort != 9999 {
			t.Fatalf("LoadConfig valid file = %+v, want overrides applied", cfg)
		}
		if cfg.DatabaseURL == "" {
			t.Fatal("LoadConfig valid file: defaults not layered under overrides")
		}
	})
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("Validate: empty databaseUrl must fail closed")
	}
	if err := (Config{DatabaseURL: "postgres://localhost/ksquad"}).Validate(); err != nil {
		t.Fatalf("Validate valid config: %v", err)
	}
}

func TestEncodeVector(t *testing.T) {
	cases := []struct {
		name string
		v    []float32
		want string
	}{
		{"empty", nil, "[]"},
		{"single", []float32{0}, "[0]"},
		{"negative and fraction", []float32{1.5, -2.25}, "[1.5,-2.25]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeVector(tc.v); got != tc.want {
				t.Fatalf("encodeVector(%v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}
