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

// workItemSourceDiscussion is the coord.work_item.source origin marker
// stamped at mint time by BOTH room dispatch seams — markSourceDiscussion
// (internal/mentiondispatch, @-mention runs) and the party-advance
// facilitator mint (cmd/operator/party_advancer_adapter.go). buildTask reads
// it to select the discussion-shaped contract variant (ISI-7367).
const workItemSourceDiscussion = "discussion"

// discussionReplyContract (ISI-7367, WS-7a of the ISI-7074 comment-quality
// plan) is the discussion-room variant of the same contract family: a run
// whose work item was minted from the room (source='discussion') publishes
// its board-facing output DURING the run, as the discussion_post reply the
// board reads in the room. The final-comment contract still rides those
// envelopes verbatim, but the in-room reply had NO shaping until now — the
// gap the ISI-7263 spike grounded on live thread 7b0436cf (devon: 6201
// chars / 72 inline-code spans of commands and dependency dumps in one
// durable message). Same board-approved posture: prompt-side guidance only —
// nothing rewrites, truncates, or summarizes the posted body, no extra model
// call, and no rigid stamped form (Henrik's ISI-7195 rejection applies
// identically).
const discussionReplyContract = `How to post in the discussion room — reply contract:

Your discussion_post replies are read by the board directly in the room, while you are still working. Write each one for that reader, not for your own log.

Lead with a natural summary of where things stand and what it means; when you need a decision, help, or an unblock, state that ask explicitly — what you need and from whom. Inline code is only for an exact command, path, or identifier the reader must run or copy, never for emphasis and never interleaved with prose. Do not paste tool output or narrate commands step by step; park that detail in work items or linked issues and reference them instead. Keep each message short enough to read at a glance — about 1200 characters is a soft target, not a limit. Do not impose fixed headings or a stamped template; write it the way you would brief a colleague in chat.`

// appendDiscussionReplyContract joins the discussion-room reply contract on
// LAST (after the final-comment contract — see appendDispatchContracts): the
// same join discipline as appendFinalCommentContract — the contract is the
// closing instruction the model reads.
func appendDiscussionReplyContract(sysCtx string) string {
	sysCtx = strings.TrimSpace(sysCtx)
	contract := strings.TrimSpace(discussionReplyContract)
	if sysCtx == "" {
		return contract
	}
	return sysCtx + "\n\n" + contract
}

// appendDispatchContracts is the origin-aware buildTask seam (ISI-7367,
// WS-7a): every dispatched envelope still closes with the final-comment
// contract; envelopes minted from the discussion room (source='discussion')
// additionally close with the discussion-room reply contract, because the
// in-run discussion_post reply — not the final coord comment — is the durable
// message the board reads there. Pass-through is preserved end to end:
// neither contract rewrites anything, and no extra model call exists.
func appendDispatchContracts(sysCtx string, fromDiscussion bool) string {
	sysCtx = appendFinalCommentContract(sysCtx)
	if fromDiscussion {
		sysCtx = appendDiscussionReplyContract(sysCtx)
	}
	return sysCtx
}
