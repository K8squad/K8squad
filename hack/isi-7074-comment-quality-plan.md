# ISI-7074 — Agent comments are hard to understand — Plan

**Status:** PLAN (planning mode — no code in this issue)
**Owner on approval:** CTO (Alfred)
**Author:** BigBoss (CEO)
**Date:** 2026-11-05

## Problem (as reported by the board)

The comments an agent pushes to the board "feel more like a **log** of k8squad
internals than a real comment about the work done and the decisions made." The
board wants the Paperclip experience:

1. **During the run:** the agent's thinking process, tool calls, and reasoning
   stream live (in italic) as it constructs its answer — ephemeral, clearly
   marked as in-progress.
2. **When the work is done:** the agent drops **one clear, detailed, readable
   response** — a summary of what was done and what was decided — not the raw
   trace.

Reference (current bad behavior): a sympozium-todo-demo issue on k8squad-test
(`http://10.0.0.219/projects/sympozium-squad%2Fsympozium-todo-demo/issues/7589df53-168e-4f92-a687-be6306befc4d`).

## Root cause (grounded in code)

k8squad has **no separation between ephemeral reasoning and the durable final
comment.** They are literally the same rows.

- Every shim progress event becomes its own durable `coord.comment` row.
  `ProgressMirror.Event` renders one row per event via `mirrorBody`
  (`pkg/controller/rundrive/progressmirror.go:148-199, 316-383`):
  - `wire.EventMessage` (any agent text) → `"[run x][untrusted] <text>"`
  - `wire.EventTool`    (every tool call) → a tool-phase line
  - `wire.EventUsage`   → a token digest line; terminal `EventStatus` → a status line
  Each is appended through the sanctioned §6.1 path
  (`coord.AppendComment`, `progressmirror.go:135`). The thread is therefore the
  full narration + tool-call log, one row per event — exactly "a log."
- A **second** durable producer adds canned noise: at settle,
  `SettleTerminalLane` appends an engine-authored one-liner
  (`settleSummary`, `pkg/coord/settle.go:205-214`, e.g. *"Run succeeded — agent
  X: lane → done (engine settle)."*), and failed follows get a terminal
  failure-reason comment (`a2asettle.go:168-175`). These are status lines, not a
  summary of work/decisions — more log, no narrative.
- The live "thinking" echo is **the same bytes as the durable comment.**
  `publishThinking` emits the identical `(author, body)` triple onto the per-run
  SSE hub (`progressmirror.go:197, 219-237`), and the client dedups the live row
  against the durable row by `(author, body)`. So k8squad's "thinking" **is** its
  "comment" — there is no ephemeral tier.
- **The wire has no semantic marker for "this is the final answer" — but the raw
  material already exists upstream.** The event taxonomy is `EventStatus /
  EventMessage / EventTool / EventSkillLoad / EventArtifactRef / EventUsage /
  EventAuthRequired / EventRateLimited` (`pkg/a2a/types.go:68-78`;
  `MessagePayload` is just `{Role, Text, Trust}`). There is no
  `EventThinking`/`EventReasoning` and no "final message" flag. **Critically, the
  goose parser already receives distinct `"thinking"` JSON blocks from the
  runtime and then coalesces them into the same `EventMessage{Role:"agent"}` as
  the answer text** (`pkg/shim/runtimes/goose.go:422-432`; opencode does the
  same). So reasoning and deliverable arrive indistinguishable *by choice at the
  parser*, not because the runtime can't tell them apart — the tiering material
  is there to recover.
- **Narration is throttled with drops, not queues:** `progressMirrorMinInterval
  = 1s`; non-terminal events arriving inside the window are dropped
  (`progressmirror.go:62-67, 168-173`). Because the final assistant text is an
  `EventMessage` (non-terminal), **the deliverable itself can be dropped** if it
  lands within 1s of a prior row — a latent correctness bug the two-tier split
  also fixes. Full reasoning/prompt/response text is deferred to the
  observability spine ISI-4812 (`progressmirror.go:68-77`). Net: today's thread
  is simultaneously *too raw* (every tool call) and *unreliable* at the one row
  that matters (the answer).

**One-line root cause:** k8squad mirrors the raw run trace directly into the
durable comment thread; it never produces a distinct, clean final comment and
never marks the intermediate trace as ephemeral thinking.

## Relationship to ISI-6310 (important — avoid double-building)

ISI-6310 ("agent draft-streaming + false run error", plan
`hack/isi-6310-streaming-plan.md`, in_review) covers the **transport/reliability**
side of live streaming:
- WS-1 kill the false "run error" (client 5-min wall clock vs durable truth)
- WS-2 add Last-Event-ID replay to the per-project SSE hub
- WS-3 verify the gateway honors long-lived SSE
- WS-4 (feature, P2) "live draft/thinking streaming like paperclip"

**ISI-7074 is the content/semantics half of the same user-visible goal.** It owns
the *two-tier comment model* — ephemeral thinking vs. one clean durable comment —
and the *quality of the final comment body*. ISI-6310's WS-2/WS-3 are the
transport that makes the ephemeral tier actually arrive live; ISI-6310 WS-4 and
ISI-7074 should be executed together (shared seam in the shim + ProgressMirror +
console). **Recommendation below folds WS-4 into this plan so one team ships the
two-tier model once**, with ISI-6310 keeping only the reliability workstreams.
(Also overlaps ISI-4812 = full reasoning-text producer surface.)

## Target behavior (the two-tier model)

| Tier | Content | Lifetime | Rendering |
|------|---------|----------|-----------|
| **Thinking** | reasoning deltas, tool calls, narration | **ephemeral** — lives on the live stream; persisted (if at all) collapsed/secondary, not as top-level comments | italic, "working…", collapsible, clearly in-progress |
| **Comment** | one clean summary: what was done + decisions made + outcome | **durable** — the single top-level `coord.comment` the board reads later | normal prose, the thing a human skims |

## Proposed workstreams (for CTO decomposition on approval)

### WS-1 — Mark thinking vs deliverable on the wire (P1, foundational, small/medium)
Give the shim a way to say "this text is reasoning/narration" vs "this is the
final answer." The parser already sees the distinction — stop throwing it away.
- Stop coalescing `"thinking"` blocks into the answer `EventMessage`
  (`goose.go:422-432`, opencode equivalent): carry a `kind`/`role` so reasoning
  is tagged thinking, and tag the final assistant turn as the deliverable — via
  a flag on `MessagePayload` or a dedicated terminal "result" on the terminal
  `EventStatus`. Decide new-event-type vs flag with the CTO so all four runtimes
  (goose, claude, opencode, crewai) express it the same way.
- Acceptance: for every runtime, exactly one piece of text per run is delivered
  tagged as the deliverable; intermediate reasoning and tool calls are tagged
  thinking.

### WS-2 — Split ProgressMirror into two sinks (P1, medium)
Stop writing every event as a durable top-level comment.
- **Thinking path:** `EventMessage`(non-final) + `EventTool` + reasoning →
  publish to the live stream only (the `thinking` outbox event already exists,
  `progressmirror.go:201-237`). Do **not** `AppendComment` them as top-level
  rows (or write them to a separate, collapsed thinking store — decide with CTO;
  must respect the FR-B3 §6.1 no-chat contract that pins `AppendComment` as the
  sanctioned write path).
- **Comment path:** on the `final`-tagged text (WS-1), write **one** clean
  `coord.AppendComment` row — the durable comment the board reads.
- Relax/queue rather than drop on the thinking path so the live trace is rich
  (coordinate the 1/sec throttle, `progressmirror.go:168-173`, and the
  reasoning-text depth deferred to ISI-4812). The deliverable must **never** be
  subject to the flood-guard drop.
- Quiet the settle-time canned comments (`settleSummary`, `settle.go:214`) so a
  completed run doesn't append engine status one-liners on top of the clean
  comment — keep terminal *failure* reasons, drop the "run succeeded" noise (or
  fold it into run metadata, not the comment thread). Decide with CTO.
- Acceptance: after a run, the thread shows **one** readable agent comment, not
  N log rows plus an engine one-liner; the live view still showed the thinking
  trace while running.

### WS-3 — Produce a clean final summary when the runtime doesn't give one (P1/P2, medium)
Some runtimes end with a terse or trace-like final turn. Define the fallback:
- Decide the product bar with the board: (a) pass through the model's final
  assistant message verbatim as the comment, vs. (b) a lightweight
  **summarization step** ("summarize work done + decisions made") when the final
  turn is missing/low-quality. Option (b) costs one extra model call per run —
  flag the cost/latency tradeoff for board sign-off.
- Define a comment template (what was asked / what changed / decisions & why /
  verification / follow-ups) so comments are consistent across agents.
- Acceptance: the durable comment reads as a human-understandable summary of
  work + decisions for each of the four runtimes.

### WS-4 — Console: render the two tiers (P1, medium, FE)
- Live: stream thinking in italic / collapsible "working…" (the
  `thinkingByMessageId` / `useRunStream` seam already exists).
- On completion: collapse thinking, surface the one clean comment as the
  top-level thread entry; keep thinking expandable for the curious.
- Depends on ISI-6310 WS-2/WS-3 for the live tier to arrive reliably through the
  gateway and across reconnects.
- Acceptance: a user watching a live run sees italic thinking while it works,
  then a single clean comment; a user opening the issue later sees only the clean
  comment (with thinking available on expand).

## Sequencing & dependencies
- **WS-1 → WS-2 → WS-3** are the backend spine (wire marker, then split sinks,
  then summary quality). WS-4 (FE) can start against WS-1's contract.
- **Depends on ISI-6310** WS-2 (SSE replay) + WS-3 (gateway SSE) for the live
  thinking tier to be reliable. Recommend ISI-6310 WS-4 be **absorbed into this
  issue** so the two-tier model is built once.
- **Coordinates with ISI-4812** (full reasoning-text producer surface) — the
  thinking tier is the natural consumer of that text.
- **Contract guard:** WS-2 must not violate the FR-B3 §6.1 no-chat contract
  (`pkg/coord/frb3_no_chat_contract_test.go`) — `AppendComment` stays the only
  sanctioned durable comment write; the ephemeral thinking tier must route
  through the run SSE / a non-comment store, not a lateral comment INSERT.

## Verification environment
Live-verify on k8squad-test (deploy mechanics per runbook; gateway 10.0.0.219),
using the same sympozium-todo-demo surface the board cited. Smallest proof per WS
is in its Acceptance line; FE WS-4 needs no full-workspace build.

## Decisions needed from the board (surfaced in the approval confirmation)
1. **Absorb ISI-6310 WS-4 here?** (recommended) — build the two-tier model once.
2. **Final-comment source:** pass-through final assistant message vs. a dedicated
   summarization step (extra model call per run — cost/latency).
3. **Thinking persistence:** purely ephemeral (live only) vs. persisted in a
   collapsed/secondary store for audit/replay after the run ends.

---

## rev2 — Post-deploy re-test verdict (2026-11-07)

**Deployed** `c02065db` (WS-1+WS-2+WS-4) to k8squad-test (helm tag-only roll
rev18→rev19, image-tag-only diff). Board (Henrik) re-tested on a fresh run.

**Verdict:**
- ✅ **Live activity / thinking tier — accepted.** "much better with the user
  activities, i like it." WS-1/WS-2/WS-4 streaming behavior is good.
- ❌ **The durable final message is still "unclear, not human understandable."**
  Henrik's pasted example is a rambling, self-correcting, apologetic model turn.
  **This is decisive evidence that the board-default PASS-THROUGH (WS-3 option a)
  is insufficient** — the raw final assistant turn is not a clean summary.
- Observed on **both** the issue-comment path AND the discussion-thread path
  (thread `7b0436cf`). The discussion deliverable is also just the model's raw
  final message (written via the `discussion_post` MCP writer), so it has the
  same content problem.
- Henrik also wonders if **markdown rendering** garbles the message.

### WS-3 — ESCALATED (was pass-through; now structured summary) (P1)
Pass-through is rejected. The durable deliverable must be a **structured,
human-readable summary**, not the raw final turn. Levers (decide with CTO/board):
- **(a) Output contract** — instruct every agent to END its run with a **clear,
  natural summary of the action done, plus any decisions/approvals it is
  requesting from the board** (Paperclip-style). **Not a rigid per-response form**
  — Henrik (2026-11-08) explicitly rejected a fixed "what was asked / what
  changed / decisions / verification / follow-ups" template stamped on every
  reply. The bar is a short human summary + a clear ask, nothing more. Cheapest;
  no extra model call; relies on model compliance.
- **(b) Dedicated summarization pass** — a final model call that turns the run
  into the summary. Robust across runtimes; **costs one extra model call per
  run** (cost/latency) — **this is the board sign-off the original plan deferred;
  Henrik's re-test is the trigger to approve it.**
- Recommend **(a)+(b) together**: output-contract first, summarization pass as
  the fallback when the final turn is still trace-like.
- **Apply to BOTH deliverables:** `coord.AppendComment` (issues) AND
  `discussion.message` via `discussion_post` (discussions). The content-shaping
  (output contract and/or summary pass) must sit where it covers both paths —
  likely the shim/agent output layer, not just the coord mirror.
- Impl ticket: **ISI-7195** (under WS-3 ISI-7100) — scope updated from
  pass-through to this escalation.
- Acceptance: on a fresh run, the durable comment AND the discussion reply each
  read as a **concise human summary of the action done + a clear statement of any
  decision requested from the board** — Paperclip-style, not a stream-of-thought
  and not a rigid section form.

### WS-5 — Markdown rendering fidelity of agent messages (NEW, P2, FE)
Investigate whether rendering artifacts (`&#x20;` entities, code-fence spacing,
hard breaks) make even a good summary hard to read in the console.
- Audit `MessageItem.tsx` "settle-then-render" Markdown path and the issue-thread
  comment renderer against a known-good sample.
- Acceptance: a well-formed summary renders cleanly (no stray entities / broken
  fences) in both the issue thread and the discussion room.

### WS-6 — Multi-option board-decision requests, Paperclip-style, in the inbox (NEW, Henrik 2026-11-08)
When an agent needs a board decision, it must **always present multiple labeled
options (a / b / c / d …)** for the board to choose — never a bare yes/no or an
open-ended question. This is the Paperclip "request a message/decision from the
board" mechanism, and it must **surface as an item in the k8squad mailbox/inbox.**
- **The mailbox already exists** — `console/app/(app)/inbox/` (`page.tsx`,
  `lib/inbox/api.ts`, `app/api/inbox/{route,unseen,seen}`), with a **DecisionCard**
  (`console/test/inbox/DecisionCard.test.tsx`) and `components/tickets/
  TicketDecisions.tsx`. So this is **extend, not build from scratch**: scope the
  gap between what the inbox renders today and the multi-option decision card
  Henrik wants.
- **Agent behavior / output contract:** a decision request must carry ≥2 labeled
  options with enough context for the board to choose. Enforce in the agent
  prompt + the decision-request helper so it's consistent across runtimes.
- **Mechanism:** wire the agent decision-request to the existing interactions
  path (ask_user_questions / request_confirmation / suggest_tasks) so it renders
  as a DecisionCard in the inbox and the board can act from there.
- Coordinate tightly with **ISI-7075** (coordinator should *suggest* next
  steps) — same board-interaction surface; this is the "always give options"
  half, 7075 is the "proactively suggest" half.
- This also tightens **WS-3**: the "decision requested from the board" portion of
  the durable message must enumerate the options, not just state that a decision
  is needed.
- Acceptance: when an agent needs a call, the board sees a DecisionCard in the
  inbox with multiple concrete options and can pick one; the agent resumes on the
  choice.

### Routed OUT of this issue
- **Coordinator proactiveness** — Henrik: the CEO/coordinator "simply confirms
  the team is blocked without any suggestion… should suggest more things to the
  user." This is *agent behavior/coordination quality*, not comment rendering →
  routed to **ISI-7075** (agents working the board + context; "stronger
  context-first / suggest next steps" theme). Not scoped here.
