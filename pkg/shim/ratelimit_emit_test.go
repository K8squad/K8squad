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

package shim

import (
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// TestEmitRateLimitedSignal is the story 5.10 producer check: a runner that
// reports a RateLimited Progress must surface it as an EventRateLimited SSE
// event carrying the RateLimitedPayload with the Retry-After preserved RAW (the
// core owns the single parse). If emitProgress's type switch loses the
// EventRateLimited case, the payload would collapse to an empty agent message
// (the default arm) and the signal would silently vanish — this fails then.
func TestEmitRateLimitedSignal(t *testing.T) {
	tk := &task{id: "run-rl", state: a2a.TaskWorking, stream: newTaskStream(), now: time.Now}

	tk.emitProgress(Progress{
		Kind:        a2a.EventRateLimited,
		RateLimited: &a2a.RateLimitedPayload{Provider: "anthropic", Model: "claude-x", RetryAfter: "120"},
	})

	tk.stream.mu.Lock()
	defer tk.stream.mu.Unlock()
	if len(tk.stream.log) != 1 {
		t.Fatalf("want exactly 1 emitted event, got %d", len(tk.stream.log))
	}
	ev := tk.stream.log[0]
	if ev.Type != a2a.EventRateLimited {
		t.Fatalf("want EventRateLimited, got %q (type switch dropped the case?)", ev.Type)
	}
	p, ok := ev.Payload.(a2a.RateLimitedPayload)
	if !ok {
		t.Fatalf("payload is %T, want a2a.RateLimitedPayload", ev.Payload)
	}
	if p.RetryAfter != "120" {
		t.Fatalf("Retry-After not carried raw: got %q, want %q", p.RetryAfter, "120")
	}
	if p.Provider != "anthropic" || p.Model != "claude-x" {
		t.Fatalf("provenance lost: %+v", p)
	}
}
