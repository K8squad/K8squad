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
	"slices"
	"testing"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
)

// ISI-4224: the settle detector matches exactly the opencode stdout lines
// upstream cmd/run.ts (v1.18.27) emits for a finished agent step — and
// nothing else, so the quiet window is armed only by the per-step terminal
// marker and never by interim events, logs, or malformed lines.
func TestOpenCodeSettleLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`{"type":"step_finish","timestamp":1763000507000,"sessionID":"ses_f6f3ae7f","part":{"type":"step-finish"}}`, true},
		{`{"type":"step_finish","part":{}}`, true},
		{`{"type":"step_start","part":{"type":"step-start"}}`, false},
		{`{"type":"text","part":{"type":"text","text":"partial"}}`, false},
		{`{"type":"tool_use","part":{}}`, false},
		{`{"type":"error","error":{"name":"E"}}`, false},
		{`not json at all`, false},
		{``, false},
	} {
		if got := openCodeSettleLine(tc.line); got != tc.want {
			t.Errorf("openCodeSettleLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// ISI-4224: the opencode ExecSpec advertises SettleLine and suppresses the
// CLI's known post-completion linger sources (auto-update, file watcher —
// always; models.dev fetch — only on BYO runs, whose rendered provider block
// is self-contained) so the process exits on its own and the runner's
// force-settle stays the backstop, not the norm.
func TestOpenCodeCommandWiresSettleAndLingerGuards(t *testing.T) {
	rt, err := Get(apiv1alpha1.RuntimeTypeOpenCode)
	if err != nil {
		t.Fatalf("Get(opencode): %v", err)
	}

	spec, err := rt.Command(LaunchContext{
		Envelope: a2a.Envelope{SystemContext: "sys", Input: "do the thing"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if spec.SettleLine == nil {
		t.Fatal("opencode ExecSpec must advertise SettleLine (ISI-4224)")
	}
	if !spec.SettleLine(`{"type":"step_finish","part":{}}`) {
		t.Fatal("advertised SettleLine must match a step_finish line")
	}
	for _, want := range []string{
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_EXPERIMENTAL_DISABLE_FILEWATCHER=1",
	} {
		if !slices.Contains(spec.Env, want) {
			t.Errorf("env missing linger guard %s: %v", want, spec.Env)
		}
	}
	if slices.Contains(spec.Env, "OPENCODE_DISABLE_MODELS_FETCH=1") {
		t.Error("models.dev fetch must stay enabled on vendor runs (provider metadata)")
	}

	byo, err := rt.Command(LaunchContext{
		Envelope:   a2a.Envelope{SystemContext: "sys", Input: "do the thing"},
		ModelRoute: a2a.ModelRoute{Endpoint: "http://byo.example/v1", Model: "qwen3.8"},
	})
	if err != nil {
		t.Fatalf("Command(byo): %v", err)
	}
	if !slices.Contains(byo.Env, "OPENCODE_DISABLE_MODELS_FETCH=1") {
		t.Error("BYO runs must disable the models.dev fetch (self-contained provider block)")
	}
}

// ISI-7755: the opencode Command wires a Finalize hook alongside Parse so the
// final assistant text is promoted to MessageKindDeliverable after stdout EOF,
// in-band and before the engine's terminal backstop fires.
func TestOpenCodeCommandWiresFinalizeHook(t *testing.T) {
	rt, err := Get(apiv1alpha1.RuntimeTypeOpenCode)
	if err != nil {
		t.Fatalf("Get(opencode): %v", err)
	}
	spec, err := rt.Command(LaunchContext{
		Envelope: a2a.Envelope{SystemContext: "sys", Input: "summarize"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if spec.Parse == nil {
		t.Fatal("opencode ExecSpec must set Parse")
	}
	if spec.Finalize == nil {
		t.Fatal("opencode ExecSpec must set Finalize (ISI-7755)")
	}

	// Feed one text event and a step_finish through Parse; Finalize should
	// emit exactly one MessageKindDeliverable with that text.
	textLine := `{"type":"text","part":{"text":"here is my summary"}}`
	finishLine := `{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}`
	for _, line := range []string{textLine, finishLine} {
		spec.Parse(line)
	}
	final := spec.Finalize()
	if len(final) != 1 {
		t.Fatalf("Finalize() = %d events, want 1", len(final))
	}
	p := final[0]
	if p.Message == nil {
		t.Fatal("Finalize() event must be an EventMessage")
	}
	if p.Message.Kind != a2a.MessageKindDeliverable {
		t.Errorf("Finalize() message kind = %q, want %q", p.Message.Kind, a2a.MessageKindDeliverable)
	}
	if p.Message.Text != "here is my summary" {
		t.Errorf("Finalize() message text = %q, want %q", p.Message.Text, "here is my summary")
	}
}

// ISI-7755: Finalize returns nothing when no text was seen (e.g. a run that
// failed before any assistant text, or an error-only run tagged deliverable by
// the error path).
func TestOpenCodeFinalizeEmptyWhenNoText(t *testing.T) {
	rt, err := Get(apiv1alpha1.RuntimeTypeOpenCode)
	if err != nil {
		t.Fatalf("Get(opencode): %v", err)
	}
	spec, err := rt.Command(LaunchContext{
		Envelope: a2a.Envelope{SystemContext: "sys", Input: "task"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	// Finalize with no prior Parse calls — no text accumulated.
	if got := spec.Finalize(); len(got) != 0 {
		t.Errorf("Finalize() with no text = %d events, want 0", len(got))
	}
	// A genuine (non-throttle) error delivers a deliverable-tagged message via
	// the error path — Finalize must not duplicate it (no thinking text
	// accumulated, so finalize returns nothing).
	spec.Parse(`{"type":"error","error":{"name":"AuthError","data":{"message":"invalid api key"}}}`)
	if got := spec.Finalize(); len(got) != 0 {
		t.Errorf("Finalize() after error-only run = %d events, want 0", len(got))
	}
}
