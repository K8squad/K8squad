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

// commentcontract.go — ISI-7100 (WS-3 of the ISI-7074 comment-quality plan):
// the final-comment contract. WS-1 tagged the deliverable on the wire
// (ISI-7098) and WS-2 made that message the run's single durable
// kind='summary' comment (ISI-7099) — but pass-through alone carries whatever
// the model happened to say, so comment shape drifts per runtime and per
// model. The board-approved default (plan decision #2) is pass-through with NO
// extra model call: quality is shaped at the prompt, upstream of the run, not
// by post-hoc summarization. The contract below rides EVERY dispatched
// envelope's system context — one source of truth in the dispatcher,
// delivered identically to every lane: stdin runtimes (goose/opencode via
// EnvelopePrompt), env-channel runtimes (codex/hermes/openclaw via the
// spilled context file), the runner default lane, and the mesh crewaiRouter
// (crewTaskText joins SystemContext+Input with the same semantics).
package rundrive

import "strings"

// finalCommentContract is appended to every dispatched envelope's system
// context. It tells the model, before any work begins, how its final message
// is published and the shape the board expects — the five sections of the
// approved template, so comments read consistently across agents and
// runtimes. It is guidance, never a wrapper: the deliverable still passes
// through VERBATIM as the comment body (WS-2's AppendSummary path); nothing
// here rewrites, truncates, or summarizes the text. A dedicated summarization
// step remains deliberately unbuilt (extra model call per run — gated on
// pass-through-quality evidence and board re-confirmation per the plan).
const finalCommentContract = `How to report — final answer contract:

Your FINAL message is published VERBATIM as the single durable comment on this work item; the board reads it as your report of the work. Intermediate reasoning and tool activity stream live while you work and are NOT repeated in the final message.

Write the final message as plain prose under these five headings, in this order (skip a heading only when it genuinely does not apply):

**What was asked** — restate the task in one or two sentences.
**What changed** — the concrete outcome: files, PRs, objects, behavior changes, with names/ids/links.
**Decisions & why** — the choices you made and the reasoning, including alternatives you rejected.
**Verification** — how you proved the result: tests run, checks green, live probes, with the evidence.
**Follow-ups** — remaining work, blockers, or next actions, each with an owner where known.

Do not paste tool logs, raw traces, or progress narration into the final message.`

// appendFinalCommentContract joins the final-comment contract onto the
// envelope's system context: empty context becomes the contract alone;
// an existing context keeps the contract LAST so it is the closing
// instruction the model reads before the work instruction.
func appendFinalCommentContract(sysCtx string) string {
	sysCtx = strings.TrimSpace(sysCtx)
	contract := strings.TrimSpace(finalCommentContract)
	if sysCtx == "" {
		return contract
	}
	return sysCtx + "\n\n" + contract
}
