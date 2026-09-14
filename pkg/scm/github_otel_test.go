/*
Copyright 2026 KSquad.

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

package scm

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

// TestLastRateRemainingDefault: before any API response the reporter says
// "unknown" (ok=false) so the reconciler never feeds a fabricated 0 into the
// headroom gauge.
func TestLastRateRemainingDefault(t *testing.T) {
	p, err := NewGitHubProvider("", ProviderCredentials{})
	if err != nil {
		t.Fatalf("NewGitHubProvider: %v", err)
	}
	if v, ok := p.LastRateRemaining(); ok {
		t.Errorf("LastRateRemaining before any call = (%d, true), want ok=false", v)
	}
}

// TestRateTrackingTransportCaptures: the transport records X-RateLimit-Remaining
// from any response, so a single wrap captures headroom for every fetcher and
// pagination page (GH-3), and a rate-limit exhaustion (0) is observed too.
func TestRateTrackingTransportCaptures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", r.URL.Query().Get("remaining"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var remaining atomic.Int64
	remaining.Store(-1)
	rt := &rateTrackingTransport{base: http.DefaultTransport, remaining: &remaining}
	client := &http.Client{Transport: rt}

	for _, want := range []int64{4999, 4998, 0} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"?remaining="+strconv.FormatInt(want, 10), nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		resp.Body.Close()
		if got := remaining.Load(); got != want {
			t.Errorf("captured remaining = %d, want %d", got, want)
		}
	}
}

// TestRateTrackingTransportIgnoresMissingHeader: a response without the header
// (some enterprise proxies strip it) leaves the prior value untouched rather
// than clobbering it to 0.
func TestRateTrackingTransportIgnoresMissingHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var remaining atomic.Int64
	remaining.Store(1234)
	rt := &rateTrackingTransport{base: http.DefaultTransport, remaining: &remaining}
	client := &http.Client{Transport: rt}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	resp.Body.Close()
	if got := remaining.Load(); got != 1234 {
		t.Errorf("remaining = %d after header-less response, want 1234 (untouched)", got)
	}
}
