# ADR-0025: Agent status-change capability — a constrained, human-gated lane-move tool

Date: 2026-11-09  |  Status: Accepted  |  Issue: ISI-7219 (epic ISI-7075, Theme 2 "B1 — the real gap")

## Context

The ISI-7075 repro surfaced a structural gap: an agent asked to work the board can create
sub-tickets, edit fields and dispatch — but **cannot change a ticket's lane**. `work_item_update`
deliberately never touches `state` (ADR-0024: "lane motion stays a custody op"); the only agent
lane-move path, `HumanStateStore.AgentTransitionState` (ISI-3601), is bound to the run-scoped
task-io token (own-item only) and invisible to an agent's tool surface. So an agent that finishes
or picks up work can only *narrate* progress; the honest line in the capability briefing
(ISI-7215 C1) had to say "changing a ticket's status/lane is NOT an agent capability in this run".

Humans stay the completion authority — an agent must never self-declare `done`. The design goal is
the narrowest capability that makes an agent's *own execution progress* board-real, while keeping
every terminal/triage/phase decision human-owned.

## Decision

Add a fourth verb to the ADR-0024 authoring lane: **`work_item_status_change`** on the
`ksquad-memory-authoring` MCP surface (same `work_item.author` capability gate, same
server-authenticated identity; no new grant slug).

**1. Constrained direct path (auto mode).** A new coord seam,
`WorkItemWriteStore.AgentChangeWorkItemStatus`, moves an item when the agent holds custody of the
item or an ancestor (ADR-0024 O-3 scope), under the same row locks as the move:

- Target ∈ **{todo, in_progress, in_review}** — the working/reporting lanes. An agent reports
  execution progress; it does not triage (`backlog`), complete (`done`/`cancelled`), reopen a
  terminal item, or manage the 10-phase human vocabulary. `blocked` remains a condition, not a
  lane (§8.6).
- Source ∉ {done, cancelled} — reopening is a human act.
- Optional `from_state` optimistic-concurrency guard + already-in-target conflict (409 semantics),
  mirroring `HumanStateStore.TransitionState`.
- §6.5 audit: `event_type='state_transition'` with `from_state`/`to_state`, the AGENT principal,
  the authoring `run_id`, `initiator="agent"`, `InitiatedByUserID` NULL — honest agent provenance,
  no fence (ADR-037). NFR-5 phase-span emission mirrors the human move.

**2. Human-gated path (propose mode).** A coordinator carrying `coordinator.propose` never moves
a lane directly: the tool raises an inert **`status_change`** discussion proposal (ticket id,
target lane, reason). A human confirms through the existing proposalconfirm shell, which fans out
into the **human** `TransitionState` seam as the confirming human. This is the only agent-reachable
path to `done`, `cancelled`, `backlog`, the phase lanes, or a reopen — and the proposal target is
validated against the board-state set at propose time (`coord.IsValidHumanState`), so a human never
confirms a doomed move.

**3. Positioning vs ISI-3601.** The task-io `AgentTransitionState` stays unchanged (run-scoped
token, own item). The MCP tool is the *capability-visible, custody-checked* counterpart: the MCP
session is not a task-io binding, so custody is enforced inside the write transaction rather than
assumed from the token.

**4. Self-retiring briefing.** `pkg/capbrief` enumerates the tool (name carries the
`work_item_status` marker prefix) and retires the "NOT an agent capability" gap line **per run**:
runs without the authoring surface (e.g. discussion runs) keep the honest gap line — correctly,
because lane changes are genuinely not their capability.

## Options considered

- *Widen `work_item_update` with a state field* — rejected: overloads field-edit semantics and
  buries a constrained, audited lane move inside a generic edit.
- *Reuse `AgentTransitionState` from the MCP edge* — rejected: it trusts the task-io binding for
  custody; an MCP caller would reach it without any custody check.
- *New capability slug (`work_item.status`)* — deferred: v1 rides `work_item.author`; the
  constraints are custody + the transition graph, not a wider grant. Splitting the grant is a
  config change (ADR-0024 O-1) if a role ever needs edit-without-lane-motion.
- *Full phase-graph enforcement for agents* — rejected for v1: the server-side human graph is
  deliberately generous (ISI-4455); the agent graph is the strict subset above.

## Consequences

- `AuthoringToolNames` (the compiled-in manifest the operator seeds into
  `status.observedTools` and the CRD `toolFilter.allow`) widens to four names — automatic for new
  and self-healed provisions; existing CRs converge on the next reconcile.
- `discussion.ProposalPayload` grows `targetState`/`reason` fields and a `status_change` action;
  `proposalconfirm` gains the state seam (nil ⇒ honest refusal, existing actions unchanged).
- The chaos lane pins the DB-backed properties: custody enforcement, constrained-target refusal,
  terminal-source refusal, CAS conflicts, and the agent-provenance audit row.
- Follow-ups owned elsewhere: console affordances for the proposal card's status_change variant;
  the coordinator auto-advance track may later consume the same seam for engine-driven phases.
