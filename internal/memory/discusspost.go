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

package memory

// discusspost.go — ISI-7367 (WS-7a of the ISI-7074 comment-quality plan): the
// discussion room is a publish surface the BOARD reads directly, and until now
// it was the one surface with NO content contract — the discussion_post tool
// description carried pure mechanics ("the thread's first message (on open) or
// the reply body"), so party-seat agents parked their full working style
// (commands, inline-code chips, dependency dumps) into the one durable message
// the board must read (live evidence: thread 7b0436cf, devon 6201 chars / 72
// inline-code spans; the WS-3 finalCommentContract shapes only the run's final
// coord comment, never the in-run reply). The constants below are the ONE
// source of truth for the tool's output-quality contract, shared by BOTH
// transports — the MCP catalog (toolmcp.go tools/list) and the HTTP shim's GET
// self-description (toolhttp.go) — so the two surfaces cannot drift. Same
// posture as the board-approved WS-3 decision and Henrik's no-rigid-form
// rejection (ISI-7195): prompt-side guidance only. Nothing here rewrites,
// truncates, or summarizes a posted body, and no extra model call exists.

// DiscussionPostToolName is NOT redeclared here: the exported constant in
// agentauthor.go (one source of truth shared with the operator provisioner
// and the capability gate, ADR-0024c D1) names this same tool.

// discussionPostBaseDescription is the mechanics half of the advertised
// description (unchanged by WS-7a; kept separate so the contract append below
// can never silently drop the open-vs-reply mechanics).
const discussionPostBaseDescription = "Post to a project's discussion room, scoped to the caller team. Omit thread_id to open a new thread (title required); pass thread_id to reply into it (optionally parent_message_id to nest). Author and tenancy are server-authenticated (never arguments). Returns the server-stamped thread (on open) or message (on reply) so you can cite what you wrote."

// discussionPostGuide is the board-readability output contract appended to the
// discussion_post tool description: the reply is read by the board in the
// room — lead with a natural summary + an explicit ask; inline code only for
// exact commands/paths the reader must act on; no tool narration or
// command+prose interleaving; park detail in work items / linked issues; a
// soft ~1200-char target, never a stamped template.
const discussionPostGuide = "The board reads your message directly in the room — write it for that reader: lead with a natural summary of where things stand, and when you need something, an explicit ask (what you need and from whom). Use inline code only for an exact command, path, or identifier the reader must run or copy — never for emphasis or narration. Do not interleave commands with prose, paste tool output, or narrate your steps; park that detail in work items or linked issues and reference them instead. Keep it readable at a glance — about 1200 characters is a soft target, not a limit."

// DiscussionPostToolDescription is the full advertised description on BOTH
// transports: mechanics + the WS-7a output contract.
const DiscussionPostToolDescription = discussionPostBaseDescription + " " + discussionPostGuide

// DiscussionPostBodyArgDescription shapes the body argument itself — the
// reply the board reads — with the same WS-7a guidance.
const DiscussionPostBodyArgDescription = "the thread's first message (on open) or the reply body — written for the board reader: natural summary + explicit ask; inline code only for exact commands/paths the reader must act on; no tool narration or command+prose interleaving; park detail in work items / linked issues; soft ~1200-char target"

// discussionPostManifest is the HTTP shim's self-description for GET
// /mcp/tools/discussion_post (ISI-7367): the SAME name, description, and body
// guidance the MCP transport advertises via tools/list — the parity surface
// that keeps the two transports from drifting ("MCP carries the output
// contract, HTTP silently doesn't").
func discussionPostManifest() map[string]any {
	return map[string]any{
		"name":        DiscussionPostToolName,
		"description": DiscussionPostToolDescription,
		"args": map[string]string{
			"project_id":        "the discussion room's project id (uuid); narrows within the caller team",
			"thread_id":         "omit to open a new thread; present (uuid) to reply into that thread",
			"title":             "thread title, required when opening a new thread (thread_id omitted)",
			"body":              DiscussionPostBodyArgDescription,
			"parent_message_id": "optional reply target (uuid) within the thread; ignored when opening",
		},
	}
}
