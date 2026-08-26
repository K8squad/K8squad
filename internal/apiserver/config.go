package apiserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod"
)

// Config is the ksquad-apiserver host configuration. DatabaseURL is the shared Postgres DSN
// (store of record, §17.3/ADR-001) the discussion surface and read models query. SessionCookie
// overrides the forwarded session-cookie name to match a BFF running with a non-default
// KSQUAD_SESSION_COOKIE. The auth.* block is the Epic 15 identity seam's tunables (chart
// ConfigMap surface, 9.5; ISI-2920).
type Config struct {
	DatabaseURL   string `json:"databaseUrl"`
	HTTPPort      int    `json:"httpPort"`
	SessionCookie string `json:"sessionCookie"`

	// JWTSigningKey is the HS256 key for the internal JWT mint (15.1). Empty ⇒ the
	// host auto-generates one (sessions then die on pod restart — Helm 9.5 supplies
	// the durable Secret for production). Accepts raw or base64-encoded bytes.
	JWTSigningKey string `json:"jwtSigningKey"`
	// JWTTTLSeconds is the internal JWT lifetime (default 3600 — spec 15.1's 1h).
	JWTTTLSeconds int `json:"jwtTtlSeconds"`
	// SessionTTLSeconds is the edge session lifetime (default 86400 — 24h).
	SessionTTLSeconds int `json:"sessionTtlSeconds"`
	// LoginRateLimit / LoginRateWindowSeconds are the failed-login brake (default
	// 5 per 900s — spec 15.1's 5/15min/IP; limit <= 0 disables).
	LoginRateLimit         int `json:"loginRateLimit"`
	LoginRateWindowSeconds int `json:"loginRateWindowSeconds"`
	// SecureCookies sets the Secure attribute on issued session cookies (default
	// true — chart TLS; explicit false only for a local http dev run).
	SecureCookies bool `json:"secureCookies"`
	// Bootstrap admin (15.2): created ONLY when auth.user is empty, from chart
	// values / env; the password value should be cleared after first login.
	BootstrapAdminUsername string `json:"bootstrapAdminUsername"`
	BootstrapAdminPassword string `json:"bootstrapAdminPassword"`
	BootstrapAdminTeamID   string `json:"bootstrapAdminTeamId"`
	// OidcGroupMapping is the raw auth.oidc.groupMapping JSON (15.9 seam). It is
	// parsed and VALIDATED at startup (fail fast on bad config); consumption
	// arrives with the OIDC login leg, where claims come from a trusted token
	// exchange — never from a client request body (PR #90 review finding 3).
	OidcGroupMapping string `json:"oidcGroupMapping"`
	// TrustedProxies is the comma-separated IP/CIDR list of reverse proxies whose
	// X-Forwarded-For may be trusted for the login limiter's client IP (PR #90
	// review finding 1). Empty (default) trusts NO ONE: the socket address is
	// used and XFF is treated as attacker-controlled decoration.
	TrustedProxies string `json:"trustedProxies"`
	// AllowedOrigins is the comma-separated list of browser origins accepted on
	// cookie-authenticated state-changing requests (CSRF guard). Empty ⇒ strict
	// same-host matching (Origin's host must equal the request Host) — correct
	// for direct exposure; a public console behind a proxy must list its origin.
	AllowedOrigins string `json:"allowedOrigins"`
	// MaxHashConcurrency bounds simultaneous argon2id derivations (default 2 —
	// each holds 64 MiB; the chart's 256Mi apiserver limit fits 2 with headroom).
	// 0 means "use the package default"; negative disables the bound.
	MaxHashConcurrency int `json:"maxHashConcurrency"`

	// BuildReaderPodEnabled is the story 8.7f (ISI-2905) feature flag for the on-demand full-tree
	// RO reader pod. Default FALSE: the build browser serves snapshot-only (v1) and never launches a
	// pod — the story degrades by default and never blocks 8.7e. An operator opts in via the chart
	// ConfigMap (9.5) / env only once a full-tree read need is proven (design §4.2 ponytail).
	BuildReaderPodEnabled bool `json:"buildReaderPodEnabled"`
	// BuildReaderPodImage is the RO git-reader image used when BuildReaderPodEnabled is true. It is
	// required for the flag to have effect; empty with the flag on still degrades to snapshot-only.
	BuildReaderPodImage string `json:"buildReaderPodImage"`
}

// DefaultConfig returns the zero-config defaults; env/flags/file override these.
func DefaultConfig() Config {
	return Config{
		// #nosec G101 -- localhost dev/default DSN with the stock Postgres password, not a real credential; env/flags/file override it in any real deployment.
		DatabaseURL:            "postgres://postgres:password@localhost:5432/ksquad?sslmode=disable",
		HTTPPort:               8080,
		SessionCookie:          SessionCookieName,
		JWTTTLSeconds:          3600,
		SessionTTLSeconds:      86400,
		LoginRateLimit:         5,
		LoginRateWindowSeconds: 900,
		SecureCookies:          true,
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
	if c.JWTSigningKey != "" && decodedKeyLen(c.JWTSigningKey) < 32 {
		return fmt.Errorf("jwtSigningKey must decode to at least 32 bytes")
	}
	return nil
}

// decodeKeyBytes returns the signing key bytes from a raw or base64-encoded
// string, trying the unpadded standard alphabet first (what
// auth.GenerateSigningKey emits — 43 chars for 32 bytes), then padded standard.
// Both Validate and DecodeJWTKey use this ONE definition (PR #90 review: the two
// previously disagreed on what counts as base64).
func decodeKeyBytes(key string) []byte {
	if raw, err := base64.RawStdEncoding.DecodeString(key); err == nil && len(raw) >= 32 {
		return raw
	}
	if raw, err := base64.StdEncoding.DecodeString(key); err == nil && len(raw) >= 32 {
		return raw
	}
	return []byte(key)
}

// decodedKeyLen returns the byte length of a raw or base64-encoded key string.
func decodedKeyLen(key string) int { return len(decodeKeyBytes(key)) }

// DecodeJWTKey returns the signing key bytes from a raw or base64-encoded string.
func DecodeJWTKey(key string) []byte { return decodeKeyBytes(key) }

// ApplyEnvOverrides layers the KSQUAD_* env knobs over the file/flag config. Every
// value is optional; unset vars leave the config untouched.
func (c *Config) ApplyEnvOverrides() {
	if v := os.Getenv("KSQUAD_JWT_SIGNING_KEY"); v != "" {
		c.JWTSigningKey = v
	}
	if v := os.Getenv("KSQUAD_JWT_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.JWTTTLSeconds = n
		}
	}
	if v := os.Getenv("KSQUAD_SESSION_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.SessionTTLSeconds = n
		}
	}
	if v := os.Getenv("KSQUAD_LOGIN_RATE_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.LoginRateLimit = n
		}
	}
	if v := os.Getenv("KSQUAD_LOGIN_RATE_WINDOW_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.LoginRateWindowSeconds = n
		}
	}
	if v := os.Getenv("KSQUAD_SECURE_COOKIES"); v != "" {
		c.SecureCookies = v != "false" && v != "0"
	}
	if v := os.Getenv("KSQUAD_BOOTSTRAP_ADMIN_USERNAME"); v != "" {
		c.BootstrapAdminUsername = v
	}
	if v := os.Getenv("KSQUAD_BOOTSTRAP_ADMIN_PASSWORD"); v != "" {
		c.BootstrapAdminPassword = v
	}
	if v := os.Getenv("KSQUAD_BOOTSTRAP_ADMIN_TEAM_ID"); v != "" {
		c.BootstrapAdminTeamID = v
	}
	if v := os.Getenv("KSQUAD_OIDC_GROUP_MAPPING"); v != "" {
		c.OidcGroupMapping = v
	}
	if v := os.Getenv("KSQUAD_TRUSTED_PROXIES"); v != "" {
		c.TrustedProxies = v
	}
	if v := os.Getenv("KSQUAD_ALLOWED_ORIGINS"); v != "" {
		c.AllowedOrigins = v
	}
	if v := os.Getenv("KSQUAD_MAX_HASH_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxHashConcurrency = n
		}
	}
	// 8.7f build-reader-pod opt-in (default off; only takes effect with an image set).
	if v := os.Getenv("KSQUAD_BUILD_READER_POD_ENABLED"); v != "" {
		c.BuildReaderPodEnabled = v != "false" && v != "0"
	}
	if v := os.Getenv("KSQUAD_BUILD_READER_POD_IMAGE"); v != "" {
		c.BuildReaderPodImage = v
	}
}

// ReaderPodConfig projects the host config onto the 8.7f readerpod.Config the build-browser reader
// launcher consumes. It is the single mapping point so the flag surface (chart/env) and the launcher
// tunables cannot drift. With BuildReaderPodEnabled false (the default) the launcher degrades to
// snapshot-only regardless of the other fields.
func (c Config) ReaderPodConfig() readerpod.Config {
	return readerpod.Config{
		Enabled:     c.BuildReaderPodEnabled,
		ReaderImage: c.BuildReaderPodImage,
	}
}
