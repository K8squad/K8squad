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

package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The D3 read-model ACs: the operator's ksquad_* exposition folds into the
// per-agent aggregate (tool calls by tool×skill, skill loads) plus the
// platform MCP table (calls + mean duration), malformed lines are skipped
// never fatal, and ?agent= scopes the payload.
func TestAggregateToolUsageExposition(t *testing.T) {
	exp := strings.Join([]string{
		"# HELP ksquad_tool_calls_total Tool calls mapped from A2A EventTool events",
		"# TYPE ksquad_tool_calls_total counter",
		`ksquad_tool_calls_total{agent="coder",skill="",tool="shell"} 12`,
		`ksquad_tool_calls_total{agent="coder",skill="git",tool="shell"} 3`,
		`ksquad_tool_calls_total{agent="reviewer",skill="",tool="ripgrep"} 7`,
		"# TYPE ksquad_skill_loads_total counter",
		`ksquad_skill_loads_total{agent="coder",skill="code-review"} 4`,
		`ksquad_skill_loads_total{agent="coder",skill="git"} 2`,
		"# TYPE ksquad_mcp_call_duration_seconds histogram",
		`ksquad_mcp_call_duration_seconds_bucket{server="k8s",tool="get",le="0.005"} 1`,
		`ksquad_mcp_call_duration_seconds_count{server="k8s",tool="get"} 4`,
		`ksquad_mcp_call_duration_seconds_sum{server="k8s",tool="get"} 1.0`,
		`this is not a valid sample`,
		`ksquad_tool_calls_total{agent="coder",tool="halfparsed" 9`, // missing closing brace: skipped
		`ksquad_tool_calls_total{agent="coder",skill="",tool="nan"} NaN`,
	}, "\n")

	agents, mcp := AggregateToolUsageExposition(exp)
	require.Len(t, agents, 2)

	coder := agents[0]
	require.Equal(t, "coder", coder.Agent)
	require.Len(t, coder.ToolCalls, 2) // shell+"" 12, shell+git 3 — NaN line skipped
	require.Equal(t, ToolCallStat{Tool: "shell", Calls: 12}, coder.ToolCalls[0])
	require.Equal(t, ToolCallStat{Tool: "shell", Skill: "git", Calls: 3}, coder.ToolCalls[1])
	require.Equal(t, []SkillLoadStat{{Skill: "code-review", Loads: 4}, {Skill: "git", Loads: 2}}, coder.SkillLoads)

	reviewer := agents[1]
	require.Equal(t, "reviewer", reviewer.Agent)
	require.Equal(t, []ToolCallStat{{Tool: "ripgrep", Calls: 7}}, reviewer.ToolCalls)

	require.Equal(t, []MCPStat{{Server: "k8s", Tool: "get", Calls: 4, AvgSeconds: 0.25}}, mcp)
}

// The end-to-end scrape path: a fake operator /metrics exposition served over
// HTTP folds through the reader unchanged, and a dead endpoint surfaces a
// scrape error (503 at the handler, not a silent empty panel).
func TestOperatorMetricsToolUsageScrape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(
			`ksquad_tool_calls_total{agent="a",skill="",tool="t"} 1` + "\n" +
				`ksquad_skill_loads_total{agent="a",skill="s"} 1` + "\n"))
	}))
	defer srv.Close()

	r := NewOperatorMetricsToolUsage(srv.URL)
	report, err := r.ToolUsage(context.Background())
	require.NoError(t, err)
	require.Empty(t, report.MCP)
	require.Len(t, report.Agents, 1)
	require.Equal(t, "a", report.Agents[0].Agent)
	// The test exposition carries no pipeline marker — a successful scrape
	// of a markerless exposition must report reporting=false (the degraded
	// signal, ISI-3348 finding 1).
	require.False(t, report.Reporting)
	require.True(t, ExpositionReportsToolUsage("ksquad_tool_usage_pipeline_up 1\n"))
	require.False(t, ExpositionReportsToolUsage("# HELP ksquad_tool_usage_pipeline_up commentary only\nksquad_other 1\n"))

	dead := NewOperatorMetricsToolUsage("http://127.0.0.1:1/metrics")
	_, err = dead.ToolUsage(context.Background())
	require.Error(t, err)
}

// The HTTP surface: JSON shape + ?agent= scoping + the degraded 503.
func TestToolUsageHandler(t *testing.T) {
	fake := &stubToolUsageReader{
		reporting: true,
		agents: []ToolUsageAgent{{
			Agent:      "coder",
			ToolCalls:  []ToolCallStat{{Tool: "shell", Calls: 5}},
			SkillLoads: []SkillLoadStat{{Skill: "git", Loads: 1}},
		}},
		mcp: []MCPStat{{Server: "k8s", Tool: "get", Calls: 2, AvgSeconds: 0.5}},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/telemetry/tool-usage", nil)
	toolUsageHandler(fake).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Agents    []ToolUsageAgent `json:"agents"`
		MCP       []MCPStat        `json:"mcp"`
		Reporting bool             `json:"reporting"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Agents, 1)
	require.Len(t, body.MCP, 1)
	require.True(t, body.Reporting)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/telemetry/tool-usage?agent=coder", nil)
	toolUsageHandler(fake).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Agents, 1)
	require.Equal(t, "coder", body.Agents[0].Agent)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/telemetry/tool-usage?agent=nobody", nil)
	toolUsageHandler(fake).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Empty(t, body.Agents)

	failing := &stubToolUsageReader{err: errToolUsageScrape}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/telemetry/tool-usage", nil)
	toolUsageHandler(failing).ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

type stubToolUsageReader struct {
	agents    []ToolUsageAgent
	mcp       []MCPStat
	reporting bool
	err       error
}

func (s *stubToolUsageReader) ToolUsage(context.Context) (ToolUsageReport, error) {
	return ToolUsageReport{Agents: s.agents, MCP: s.mcp, Reporting: s.reporting}, s.err
}

type errString string

func (e errString) Error() string { return string(e) }

const errToolUsageScrape errString = "scrape failed"
