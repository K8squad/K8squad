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
// Package capbrief renders the capability-aware briefing the context
// assembler injects into a run's system context (ISI-7215 C1, parent ISI-7075).
//
// Root cause it closes: a run's context never said WHAT the run can actually
// do on the board. The tool surface is wired control-plane-side and recorded
// on the Run as an immutable CapabilityManifest (ADR-044), but it reaches the
// runtime only as an MCP config — nothing in the system context enumerated it,
// so in the ISI-7075 repro the discussion agent (holding discussion_search +
// discussion_post + the always-on work_item_search) narrated a work log
// instead of acting: a capability gap the agent could not even see. This
// package derives the briefing FROM the recorded manifest itself — the same
// immutable audit truth the sandbox wiring follows — so the agent's
// self-model of its capabilities can never drift from the tools it actually
// holds: capability-aware by construction, never a hardcoded list.
//
// It mirrors pkg/teamroster (ISI-5245): a pure, deterministic renderer whose
// output rides the authoritative, must-include tier of the §8.5 envelope
// (kind "capabilities"), injected after the role directive and team roster.
package capbrief

import (
	"strings"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/memory"
	"github.com/K8squad/K8squad/pkg/capability"
)

// toolSummaries carries the one-line "what you can do with it" for the
// first-party built-in tools, keyed by the SAME exported names the memory
// service's MCP edge registers (internal/memory) — one source of truth, so a
// renamed tool cannot leave a stale briefing line behind. Tools from BYO MCP
// servers are listed by name only; their own tools/list describes them.
var toolSummaries = map[string]string{
	memory.WorkItemCreateToolName:   "create a sub-ticket under an item you hold in custody (optionally assign it in the same call)",
	memory.WorkItemUpdateToolName:   "edit the title/body/parent of an item you hold in custody",
	memory.WorkItemAssignToolName:   "hand an item you hold in custody to a teammate agent (the delegation verb)",
	memory.WorkItemSearchToolName:   "search this team's tickets by topic — find related/prior work before duplicating it",
	memory.DiscussionSearchToolName: "search this project's indexed discussion messages",
	memory.DiscussionPostToolName:   "open a thread or reply in this project's discussion room (your speaking verb here)",
}

// statusToolMarker is the name prefix a status-change tool (ISI-7219 B1, the
// acknowledged capability gap) would carry. While no wired tool matches it,
// the briefing states the gap honestly — lane changes stay a human/custody
// operation — so an agent asked to move a ticket says so in its reply instead
// of silently pretending it did. When B1 lands, the line retires itself.
const statusToolMarker = "work_item_status"

// Brief renders the capability briefing for run from its recorded
// CapabilityManifest. It is a pure function of the manifest — deterministic,
// byte-identical on resume (the manifest is immutable once recorded, ADR-044)
// — never reads the cluster, and returns "" for a run with no manifest or no
// endpoints (nothing wired, nothing to claim: unchanged behavior).
func Brief(run *api.Run) string {
	if run == nil || run.Status.CapabilityManifest == nil {
		return ""
	}
	eps := capability.EndpointsFromManifest(run.Status.CapabilityManifest)
	if len(eps) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Your capabilities — the tool surface wired to THIS run\n\n")
	b.WriteString("The control plane has attached exactly these MCP tool surfaces to this run. ")
	b.WriteString("This list is the truth about what you can do here: anything not on it is not yours to do. ")
	b.WriteString("When the work calls for one of these actions, CALL the tool — the tool call IS the deliverable. ")
	b.WriteString("Do not narrate what you would do, do not write the outcome to a file instead of acting, ")
	b.WriteString("and never report an action you did not perform.\n")

	hasStatusTool := false
	for _, ep := range eps {
		if len(ep.AllowTools) == 0 {
			// A server with no narrowed allow set: its tools arrive with its own
			// catalog at runtime — say so, never invent names.
			b.WriteString("\n- " + ep.Name + ": attached MCP server; its tool list arrives with its own catalog at runtime.")
			continue
		}
		parts := make([]string, 0, len(ep.AllowTools))
		for _, tool := range ep.AllowTools {
			if strings.HasPrefix(tool, statusToolMarker) {
				hasStatusTool = true
			}
			if sum, ok := toolSummaries[tool]; ok {
				parts = append(parts, tool+" — "+sum)
			} else {
				parts = append(parts, tool)
			}
		}
		b.WriteString("\n- " + ep.Name + ": " + strings.Join(parts, "; ") + ".")
	}

	if !hasStatusTool {
		b.WriteString("\n\nChanging a ticket's status/lane is NOT an agent capability in this run ")
		b.WriteString("(it stays a human/custody operation). If the task needs one, say so explicitly ")
		b.WriteString("in your reply instead of claiming it moved.")
	}
	return b.String()
}
