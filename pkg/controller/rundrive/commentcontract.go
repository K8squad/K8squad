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
// model. The board-approved default (plan decision #2, refined on ISI-7195
// 2026-11-08) is pass-through with NO extra model call: quality is shaped at
// the prompt as a natural-summary-plus-board-ask contract, upstream of the
// run, not by post-hoc summarization or a stamped template. The contract
// below rides EVERY dispatched
// envelope's system context — one source of truth in the dispatcher,
// delivered identically to every lane: stdin runtimes (goose/opencode via
// EnvelopePrompt), env-channel runtimes (codex/hermes/openclaw via the
// spilled context file), the runner default lane, and the mesh crewaiRouter
// (crewTaskText joins SystemContext+Input with the same semantics).
package rundrive

import "strings"

// finalCommentContract is appended to every dispatched envelope's system
// context. It tells the model, before any work begins, how its final message
// is published and the shape the board expects — a clear, natural summary of
// the work plus an explicit ask when a board decision is needed, Paperclip-
// style. Henrik rejected a rigid per-response form (ISI-7195, 2026-11-08): no
// stamped headings, no fixed section order. It is guidance, never a wrapper:
// the deliverable still passes through VERBATIM as the comment body (WS-2's
// AppendSummary path); nothing here rewrites, truncates, or summarizes the
// text. A dedicated summarization step remains deliberately unbuilt (extra
// model call per run — gated on output-contract-quality evidence and board
// re-confirmation per the plan).
const finalCommentContract = `How to report — final answer contract:

Your FINAL message is published VERBATIM as the single durable comment on this work item; the board reads it as your report of the work. Intermediate reasoning and tool activity stream live while you work and are NOT repeated in the final message.

End with a short, natural summary in plain prose — a few sentences a teammate could read and understand without opening the diff: what you did and what it means. When a claim needs a name or id to be verifiable (a PR, a file, a live check, a test result), include it. Keep it concise. Do not impose fixed headings or a stamped template — write it the way you would explain the outcome to a colleague.

If you need a decision, approval, or unblock from the board, state that ask explicitly and specifically as the close of the message: what you need and from whom. If you need nothing, do not invent an ask.

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
