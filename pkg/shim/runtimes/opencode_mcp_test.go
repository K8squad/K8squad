package runtimes

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/K8squad/K8squad/pkg/capability"
)

// ISI-5334 (P2-6): opencode flattens an MCP-served call to `<server>_<tool>` on
// its --format=json wire and sets no server field, so the parser must recover
// ToolPayload.Server from the Run's declared MCP server keys. With Server set the
// telemetry mapper emits an mcp.call span (SpanKindClient) instead of bucketing
// the call as a local `system` gen_ai.tool.call.
func TestOpenCodeParserMCPToolServer(t *testing.T) {
	parse := newOpenCodeParser([]string{"ksquad-memory-discussion", "mempalace"})
	line := `{"type":"tool_use","part":{"tool":"ksquad-memory-discussion_post","state":{"status":"completed","input":{"body":"hi"}}}}`
	out := parse(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Tool)
	assert.Equal(t, "ksquad-memory-discussion", out[0].Tool.Server, "the server key opencode flattened into the tool name")
	assert.Equal(t, "post", out[0].Tool.Name, "the bare tool, mirroring the codex mcp_tool_call shape")
}

// A tool that names no declared MCP server is left as a local call (no Server).
func TestOpenCodeParserLocalToolUnchanged(t *testing.T) {
	parse := newOpenCodeParser([]string{"mempalace"})
	line := `{"type":"tool_use","part":{"tool":"read","state":{"status":"completed","input":{"filePath":"/etc/hostname"}}}}`
	out := parse(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Tool)
	assert.Empty(t, out[0].Tool.Server)
	assert.Equal(t, "read", out[0].Tool.Name)
}

// A Run with no MCP servers never rewrites a tool name, even one that happens to
// contain an underscore.
func TestOpenCodeParserNoServersNoop(t *testing.T) {
	parse := newOpenCodeParser(nil)
	line := `{"type":"tool_use","part":{"tool":"mempalace_search","state":{"status":"completed","input":{"query":"x"}}}}`
	out := parse(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Tool)
	assert.Empty(t, out[0].Tool.Server)
	assert.Equal(t, "mempalace_search", out[0].Tool.Name)
}

// mcpServerFor matches the LONGEST server key so overlapping prefixes resolve to
// the right server (ksquad-memory vs ksquad-memory-discussion).
func TestMCPServerForLongestMatch(t *testing.T) {
	servers := []string{"ksquad-memory", "ksquad-memory-discussion"}
	server, bare, ok := mcpServerFor("ksquad-memory-discussion_post", servers)
	require.True(t, ok)
	assert.Equal(t, "ksquad-memory-discussion", server)
	assert.Equal(t, "post", bare)

	server, bare, ok = mcpServerFor("ksquad-memory_get", servers)
	require.True(t, ok)
	assert.Equal(t, "ksquad-memory", server)
	assert.Equal(t, "get", bare)

	_, _, ok = mcpServerFor("read", servers)
	assert.False(t, ok, "a local tool matches no server")
}

// mcpServerNames pulls the server keys off the endpoint set and tolerates an
// empty/nameless endpoint list.
func TestMCPServerNames(t *testing.T) {
	assert.Nil(t, mcpServerNames(nil))
	got := mcpServerNames([]capability.Endpoint{
		{Name: "mempalace"},
		{Name: ""}, // defensive: skipped
		{Name: "ksquad-memory-discussion"},
	})
	assert.Equal(t, []string{"mempalace", "ksquad-memory-discussion"}, got)
}

// An in-flight (start phase) MCP call is enriched too, so the pending span is
// opened as an mcp.call with the right server from the first frame.
func TestOpenCodeParserMCPToolStartPhase(t *testing.T) {
	parse := newOpenCodeParser([]string{"mempalace"})
	line := `{"type":"tool_use","part":{"tool":"mempalace_search","state":{"status":"running","input":{"query":"x"}}}}`
	out := parse(line)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Tool)
	assert.Equal(t, "start", out[0].Tool.Phase)
	assert.Equal(t, "mempalace", out[0].Tool.Server)
	assert.Equal(t, "search", out[0].Tool.Name)
}
