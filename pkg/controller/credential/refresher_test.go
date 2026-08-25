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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAnthropicRefresher_Success_RotatesRefreshToken(t *testing.T) {
	var gotGrant, gotRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotRefresh = r.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":28800,"token_type":"bearer"}`))
	}))
	defer srv.Close()

	r := &AnthropicRefresher{TokenURL: srv.URL, ClientID: "cid", HTTPClient: srv.Client()}
	before := time.Now()
	got, err := r.Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotGrant != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", gotGrant)
	}
	if gotRefresh != "old-refresh" {
		t.Errorf("sent refresh_token = %q, want old-refresh", gotRefresh)
	}
	if got.AccessToken != "new-access" {
		t.Errorf("AccessToken = %q, want new-access", got.AccessToken)
	}
	if got.RefreshToken != "rotated-refresh" {
		t.Errorf("RefreshToken = %q, want rotated-refresh (provider rotation persisted)", got.RefreshToken)
	}
	// expires_in 28800s = 8h; expiry should be ~8h out.
	if d := got.ExpiresAt.Sub(before); d < 7*time.Hour || d > 9*time.Hour {
		t.Errorf("ExpiresAt %v not ~8h from now", got.ExpiresAt)
	}
}

func TestAnthropicRefresher_NonRotating_KeepsRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No refresh_token in the response → provider does not rotate.
		_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600}`))
	}))
	defer srv.Close()

	r := &AnthropicRefresher{TokenURL: srv.URL, HTTPClient: srv.Client()}
	got, err := r.Refresh(context.Background(), "keepme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RefreshToken != "keepme" {
		t.Errorf("RefreshToken = %q, want keepme (non-rotating provider must not strand the Secret)", got.RefreshToken)
	}
}

func TestAnthropicRefresher_InvalidGrant_IsExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token expired"}`))
	}))
	defer srv.Close()

	r := &AnthropicRefresher{TokenURL: srv.URL, HTTPClient: srv.Client()}
	_, err := r.Refresh(context.Background(), "dead")
	if !errors.Is(err, ErrRefreshExpired) {
		t.Fatalf("err = %v, want wrap of ErrRefreshExpired", err)
	}
}

func TestAnthropicRefresher_ServerError_IsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`upstream down`))
	}))
	defer srv.Close()

	r := &AnthropicRefresher{TokenURL: srv.URL, HTTPClient: srv.Client()}
	_, err := r.Refresh(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrRefreshExpired) {
		t.Fatalf("5xx must NOT be terminal-expired (it is transient); got %v", err)
	}
}

func TestAnthropicRefresher_EmptyAccessToken_IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"","expires_in":3600}`))
	}))
	defer srv.Close()
	r := &AnthropicRefresher{TokenURL: srv.URL, HTTPClient: srv.Client()}
	if _, err := r.Refresh(context.Background(), "x"); err == nil {
		t.Fatal("expected error on empty access_token")
	}
}

func TestNewDefaultAnthropicRefresher_Defaults(t *testing.T) {
	r := NewDefaultAnthropicRefresher()
	if r.TokenURL == "" || r.ClientID == "" || r.HTTPClient == nil {
		t.Fatalf("default refresher not fully populated: %+v", r)
	}
}

// fakeRefresher is the controller-test double.
type fakeRefresher struct {
	result RefreshedToken
	err    error
	calls  int
}

func (f *fakeRefresher) Refresh(_ context.Context, _ string) (RefreshedToken, error) {
	f.calls++
	return f.result, f.err
}
