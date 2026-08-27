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
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Epic D tool-usage read model (ISI-3288, plan §2.4 story D3): the console
// per-agent/per-run panel's data path.
// ============================================================================
//
// The canonical aggregate is the tool-usage metric set the operator exports on
// its controller-runtime /metrics endpoint (ksquad_tool_calls_total,
// ksquad_skill_loads_total, ksquad_mcp_call_duration_seconds). This file is the
// apiserver-side projection: scrape the operator's exposition and aggregate the
// three families into a per-agent JSON read model served behind the ONE §13
// authz choke point, exactly like the other 8.x read models. run.id is
// deliberately NOT a metric label (bounded cardinality, 13.6 discipline) — the
// per-RUN surface is the Run's OTel trace (spans carry ksquad.run.id), which
// the console links per Run; the metrics panel answers the per-AGENT question.

// ToolCallStat is one (tool, skill) call count for an agent.
type ToolCallStat struct {
	Tool  string `json:"tool"`
	Skill string `json:"skill,omitempty"`
	Calls uint64 `json:"calls"`
}

// SkillLoadStat is one skill's load count for an agent.
type SkillLoadStat struct {
	Skill string `json:"skill"`
	Loads uint64 `json:"loads"`
}

// MCPStat aggregates one MCP-served (server, tool) pair: call count and mean
// duration from the histogram's _count/_sum.
type MCPStat struct {
	Server     string  `json:"server"`
	Tool       string  `json:"tool"`
	Calls      uint64  `json:"calls"`
	AvgSeconds float64 `json:"avgSeconds"`
}

// ToolUsageAgent is one agent's tool-usage aggregate.
type ToolUsageAgent struct {
	Agent      string          `json:"agent"`
	ToolCalls  []ToolCallStat  `json:"toolCalls"`
	SkillLoads []SkillLoadStat `json:"skillLoads"`
	MCP        []MCPStat       `json:"mcp"`
}

// ToolUsageReport is the D3 read model's answer: the per-agent aggregates,
// the platform MCP table, and whether the operator's exposition actually
// carries the Epic D pipeline (the ksquad_tool_usage_pipeline_up marker the
// operator seeds at registration). Reporting=false on a successful scrape
// means the instrumentation pipeline is not exporting — the panel renders an
// explicit degraded state instead of a quiet "no activity yet" (review
// ISI-3348 finding 1: an OK-state must never mask a dead pipeline).
type ToolUsageReport struct {
	Agents    []ToolUsageAgent `json:"agents"`
	MCP       []MCPStat        `json:"mcp"`
	Reporting bool             `json:"reporting"`
}

// ToolUsageReader is the D3 read-model seam. Nil ⇒ GET /api/telemetry/tool-usage
// keeps its documented 501 (a dev run without the metrics URL wired), exactly
// like the other read models.
type ToolUsageReader interface {
	ToolUsage(ctx context.Context) (ToolUsageReport, error)
}

// operatorMetricsToolUsage scrapes the operator's Prometheus exposition and
// aggregates the ksquad_* tool-usage families per agent.
type operatorMetricsToolUsage struct {
	url    string
	client *http.Client
}

// DefaultOperatorMetricsURL is the in-cluster operator metrics scrape target
// (controller-runtime metrics registry, :8080). Overridable because the
// service wiring is chart-owned and not yet canonical.
const DefaultOperatorMetricsURL = "http://ksquad-operator-metrics.ksquad-system.svc.cluster.local:8080/metrics"

// NewOperatorMetricsToolUsage builds the scraper over the operator's metrics
// exposition URL (the D2-registered ksquad_* families).
func NewOperatorMetricsToolUsage(metricsURL string) ToolUsageReader {
	if metricsURL == "" {
		metricsURL = DefaultOperatorMetricsURL
	}
	return &operatorMetricsToolUsage{
		url: metricsURL,
		client: &http.Client{
			Timeout: 5 * time.Second, // a panel read must never hang the console
		},
	}
}

func (s *operatorMetricsToolUsage) ToolUsage(ctx context.Context) (ToolUsageReport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return ToolUsageReport{}, fmt.Errorf("tool-usage: build scrape request: %w", err)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return ToolUsageReport{}, fmt.Errorf("tool-usage: scrape operator metrics: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return ToolUsageReport{}, fmt.Errorf("tool-usage: operator metrics returned %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20)) // 8 MiB: a bounded exposition
	if err != nil {
		return ToolUsageReport{}, fmt.Errorf("tool-usage: read exposition: %w", err)
	}
	agents, mcp := AggregateToolUsageExposition(string(body))
	return ToolUsageReport{
		Agents:    agents,
		MCP:       mcp,
		Reporting: ExpositionReportsToolUsage(string(body)),
	}, nil
}

// pipelineMarker is the exposition sample name the operator's registered
// mapper seeds at startup (toolusage.Instruments.PipelineUp). Its presence in
// a scraped exposition proves the D2 pipeline is exported on that surface.
const pipelineMarker = "ksquad_tool_usage_pipeline_up"

// ExpositionReportsToolUsage reports whether a Prometheus exposition carries
// the Epic D pipeline-liveness marker. Pure — the unit-test seam.
func ExpositionReportsToolUsage(exposition string) bool {
	for _, line := range strings.Split(exposition, "\n") {
		line = strings.TrimSpace(line)
		// A sample line starts with the family name; HELP/TYPE comment
		// headers are skipped (comments alone would not prove a live
		// gauge child).
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, _, _, ok := parseSample(line); ok && name == pipelineMarker {
			return true
		}
	}
	return false
}

// labelSet is the parsed label block of one exposition sample.
type labelSet map[string]string

// AggregateToolUsageExposition folds a Prometheus text exposition into the
// per-agent tool-usage aggregate plus the platform-scoped MCP table (the
// duration histogram is {server,tool}-labeled — per-agent attribution is not
// in the metrics by design, 13.6 cardinality discipline). Pure — the
// unit-test seam. Unknown families and malformed lines are skipped, never
// fatal: telemetry is observational and one bad line must not blank the panel.
func AggregateToolUsageExposition(exposition string) ([]ToolUsageAgent, []MCPStat) {
	type agentAgg struct {
		tools  map[string]*ToolCallStat
		skills map[string]*SkillLoadStat
	}
	agents := map[string]*agentAgg{}
	mcp := map[string]*MCPStat{}
	mcpSums := map[string]float64{}
	agentOf := func(name string) *agentAgg {
		a, ok := agents[name]
		if !ok {
			a = &agentAgg{
				tools:  map[string]*ToolCallStat{},
				skills: map[string]*SkillLoadStat{},
			}
			agents[name] = a
		}
		return a
	}

	for _, line := range strings.Split(exposition, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := parseSample(line)
		if !ok {
			continue
		}
		switch {
		case name == "ksquad_tool_calls_total":
			a := agentOf(labels["agent"])
			key := labels["tool"] + "\x00" + labels["skill"]
			st, seen := a.tools[key]
			if !seen {
				st = &ToolCallStat{Tool: labels["tool"], Skill: labels["skill"]}
				a.tools[key] = st
			}
			st.Calls += uint64(value)
		case name == "ksquad_skill_loads_total":
			a := agentOf(labels["agent"])
			st, seen := a.skills[labels["skill"]]
			if !seen {
				st = &SkillLoadStat{Skill: labels["skill"]}
				a.skills[labels["skill"]] = st
			}
			st.Loads += uint64(value)
		case name == "ksquad_mcp_call_duration_seconds_count":
			key := labels["server"] + "\x00" + labels["tool"]
			st, seen := mcp[key]
			if !seen {
				st = &MCPStat{Server: labels["server"], Tool: labels["tool"]}
				mcp[key] = st
			}
			st.Calls += uint64(value)
		case name == "ksquad_mcp_call_duration_seconds_sum":
			mcpSums[labels["server"]+"\x00"+labels["tool"]] += value
		}
	}

	out := make([]ToolUsageAgent, 0, len(agents))
	for name, a := range agents {
		agg := ToolUsageAgent{Agent: name}
		for _, st := range a.tools {
			agg.ToolCalls = append(agg.ToolCalls, *st)
		}
		for _, st := range a.skills {
			agg.SkillLoads = append(agg.SkillLoads, *st)
		}
		sort.Slice(agg.ToolCalls, func(i, j int) bool { return agg.ToolCalls[i].Tool < agg.ToolCalls[j].Tool })
		sort.Slice(agg.SkillLoads, func(i, j int) bool { return agg.SkillLoads[i].Skill < agg.SkillLoads[j].Skill })
		if len(agg.ToolCalls) == 0 && len(agg.SkillLoads) == 0 {
			continue
		}
		out = append(out, agg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })

	mcpOut := make([]MCPStat, 0, len(mcp))
	for key, st := range mcp {
		if sum, ok := mcpSums[key]; ok && st.Calls > 0 {
			st.AvgSeconds = math.Round(sum/float64(st.Calls)*1e4) / 1e4
		}
		mcpOut = append(mcpOut, *st)
	}
	sort.Slice(mcpOut, func(i, j int) bool {
		return mcpOut[i].Server < mcpOut[j].Server || (mcpOut[i].Server == mcpOut[j].Server && mcpOut[i].Tool < mcpOut[j].Tool)
	})
	return out, mcpOut
}

// parseSample splits one exposition line into metric name, labels, and value.
func parseSample(line string) (string, labelSet, float64, bool) {
	valueStr := line
	labels := labelSet{}
	if i := strings.IndexByte(line, '{'); i >= 0 {
		end := strings.LastIndexByte(line, '}')
		if end < i {
			return "", nil, 0, false
		}
		var err error
		labels, err = parseLabels(line[i+1 : end])
		if err != nil {
			return "", nil, 0, false
		}
		valueStr = strings.TrimSpace(line[end+1:])
		line = line[:i]
	} else {
		// Unlabeled sample ("name value"): split the leading metric name —
		// a bare line is name+value, never name-only (a sample always
		// carries a value). Found by the pipeline marker, which exports
		// label-less (ISI-3348).
		fields := strings.Fields(valueStr)
		if len(fields) != 2 {
			return "", nil, 0, false
		}
		line, valueStr = fields[0], fields[1]
	}
	fields := strings.Fields(valueStr)
	if len(fields) == 0 {
		return "", nil, 0, false
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(value) {
		return "", nil, 0, false
	}
	return strings.TrimSpace(line), labels, value, true
}

// parseLabels parses `k="v",k2="v2"` — the exposition label block. Escaped
// quotes inside values use the standard backslash form.
func parseLabels(s string) (labelSet, error) {
	out := labelSet{}
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, fmt.Errorf("bad labels")
		}
		key := strings.TrimSpace(s[:eq])
		s = strings.TrimLeft(s[eq+1:], " ")
		if len(s) == 0 || s[0] != '"' {
			return nil, fmt.Errorf("bad labels")
		}
		var b strings.Builder
		i := 1
		for ; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				b.WriteByte(s[i+1])
				i++
				continue
			}
			if s[i] == '"' {
				break
			}
			b.WriteByte(s[i])
		}
		if i >= len(s) {
			return nil, fmt.Errorf("bad labels")
		}
		out[key] = b.String()
		s = strings.TrimLeft(s[i+1:], ", ")
	}
	return out, nil
}

// toolUsageHandler is GET /api/telemetry/tool-usage: the D3 read model behind
// the §13 choke point. `?agent=` scopes the agents array to one agent (the
// platform-scoped MCP table rides every response).
func toolUsageHandler(reader ToolUsageReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report, err := reader.ToolUsage(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "tool-usage read model unavailable: "+err.Error())
			return
		}
		if agent := r.URL.Query().Get("agent"); agent != "" {
			scoped := make([]ToolUsageAgent, 0, 1)
			for _, a := range report.Agents {
				if a.Agent == agent {
					scoped = append(scoped, a)
				}
			}
			report.Agents = scoped
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(report); err != nil {
			// Body already committed; nothing further to do.
			_ = err
		}
	}
}
