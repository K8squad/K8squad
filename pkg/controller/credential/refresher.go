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

package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrRefreshExpired is the sentinel the controller keys the terminal
// StateExpired path on: the provider rejected the refresh token itself
// (OAuth `invalid_grant`), which means the ~9-day refresh window has lapsed and
// only a human re-login can revive the credential (story 7.7 / 7.4). A
// TokenRefresher MUST return an error that wraps this (errors.Is) for that case
// and ONLY that case — a transient network/5xx error must NOT, so the
// controller retries those instead of falsely expiring a live credential.
var ErrRefreshExpired = errors.New("oauth refresh token rejected (invalid_grant); re-login required")

// RefreshedToken is the result of one successful refresh: the new access token,
// the (possibly rotated) refresh token, and the new expiry. The controller
// writes all three back to the SAME Secret. Providers that rotate the refresh
// token on every use (Anthropic does) return a new RefreshToken; a provider
// that does not returns the same value it was given — either way the controller
// persists whatever it gets, so a rotating provider never strands the Secret on
// a spent refresh token.
type RefreshedToken struct {
	// AccessToken is the new ~8h access token agent pods will mount.
	AccessToken string
	// RefreshToken is the refresh token to persist for the NEXT refresh.
	RefreshToken string
	// ExpiresAt is the new access-token expiry.
	ExpiresAt time.Time
}

// TokenRefresher exchanges a refresh token for a fresh access token. It is the
// one seam the controller depends on for provider I/O, so the reconcile logic
// is unit-testable with a fake and the concrete provider call (a real HTTP
// round-trip to the OAuth token endpoint) is isolated and swappable per
// provider — the vendor-neutral discipline the rest of Epic 7 already follows.
type TokenRefresher interface {
	// Refresh exchanges refreshToken for a new access token. It returns a value
	// wrapping ErrRefreshExpired iff the provider rejected the refresh token
	// (invalid_grant); any other error is treated as transient and retried.
	Refresh(ctx context.Context, refreshToken string) (RefreshedToken, error)
}

// tokenResponse is the RFC 6749 §5.1 successful-refresh JSON body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"` // seconds
	TokenType    string `json:"token_type"`
}

// errorResponse is the RFC 6749 §5.2 error JSON body; `error` == "invalid_grant"
// is the refresh-token-rejected signal.
type errorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// AnthropicRefresher performs the Claude Code OAuth-subscription refresh
// (story 7.2 / 7.7): a standard RFC 6749 `refresh_token` grant against the
// provider's OAuth token endpoint. The endpoint and client ID are FIELDS, not
// constants: arch §11.1 / ADR-032 call the exact refresh endpoint/lead-time
// "controller tuning, not a v1 gate", and pinning them as operator
// configuration keeps the controller vendor-neutral and lets the values move
// without a code change. DefaultAnthropicRefresher fills the known Claude Code
// OAuth defaults.
type AnthropicRefresher struct {
	// TokenURL is the OAuth token endpoint the refresh grant is POSTed to.
	TokenURL string
	// ClientID is the OAuth client the refresh grant is issued under. Empty =
	// omit the field (some deployments carry the client binding in the token).
	ClientID string
	// HTTPClient is the client used for the round-trip; nil falls back to a
	// bounded-timeout default so a hung endpoint cannot wedge the reconciler.
	HTTPClient *http.Client
}

// Default Claude Code OAuth values. These are operator-overridable defaults,
// not a hardcoded contract — see AnthropicRefresher.
const (
	defaultAnthropicTokenURL = "https://console.anthropic.com/v1/oauth/token"
	defaultAnthropicClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	defaultRefreshHTTPTimeout = 30 * time.Second
)

// NewDefaultAnthropicRefresher returns an AnthropicRefresher pre-filled with the
// known Claude Code OAuth token endpoint and client ID and a bounded-timeout
// HTTP client. Operators pin different values by constructing the struct
// directly (or by overriding via cmd/operator flags).
func NewDefaultAnthropicRefresher() *AnthropicRefresher {
	return &AnthropicRefresher{
		TokenURL:   defaultAnthropicTokenURL,
		ClientID:   defaultAnthropicClientID,
		HTTPClient: &http.Client{Timeout: defaultRefreshHTTPTimeout},
	}
}

// Refresh implements TokenRefresher against an RFC 6749 token endpoint.
func (a *AnthropicRefresher) Refresh(ctx context.Context, refreshToken string) (RefreshedToken, error) {
	if a.TokenURL == "" {
		return RefreshedToken{}, fmt.Errorf("AnthropicRefresher.TokenURL is empty")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if a.ClientID != "" {
		form.Set("client_id", a.ClientID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshedToken{}, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultRefreshHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transport failure is transient — the controller retries, never
		// expires the credential on a blip.
		return RefreshedToken{}, fmt.Errorf("oauth refresh transport: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return RefreshedToken{}, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		var tr tokenResponse
		if err := json.Unmarshal(body, &tr); err != nil {
			return RefreshedToken{}, fmt.Errorf("decode refresh response: %w", err)
		}
		if tr.AccessToken == "" {
			return RefreshedToken{}, fmt.Errorf("oauth refresh returned empty access_token")
		}
		newRefresh := tr.RefreshToken
		if newRefresh == "" {
			// Non-rotating provider: keep the token we still hold.
			newRefresh = refreshToken
		}
		if tr.ExpiresIn <= 0 {
			return RefreshedToken{}, fmt.Errorf("oauth refresh returned non-positive expires_in %d", tr.ExpiresIn)
		}
		return RefreshedToken{
			AccessToken:  tr.AccessToken,
			RefreshToken: newRefresh,
			// Expiry is computed relative to the caller's clock at return; the
			// controller stamps it from expires_in so a skewed provider clock
			// does not distort the local refresh schedule.
			ExpiresAt: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		}, nil
	}

	// Distinguish a rejected refresh token (terminal → re-login) from a
	// transient provider error (retry). Only invalid_grant is terminal.
	var er errorResponse
	_ = json.Unmarshal(body, &er)
	if er.Error == "invalid_grant" || resp.StatusCode == http.StatusBadRequest && er.Error == "" && strings.Contains(strings.ToLower(string(body)), "invalid_grant") {
		return RefreshedToken{}, fmt.Errorf("%w: %s", ErrRefreshExpired, strings.TrimSpace(er.ErrorDescription))
	}
	return RefreshedToken{}, fmt.Errorf("oauth refresh failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
