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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// TestOpenCodePromptRidesStdin (ISI-4188 gap 5): the envelope prompt is fed
// to `opencode run` on stdin — never argv — so the CLI no longer exits with
// "You must provide a message or a command".
func TestOpenCodePromptRidesStdin(t *testing.T) {
	spec, err := Get("opencode")
	require.NoError(t, err)

	exec, err := spec.Command(LaunchContext{
		Envelope: a2a.Envelope{SystemContext: "you are sam", Input: "read /etc/hostname"},
		WorkDir:  "/w",
	})
	require.NoError(t, err)

	assert.Equal(t, "you are sam\n\nread /etc/hostname", exec.Stdin)
	for _, a := range exec.Args {
		assert.NotContains(t, a, "read /etc/hostname", "prompt must not ride argv")
	}
}

// TestEnvelopePromptHalves: either envelope half may be empty; both present
// join with a blank line (mirrors the codex wrapper's buildEnvelope).
func TestEnvelopePromptHalves(t *testing.T) {
	assert.Equal(t, "in", EnvelopePrompt(LaunchContext{Envelope: a2a.Envelope{Input: "in"}}))
	assert.Equal(t, "sys", EnvelopePrompt(LaunchContext{Envelope: a2a.Envelope{SystemContext: "sys"}}))
	assert.Equal(t, "sys\n\nin", EnvelopePrompt(LaunchContext{Envelope: a2a.Envelope{SystemContext: "sys", Input: "in"}}))
	assert.Equal(t, "", EnvelopePrompt(LaunchContext{}))
}

// TestOpenCodeConfigAllowsTools (ISI-4188 gap 8): the rendered opencode.json
// disables the interactive permission gate — an unattended sandbox Run must
// not auto-reject every tool call.
func TestOpenCodeConfigAllowsTools(t *testing.T) {
	spec, err := Get("opencode")
	require.NoError(t, err)
	exec, err := spec.Command(LaunchContext{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "llama3.1"},
		WorkDir:    "/w",
	})
	require.NoError(t, err)
	require.Len(t, exec.WorkDirFiles, 1)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(exec.WorkDirFiles[0].Content, &doc))
	perm, ok := doc["permission"].(map[string]any)
	require.True(t, ok, "permission block missing: %s", exec.WorkDirFiles[0].Content)
	assert.Equal(t, "allow", perm["*"])
}

// TestParseOpenCodeLineToolUse (ISI-4188 gap 7): a completed tool call maps
// to one phase="result" EventTool with OK=true and the raw args on the
// internal ToolArgs seam (hashed downstream by the engine funnel).
func TestParseOpenCodeLineToolUse(t *testing.T) {
	line := `{"type":"tool_use","timestamp":1,"sessionID":"s","part":{"type":"tool","tool":"read","callID":"c1","state":{"status":"completed","input":{"filePath":"/etc/hostname"},"output":"x","time":{"start":1,"end":2}}}}`
	out := parseOpenCodeLine(line)
	require.Len(t, out, 1)
	p := out[0]
	assert.Equal(t, a2a.EventTool, p.Kind)
	require.NotNil(t, p.Tool)
	assert.Equal(t, "read", p.Tool.Name)
	assert.Equal(t, "result", p.Tool.Phase)
	require.NotNil(t, p.Tool.OK)
	assert.True(t, *p.Tool.OK)
	assert.JSONEq(t, `{"filePath":"/etc/hostname"}`, p.ToolArgs)
}

func TestParseOpenCodeLineToolError(t *testing.T) {
	line := `{"type":"tool_use","part":{"tool":"shell","state":{"status":"error","input":{"cmd":"ls"}}}}`
	out := parseOpenCodeLine(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Tool)
	assert.Equal(t, "result", out[0].Tool.Phase)
	require.NotNil(t, out[0].Tool.OK)
	assert.False(t, *out[0].Tool.OK)
}

func TestParseOpenCodeLineToolInFlight(t *testing.T) {
	line := `{"type":"tool_use","part":{"tool":"read","state":{"status":"running","input":{}}}}`
	out := parseOpenCodeLine(line)
	require.Len(t, out, 1)
	assert.Equal(t, "start", out[0].Tool.Phase)
	assert.Nil(t, out[0].Tool.OK)
}

func TestParseOpenCodeLineText(t *testing.T) {
	line := `{"type":"text","part":{"type":"text","text":"hostname=abc123"}}`
	out := parseOpenCodeLine(line)
	require.Len(t, out, 1)
	assert.Equal(t, a2a.EventMessage, out[0].Kind)
	require.NotNil(t, out[0].Message)
	assert.Equal(t, "hostname=abc123", out[0].Message.Text)
	assert.Equal(t, "untrusted", out[0].Message.Trust)
}

func TestParseOpenCodeLineError(t *testing.T) {
	line := `{"type":"error","error":{"name":"UnknownError","data":{"message":"boom"}}}`
	out := parseOpenCodeLine(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Message)
	assert.Contains(t, out[0].Message.Text, "UnknownError")
	assert.Contains(t, out[0].Message.Text, "boom")
}

// TestParseOpenCodeLineDegrades: step bookkeeping is dropped, and a non-JSON
// line (CLI diagnostics interleave on stdout) degrades to a message — parse
// failure never kills a running task.
func TestParseOpenCodeLineDegrades(t *testing.T) {
	assert.Empty(t, parseOpenCodeLine(`{"type":"step_start","part":{"type":"step-start"}}`))
	assert.Empty(t, parseOpenCodeLine(`{"type":"step_finish","part":{"type":"step-finish"}}`))

	out := parseOpenCodeLine("not json at all")
	require.Len(t, out, 1)
	assert.Equal(t, a2a.EventMessage, out[0].Kind)
	assert.Equal(t, "not json at all", out[0].Message.Text)
}

// TestParseOpenCodeStepFinishUsage (ISI-4238): a step_finish carrying the
// token block maps onto one EventUsage — the raw material for llm.call
// spans, the run's token totals and the interaction views. Model
// attribution is provider/model; cost + duration ride when reported.
func TestParseOpenCodeStepFinishUsage(t *testing.T) {
	line := `{"type":"step_finish","part":{"type":"step-finish","providerID":"anthropic","modelID":"claude-sonnet-4","tokens":{"input":1200,"output":340,"reasoning":50,"cache":{"read":8000,"write":400}},"cost":{"input":0.0036,"output":0.0021,"total":0.0057},"duration":4200}}`
	out := parseOpenCodeLine(line)
	require.Len(t, out, 1)
	assert.Equal(t, a2a.EventUsage, out[0].Kind)
	require.NotNil(t, out[0].Usage)
	u := out[0].Usage
	assert.Equal(t, "anthropic/claude-sonnet-4", u.Model)
	assert.Equal(t, 1200, u.Input)
	assert.Equal(t, 340, u.Output)
	assert.Equal(t, 50, u.Reasoning)
	assert.Equal(t, 8000, u.CacheRead)
	assert.Equal(t, 400, u.CacheWrite)
	assert.InDelta(t, 0.0057, u.CostUSD, 1e-9)
	assert.Equal(t, int64(4200), u.DurationMS)
}

// TestParseOpenCodeStepFinishModelOnly: a step-finish part with tokens but
// no provider prefix keeps the bare modelID; a part with neither model nor
// provider still yields usage (the engine attributes the launch model).
func TestParseOpenCodeStepFinishModelOnly(t *testing.T) {
	line := `{"type":"step_finish","part":{"type":"step-finish","modelID":"qwen3:8b","tokens":{"input":10,"output":5}}}`
	out := parseOpenCodeLine(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Usage)
	assert.Equal(t, "qwen3:8b", out[0].Usage.Model)
	assert.Equal(t, int64(0), out[0].Usage.DurationMS)
}

// TestParseOpenCodeStepFinishWithoutTokensStaysDropped (ISI-4238): a bare
// step_finish (older wire, bookkeeping only) must NOT emit zeroed usage —
// that would poison the run's token totals.
func TestParseOpenCodeStepFinishWithoutTokensStaysDropped(t *testing.T) {
	assert.Empty(t, parseOpenCodeLine(`{"type":"step_finish","part":{"type":"step-finish","cost":{"total":1}}}`))
	assert.Empty(t, parseOpenCodeLine(`{"type":"step_finish"}`))
}
