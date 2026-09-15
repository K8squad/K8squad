# The Phase Lifecycle

KSquad models each work item's journey as a **phase lifecycle**: a board whose
lanes are the stages a ticket moves through, from intake to a terminal state.
This guide explains the phases, how to bind roles to phases, how the coordinator
drives a ticket through them, how to enable or skip phases per project, how the
lifecycle projects onto your source-control provider, and the observability it
emits.

> This is a user/method guide. It describes how to *use* the lifecycle, not its
> internal design.

> **Rollout status.** The ten-phase board, the phase → issue-state projection,
> and the time-in-phase observability are live. Role↔phase binding
> (`activePhases`) and coordinator-driven auto-advance are rolling out; until
> your cluster has them, tickets move through the phases under human control and
> the projection/observability above still apply.

## The ten phases

A work item's `state` is one of ten lifecycle values, in canonical order:

| Phase | Kind | Meaning |
|---|---|---|
| `backlog` | intake | Captured, not yet ready to work. |
| `todo` | intake | Ready to enter the pipeline. |
| `design` | working | Shaping the solution. |
| `planning` | working | Breaking the design into work. |
| `implementation` | working | Building it. |
| `code_review` | working | Reviewing the change. |
| `testing` | working | Validating it. |
| `documentation` | working | Documenting it. |
| `done` | terminal | Completed successfully. |
| `cancelled` | terminal | Closed, not planned. |

The six middle values (`design` … `documentation`) are the **working phases** —
the stages a role can be assigned to. `backlog`/`todo` are intake lanes and
`done`/`cancelled` are terminal; no role "works" them.

> **Note on `in_progress` / `in_review`.** These two remain valid engine lanes
> used by the dispatch machinery, and the console folds them into the
> Implementation / Code-review columns for display. You do not author them as
> phases; treat the six working phases above as the authored surface.

## Binding a role to phases

By default a role is **phase-agnostic**: it is eligible to be dispatched in any
phase (this is the back-compatible default — teams that never opt in behave
exactly as before). To restrict a role to specific phases, set `activePhases`
on the `Role`:

```yaml
apiVersion: ksquad.io/v1alpha1
kind: Role
metadata:
  name: architect
spec:
  # ... existing fields (promptRef, model, defaultSkills) ...
  activePhases: [design, planning]   # eligible only in these phases
```

- `activePhases` values are the **six working phases only**. Naming an intake or
  terminal lane (e.g. `done`) is a configuration error and is rejected at
  admission.
- An **empty / omitted** `activePhases` means phase-agnostic (eligible
  everywhere).
- When a ticket sits on a working phase, the engine dispatches it to the first
  team member whose role is eligible for that phase. If no member is eligible,
  the ticket is left on its lane (an honest skip) rather than dispatched to a
  non-matching agent.

## The coordinator

A team can nominate **one** role as its **coordinator** — the single actor that
advances tickets across phases and dispatches the phase-appropriate role:

```yaml
apiVersion: ksquad.io/v1alpha1
kind: Role
metadata:
  name: manager
spec:
  coordinator: true
  coordinatorMode: auto        # or "propose"
  # a coordinator MAY also carry activePhases, but need not
```

- **At most one** coordinator role per team (enforced at team admission).
- A team **without** a coordinator behaves exactly as it does today: tickets are
  human-driven across the board; nothing auto-advances.

### Coordinator modes

| Mode | Behavior |
|---|---|
| `auto` (default) | On a phase agent's successful run, the coordinator advances the ticket to the next enabled phase and dispatches that phase's role — no human gate. |
| `propose` | Before each advance the coordinator raises a confirmation (an audit-logged proposal); it only advances once accepted. |

In **both** modes a human can move a card at any time; the coordinator
reconciles *to* the human-set lane and never fights a human move. When a ticket
reaches the end of the enabled phases, a successful `documentation` run moves it
to `done`; a kill/cancel moves it to `cancelled`.

## Enabling or skipping phases per project

The ten-value enum is global, but the *routed pipeline* is **per project**. A
project carries an ordered list of enabled phases; the coordinator walks that
list and simply never enters a disabled phase.

- **Default (no configuration):** all six working phases are enabled — the full
  pipeline.
- **Skip a phase:** remove it from the project's enabled list. Example: a docs
  team that runs `design → implementation → done` (skipping `planning`,
  `code_review`, `testing`, `documentation`) advances straight from
  implementation to done.

A disabled phase is never entered and never dispatched; the coordinator advances
to the next *enabled* phase in canonical order.

## How phases map to your source provider

When a project links its work items to GitHub (or another provider) issues, the
ten phases project onto the two states an issue can express:

| Phase | Issue state |
|---|---|
| `backlog`, `todo` | open |
| `design`, `planning`, `implementation`, `code_review`, `testing`, `documentation` | open (in progress) |
| `done` | closed — completed |
| `cancelled` | closed — not planned |

Only the two **terminal** phases close the upstream issue. On GitHub the close
carries a reason: `done` closes as *completed*, `cancelled` closes as *not
planned*. Every working phase keeps the issue open — moving a ticket between
working phases does not churn the linked issue. (Providers that cannot model a
close reason simply close the issue.)

## Observability: time in each phase

Every phase transition emits telemetry so you can answer "how long did this
ticket spend in each phase?":

- **Trace span** `coord.phase_transition` — one per transition, carrying the
  join key `ksquad.work_item.ref` plus `ksquad.phase.from`, `ksquad.phase.to`,
  `ksquad.phase.initiator` (human / agent / coordinator), and
  `ksquad.duration.ms` (time spent in the phase being left). Query it by
  `ksquad.work_item.ref` in your trace backend to reconstruct a ticket's full
  phase timeline. This correlates with the per-run traces so a phase's duration
  lines up with the runs that happened inside it.
- **Metrics** (bounded, aggregate — no per-ticket labels):
  - `ksquad_coord_phase_transitions_total{from,to,initiator}` — a counter over
    the transition graph.
  - `ksquad_coord_phase_duration_seconds{phase,initiator}` — a histogram of
    time-in-phase, keyed on the phase left.

Per-ticket detail lives on the span (and the audit log); the metrics stay
low-cardinality so dashboards over "average time in code_review" and similar
questions are cheap.

## Quick start

1. Author your working roles with `activePhases` for the phases each should own.
2. Nominate one role as `coordinator` (choose `auto` or `propose`).
3. Optionally set the project's enabled phases to skip stages you don't run.
4. Link the project to its issue tracker to get the phase → issue-state
   projection.
5. Watch time-in-phase in your observability backend via
   `coord.phase_transition` spans and the `ksquad_coord_phase_*` metrics.
