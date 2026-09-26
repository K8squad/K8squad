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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

// TestFetchSpanCarriesHTTPSemantics (ISI-5013): a scm.fetch.<kind> span for a
// provider Snapshot carries the outbound provider call's HTTP client semantics
// — http.request.method, url.path, server.address and http.response.status_code
// — stamped by the provider HTTP transport. Previously the span carried zero
// http.* attributes, so RED analysis could not see method/route/status per
// outbound SCM call.
func TestFetchSpanCarriesHTTPSemantics(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/app/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	p, err := NewGitHubProvider(srv.URL, ProviderCredentials{})
	if err != nil {
		t.Fatalf("NewGitHubProvider: %v", err)
	}
	if _, err := p.Snapshot(context.Background(), "https://github.com/acme/app",
		SnapshotOptions{Types: []RecordType{RecordTypeIssue}}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	var attrs map[string]attribute.KeyValue
	for _, s := range sr.Ended() {
		if s.Name() != "scm.fetch.issues" {
			continue
		}
		attrs = map[string]attribute.KeyValue{}
		for _, a := range s.Attributes() {
			attrs[string(a.Key)] = a
		}
	}
	if attrs == nil {
		t.Fatal("no scm.fetch.issues span recorded")
	}

	method, methodOK := attrs["http.request.method"]
	if !methodOK || method.Value.AsString() != http.MethodGet {
		t.Errorf("http.request.method = %q, want %q", method.Value.AsString(), http.MethodGet)
	}
	path, pathOK := attrs["url.path"]
	if !pathOK || !strings.HasSuffix(path.Value.AsString(), "/repos/acme/app/issues") {
		t.Errorf("url.path = %q, want suffix %q", path.Value.AsString(), "/repos/acme/app/issues")
	}
	u, _ := url.Parse(srv.URL)
	host, hostOK := attrs["server.address"]
	if !hostOK || host.Value.AsString() != u.Hostname() {
		t.Errorf("server.address = %q, want %q", host.Value.AsString(), u.Hostname())
	}
	status, statusOK := attrs["http.response.status_code"]
	if !statusOK || status.Value.AsInt64() != http.StatusOK {
		t.Errorf("http.response.status_code = %d, want %d", status.Value.AsInt64(), http.StatusOK)
	}
}
