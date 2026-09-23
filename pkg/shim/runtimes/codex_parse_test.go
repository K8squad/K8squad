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

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
)

// ISI-4732: the codex adapter must wire a stdout parser so codex `exec --json`
// runs emit typed EventTool/EventUsage events (before this fix Parse was nil and
// every line fell through to the opaque line→message mapping — zero tool/llm
// spans for codex).
func TestCodexCommandSetsParser(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeCodex)
	spec, err := rt.Command(LaunchContext{WorkDir: "/work"})
	require.NoError(t, err)
	require.NotNil(t, spec.Parse, "codex ExecSpec must carry a stdout Parse func (ISI-4732)")
}

// A completed command_execution maps to one phase="result" EventTool named
// "bash" (categorized bash→ksquad.tool.type), OK from exit_code, with the raw
// command on the internal ToolArgs seam (hashed downstream by the engine).
func TestParseCodexCommandExecution(t *testing.T) {
	line := `{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"bash -lc \"ls -la\"","aggregated_output":"total 0","exit_code":0,"status":"completed"}}`
	out := parseCodexLine(line)
	require.Len(t, out, 1)
	p := out[0]
	assert.Equal(t, a2a.EventTool, p.Kind)
	require.NotNil(t, p.Tool)
	assert.Equal(t, "bash", p.Tool.Name)
	assert.Equal(t, "result", p.Tool.Phase)
	require.NotNil(t, p.Tool.OK)
	assert.True(t, *p.Tool.OK)
	assert.Empty(t, p.Tool.Command, "an unrecognized shell head (ls) leaves Command empty")
	assert.Contains(t, p.ToolArgs, "ls -la")
}

// ISI-4720 reuse: a bash-wrapped git call carries the recognized head token on
// ToolPayload.Command, so the telemetry spine categorizes the otherwise-opaque
// shell span by what it actually ran. The command line itself never travels.
func TestParseCodexCommandExecutionHeadToken(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"command_execution","command":"bash -lc \"git commit -m secret\"","exit_code":0,"status":"completed"}}`
	out := parseCodexLine(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Tool)
	assert.Equal(t, "git", out[0].Tool.Command, "the recognized executable head, never the argument line")
}

// A non-zero exit code settles the call as failed (OK=false).
func TestParseCodexCommandExecutionFailure(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"command_execution","command":"bash -lc \"kubectl get pods\"","exit_code":1,"status":"failed"}}`
	out := parseCodexLine(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Tool.OK)
	assert.False(t, *out[0].Tool.OK)
	assert.Equal(t, "kubectl", out[0].Tool.Command)
}

// An argv-array command form still yields a shell span with the head extracted
// (wire-drift tolerance: string vs array must never fail the line).
func TestParseCodexCommandExecutionArgvArray(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"command_execution","command":["bash","-lc","npm test"],"exit_code":0,"status":"completed"}}`
	out := parseCodexLine(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Tool)
	assert.Equal(t, "npm", out[0].Tool.Command)
}

// An MCP tool call carries the server, so the telemetry spine emits an mcp.call
// span (not gen_ai.tool.call). Arguments ride the internal ToolArgs seam.
func TestParseCodexMCPToolCall(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"github","tool":"list_issues","arguments":{"repo":"K8squad/K8squad"},"status":"completed"}}`
	out := parseCodexLine(line)
	require.Len(t, out, 1)
	p := out[0]
	assert.Equal(t, a2a.EventTool, p.Kind)
	require.NotNil(t, p.Tool)
	assert.Equal(t, "list_issues", p.Tool.Name)
	assert.Equal(t, "github", p.Tool.Server)
	assert.Equal(t, "result", p.Tool.Phase)
	require.NotNil(t, p.Tool.OK)
	assert.True(t, *p.Tool.OK)
	assert.JSONEq(t, `{"repo":"K8squad/K8squad"}`, p.ToolArgs)
}

// A web_search item becomes a tool span so the model's search activity is
// visible; the query rides ToolArgs.
func TestParseCodexWebSearch(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"web_search","query":"opentelemetry gen_ai semconv"}}`
	out := parseCodexLine(line)
	require.Len(t, out, 1)
	assert.Equal(t, "web_search", out[0].Tool.Name)
	assert.Equal(t, "opentelemetry gen_ai semconv", out[0].ToolArgs)
}

// An agent_message maps to an untrusted message event; reasoning/bookkeeping
// items are dropped.
func TestParseCodexAgentMessageAndBookkeeping(t *testing.T) {
	msg := parseCodexLine(`{"type":"item.completed","item":{"type":"agent_message","text":"Done."}}`)
	require.Len(t, msg, 1)
	assert.Equal(t, a2a.EventMessage, msg[0].Kind)
	assert.Equal(t, "Done.", msg[0].Message.Text)
	assert.Equal(t, "untrusted", msg[0].Message.Trust)

	assert.Empty(t, parseCodexLine(`{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}`))
	assert.Empty(t, parseCodexLine(`{"type":"thread.started","thread_id":"0199"}`))
	assert.Empty(t, parseCodexLine(`{"type":"turn.started"}`))
	assert.Empty(t, parseCodexLine(`{"type":"item.started","item":{"type":"command_execution","command":"bash -lc ls"}}`))
}

// turn.completed maps to an EventUsage: input/output/reasoning tokens plus the
// cached-input subset carried distinctly onto CacheRead. Model is empty (the
// engine backfills the launch model).
func TestParseCodexTurnCompletedUsage(t *testing.T) {
	line := `{"type":"turn.completed","usage":{"input_tokens":2606,"cached_input_tokens":2048,"output_tokens":54,"reasoning_output_tokens":12}}`
	out := parseCodexLine(line)
	require.Len(t, out, 1)
	assert.Equal(t, a2a.EventUsage, out[0].Kind)
	u := out[0].Usage
	require.NotNil(t, u)
	assert.Equal(t, "", u.Model, "engine backfills the launch model")
	assert.Equal(t, 2606, u.Input)
	assert.Equal(t, 54, u.Output)
	assert.Equal(t, 12, u.Reasoning)
	assert.Equal(t, 2048, u.CacheRead)
}

// A turn.completed with no usable token block stays dropped — never zeroed usage
// that would poison the run's token totals (ISI-4238 posture).
func TestParseCodexTurnCompletedWithoutTokensStaysDropped(t *testing.T) {
	assert.Empty(t, parseCodexLine(`{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0}}`))
	assert.Empty(t, parseCodexLine(`{"type":"turn.completed"}`))
}

// A top-level error / turn.failed surfaces as a message; parse never drops the
// signal.
func TestParseCodexError(t *testing.T) {
	out := parseCodexLine(`{"type":"error","message":"model overloaded"}`)
	require.Len(t, out, 1)
	assert.Contains(t, out[0].Message.Text, "model overloaded")

	failed := parseCodexLine(`{"type":"turn.failed","error":{"message":"boom"}}`)
	require.Len(t, failed, 1)
	assert.Contains(t, failed[0].Message.Text, "boom")
}

// A non-JSON line (codex diagnostics interleave on stdout) degrades to a
// message — parse failure must never kill a running task.
func TestParseCodexNonJSONDegrades(t *testing.T) {
	out := parseCodexLine("not json at all")
	require.Len(t, out, 1)
	assert.Equal(t, a2a.EventMessage, out[0].Kind)
	assert.Equal(t, "not json at all", out[0].Message.Text)
}
