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

package runtimes

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// TestOpenCodeErrorEmitsRateLimitedSignal (ISI-6054, 5.10 producer half): a
// provider-throttle error frame (the zai 429 "Weekly/Monthly Limit Exhausted"
// observed live in the ISI-6043 canary) is sequenced as an EventRateLimited
// progress unit alongside the user-facing message and the llm.call usage
// error — so the core's bridge can act mid-Run (5.11 fallback / 2.11 pause).
func TestOpenCodeErrorEmitsRateLimitedSignal(t *testing.T) {
	line := `{"type":"error","error":{"name":"API Error","data":{"message":"429 Weekly/Monthly Limit Exhausted (org quota, reset later)"}}}`
	var sawSignal bool
	for _, p := range parseOpenCodeLine(line) {
		if p.Kind == a2a.EventRateLimited {
			sawSignal = true
			require.NotNil(t, p.RateLimited, "rate-limited progress must carry the payload struct")
		}
	}
	assert.True(t, sawSignal, "the zai 429 limit-exhausted frame must emit the EventRateLimited signal")
}

// TestOpenCodeOrdinaryErrorEmitsNoRateLimitedSignal: detection is narrow —
// ordinary provider/timeout errors must NOT park or switch a Run.
func TestOpenCodeOrdinaryErrorEmitsNoRateLimitedSignal(t *testing.T) {
	for _, line := range []string{
		`{"type":"error","error":{"name":"API Error","data":{"message":"connection refused"}}}`,
		`{"type":"error","error":{"name":"API Error","data":{"message":"context deadline exceeded (30s)"}}}`,
		`{"type":"error","error":{"name":"Model Error","data":{"message":"invalid model id"}}}`,
	} {
		for _, p := range parseOpenCodeLine(line) {
			assert.NotEqual(t, a2a.EventRateLimited, p.Kind, "ordinary error %s must not read as a throttle", line)
		}
	}
}

// TestOpenCodeCardAdvertisesRateLimitSignal: now that the adapter emits the
// standardized signal, the capability card says so — the mesh standing-process
// card derives from the same Capabilities(), so both surfaces flip together.
func TestOpenCodeCardAdvertisesRateLimitSignal(t *testing.T) {
	rt, err := Get("opencode")
	require.NoError(t, err)
	assert.True(t, rt.Capabilities().RateLimitSignal, "opencode card must advertise rateLimitSignal=true")
}

// TestIsRateLimitErrorSignatures pins the detector's accepted signatures.
func TestIsRateLimitErrorSignatures(t *testing.T) {
	for _, s := range []struct{ name, detail string }{
		{"API Error", "429 Too Many Requests"},
		{"API Error", "Rate limit reached for requests"},
		{"API Error", "Weekly/Monthly Limit Exhausted"},
		{"API Error", "quota exceeded for this organization"},
	} {
		assert.True(t, isRateLimitError(s.name, s.detail), "%s: %s", s.name, s.detail)
	}
	assert.False(t, isRateLimitError("API Error", "500 internal server error"))
}
