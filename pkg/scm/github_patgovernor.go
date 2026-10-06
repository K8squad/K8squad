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
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"sync"

	"golang.org/x/time/rate"
)

// ISI-5498 — the durable answer to GitHub's SECONDARY (abuse) rate limit on a
// SHARED PAT. Multiple Projects mirror under one owner's token (e.g. sympozium
// + bmad-squad both on sympozium-scm-pat), and the reposync reconciler builds a
// FRESH GitHubProvider on every pass — so any budget that must be shared across
// the projects on one PAT cannot live on the provider instance (it would reset
// every reconcile) and cannot be a per-Project timer (N independent timers
// collectively burst GitHub). The governor therefore lives in a process-global
// registry keyed by a non-reversible PAT fingerprint: a single token-bucket
// per PAT that paces EVERY outbound call, so no one Project can exhaust the
// secondary-limit budget for the others sharing the PAT.
//
// This is the central per-PAT pacing that supersedes the transitional per-poll
// jitter from PR#782 (ISI-5475): jitter only spread the requeue schedule to
// keep co-PAT Projects out of lockstep, whereas the governor caps the actual
// outbound rate regardless of how the polls line up. The async per-PAT
// scheduler (ISI-5482) draws from this same governor to pace its coalesced
// queue.

const (
	// defaultPATRequestsPerSecond is the sustained outbound call rate per PAT.
	// 5/s = 300/min sits well under GitHub's ~900 points/min REST ceiling and,
	// crucially, SERIALIZES the fan-out so concurrent bursts (the usual
	// secondary-limit trigger) are smoothed across the projects on the PAT.
	defaultPATRequestsPerSecond = 5.0
	// defaultPATBurst absorbs a single snapshot's fan-out (issues + PRs +
	// check-runs + branches + releases) without stalling the common case while
	// still capping the instantaneous burst a PAT can emit.
	defaultPATBurst = 10

	// envPATRate / envPATBurst let ops retune the governor without a rebuild.
	envPATRate  = "KSQUAD_SCM_PAT_RATE"
	envPATBurst = "KSQUAD_SCM_PAT_BURST"
)

// patKey is a non-reversible fingerprint of a PAT, used ONLY as the in-memory
// map key for the shared governor. It never names the token in a log, a metric,
// or on the wire; 8 bytes of SHA-256 is collision-safe for the handful of
// distinct PATs a deployment runs.
func patKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// patState is the shared-per-PAT state. Today it holds only the governor; the
// ETag conditional-request cache (ISI-5497) layers onto this same struct so
// both knobs share one registry entry keyed by the PAT fingerprint.
type patState struct {
	limiter *rate.Limiter
}

// patRegistry maps PAT fingerprints to their shared state. One instance
// (sharedPATRegistry) is process-global; a fresh one is cheap for tests.
type patRegistry struct {
	mu     sync.Mutex
	states map[string]*patState
	rps    rate.Limit
	burst  int
}

// newPATRegistry builds a registry with the given governor parameters.
func newPATRegistry(rps rate.Limit, burst int) *patRegistry {
	return &patRegistry{
		states: map[string]*patState{},
		rps:    rps,
		burst:  burst,
	}
}

// newPATRegistryFromEnv builds the process-global registry, honouring the
// KSQUAD_SCM_PAT_RATE / KSQUAD_SCM_PAT_BURST ops overrides.
func newPATRegistryFromEnv() *patRegistry {
	rps := rate.Limit(defaultPATRequestsPerSecond)
	if v := os.Getenv(envPATRate); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			rps = rate.Limit(f)
		}
	}
	burst := defaultPATBurst
	if v := os.Getenv(envPATBurst); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			burst = n
		}
	}
	return newPATRegistry(rps, burst)
}

// sharedPATRegistry is the process-global per-PAT governor. Every GitHubProvider
// resolves its state from here so projects on one PAT share one token bucket.
var sharedPATRegistry = newPATRegistryFromEnv()

// stateFor returns (creating on first use) the shared state for one PAT.
func (r *patRegistry) stateFor(token string) *patState {
	key := patKey(token)
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.states[key]; ok {
		return st
	}
	st := &patState{
		limiter: rate.NewLimiter(r.rps, r.burst),
	}
	r.states[key] = st
	return st
}

// patGovernorTransport is the innermost provider RoundTripper: it sits BELOW the
// oauth2 auth transport so it paces the already-signed request immediately
// before the real network call. Every call waits on the PAT's shared token
// bucket, so the projects on one PAT never burst past GitHub's secondary limit
// together.
type patGovernorTransport struct {
	base  http.RoundTripper
	state *patState
}

func (t *patGovernorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Wait honours the request context (the client timeout), so a wedged bucket
	// cancels with the pass instead of stalling a worker forever — never fires
	// the call before a token is available, never blocks past the deadline.
	if t.state != nil && t.state.limiter != nil {
		if err := t.state.limiter.Wait(req.Context()); err != nil {
			return nil, err
		}
	}
	return t.base.RoundTrip(req)
}
