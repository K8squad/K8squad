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
