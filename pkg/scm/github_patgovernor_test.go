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

package scm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// The governor serializes calls against the per-PAT token bucket: N calls
// through a burst-1 limiter take at least (N-1)*interval of real time.
func TestGovernorPacesRequests(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	const interval = 40 * time.Millisecond
	const n = 4
	state := &patState{limiter: rate.NewLimiter(rate.Every(interval), 1)}
	client := &http.Client{Transport: &patGovernorTransport{base: http.DefaultTransport, state: state}}

	start := time.Now()
	for i := 0; i < n; i++ {
		resp, err := client.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if elapsed, want := time.Since(start), time.Duration(n-1)*interval; elapsed < want {
		t.Fatalf("elapsed %v < %v — governor is not pacing the PAT", elapsed, want)
	}
}

// Calls on the SAME PAT draw from one bucket; a distinct PAT is unthrottled. A
// burst-1 bucket on PAT A forces (n-1)*interval across A's calls, while a
// parallel call on PAT B (its own full bucket) is immediate — proving the budget
// is shared per PAT, not global.
func TestGovernorBudgetIsPerPAT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newPATRegistry(rate.Every(50*time.Millisecond), 1)
	a := r.stateFor("tokA")
	b := r.stateFor("tokB")
	if a == b {
		t.Fatal("distinct PATs must not share a bucket")
	}

	// PAT B still has its full burst token → immediate.
	trB := &patGovernorTransport{base: http.DefaultTransport, state: b}
	reqB, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	start := time.Now()
	respB, err := trB.RoundTrip(reqB)
	if err != nil {
		t.Fatalf("PAT B call: %v", err)
	}
	respB.Body.Close()
	if time.Since(start) > 40*time.Millisecond {
		t.Fatalf("PAT B call was throttled by PAT A's bucket (took %v)", time.Since(start))
	}
}

// A canceled context aborts the governor wait instead of stalling the worker.
func TestGovernorHonoursContextCancel(t *testing.T) {
	// Exhausted burst + ~never-refill rate → Wait blocks until the ctx fires.
	state := &patState{limiter: rate.NewLimiter(rate.Every(time.Hour), 1)}
	state.limiter.Allow() // drain the single burst token
	tr := &patGovernorTransport{base: http.DefaultTransport, state: state}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid/", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("expected a context error from the governor wait, got nil")
	}
}

// A nil state (or nil limiter) is a safe passthrough — the transport never
// panics when the governor is absent.
func TestGovernorNilStateIsPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, st := range []*patState{nil, {limiter: nil}} {
		tr := &patGovernorTransport{base: http.DefaultTransport, state: st}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("passthrough call: %v", err)
		}
		resp.Body.Close()
	}
}

func TestPATKeyDeterministicAndOpaque(t *testing.T) {
	a := patKey("ghp_secret_token")
	if a != patKey("ghp_secret_token") {
		t.Fatal("patKey must be deterministic")
	}
	if a == patKey("ghp_other_token") {
		t.Fatal("distinct tokens must not collide")
	}
	if strings.Contains(a, "ghp_secret_token") || strings.Contains(a, "secret") {
		t.Fatalf("fingerprint %q leaked the token", a)
	}
	if len(a) != 16 {
		t.Fatalf("fingerprint len = %d, want 16 hex chars", len(a))
	}
}

func TestRegistryStateSharedPerPAT(t *testing.T) {
	r := newPATRegistry(rate.Inf, 1)
	s1 := r.stateFor("tokA")
	s2 := r.stateFor("tokA")
	s3 := r.stateFor("tokB")
	if s1 != s2 {
		t.Fatal("two providers on the same PAT must share one governor")
	}
	if s1 == s3 {
		t.Fatal("distinct PATs must get distinct state")
	}
}

func TestGovernorEnvOverride(t *testing.T) {
	t.Setenv(envPATRate, "2.5")
	t.Setenv(envPATBurst, "7")
	r := newPATRegistryFromEnv()
	if r.rps != rate.Limit(2.5) {
		t.Fatalf("rps = %v, want 2.5 from %s", r.rps, envPATRate)
	}
	if r.burst != 7 {
		t.Fatalf("burst = %d, want 7 from %s", r.burst, envPATBurst)
	}

	t.Setenv(envPATRate, "garbage")
	t.Setenv(envPATBurst, "-3")
	rd := newPATRegistryFromEnv()
	if rd.rps != rate.Limit(defaultPATRequestsPerSecond) || rd.burst != defaultPATBurst {
		t.Fatalf("invalid env should fall back to defaults, got rps=%v burst=%d", rd.rps, rd.burst)
	}
}
