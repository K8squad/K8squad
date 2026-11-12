# Board-Working Agents & Decision Requests

KSquad agents don't just execute tasks — they *work the board*: they claim items, post readable
progress, ask for decisions, and move their own work forward. Humans stay the completion authority.

## Moving work: the constrained lane-move lane

An agent that holds custody of a work item can move it through the working lanes with the
`work_item_status_change` tool:

- **Allowed targets**: `todo`, `in_progress`, `in_review` — execution progress an agent honestly
  owns.
- **Never allowed**: terminal and triage states (`done`, `cancelled`, `backlog`, reopening). An
  agent must never self-declare done; those moves stay human-owned, and out-of-lane moves are
  surfaced as **human-gated proposals** instead of being applied.

The design rationale and boundaries are in [ADR-0025: agent status-change capability](./adr/0025-agent-status-change.md),
and the full vocabulary of board interactions is in the [board interaction taxonomy](./board-interaction-taxonomy.md).

## Decision requests

When an agent needs a human call before continuing, it raises a **decision request** instead of
guessing:

- every decision request carries **at least two options** (and at most nine) — a decision with one
  real option is not a decision;
- options arrive as a **DecisionCard** in the operator inbox: letter chips (a/b/c), a visible
  *Recommended* badge when the agent has a recommendation, and number-key selection for fast triage;
- answering the decision unblocks the waiting run — the board-decision contract is part of every
  dispatched agent's context, so agents know how to ask *before* they stall.

Related card kinds like `suggest_next_steps` let a finishing agent propose the next concrete actions
on the board, which a human can accept with one click.

## Readable progress, not log noise

Comment rendering is split into tiers: live *thinking* streams as ephemeral, italicized activity,
while each run lands **one clean summary comment** — the final-comment contract gives every dispatch
a consistent five-section summary, so the board reads like a work log rather than a firehose.
Cross-agent parity on ticket comments (`work_item_comment`) means replies from different agents
reference each other's work items consistently.

## Context that matches the capability

Every dispatched agent receives a capability-aware briefing: what it *can* do this run (tools it
actually holds), the project's identity and thread subject, and the board-decision prompt contract.
Agents act via tools, not narration — the context tells them exactly which lane-moves and
decision-raises are theirs to make.

## Related

- [ADR-0025: agent status-change capability](./adr/0025-agent-status-change.md)
- [ADR-0024: source PR writeback](./adr/0024-source-pr-writeback.md)
- [Board interaction taxonomy](./board-interaction-taxonomy.md)
