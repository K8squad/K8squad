# AgentMesh Architecture

The AgentMesh is the substrate that connects KSquad's control plane to the agent runtimes that do the
work. Every dispatched run — and every standing, always-on agent — is reachable through the same
three pluggable seams, so adding a runtime never touches the control plane.

## The three seams

The mesh (a vendored, generic library in `third_party/k8sagentmesh`) is built around three
pluggable surfaces:

| Seam | Owns | Examples |
|------|------|----------|
| **RuntimeAdapter** | *mechanism* — how work is expressed to one runtime flavor and how its events come back | opencode, codex, claude-code, crewai, goose adapters |
| **PlatformHost** | *substrate* — where standing processes live (pods, gateways) | the KSquad sandbox/gateway host |
| **SchedulingPolicy** | *decisions* — which standing pools exist and how big they are | `RolePool` target-N prescheduling (policy #1) — see [Scheduling policy & warm pools](./scheduling-policy.md) |

A new runtime registers an adapter and a shim engine; no control-plane change is needed. That is how
`vibe`, `goose`, and `crewai` all landed without touching the operator's reconcilers.

## One-shot vs. always-on runtimes

- **One-shot runtimes** (claude-code, codex, opencode, openclaw, hermes, vibe, goose) execute a single
  dispatched task per spawn: the run lands, the engine streams JSON events, the shim normalizes them
  into the coordination record, and the sandbox goes idle.
- **Always-on runtimes** (`crewai`) hold a standing process in the pool. The gateway's
  `crewaiRouter` lane routes work to the standing crew over A2A using a shared `AUTH_TOKEN`; the
  crew keeps long-lived state in its sandbox. Dispatch to a crew is a message, not a spawn.

## Hibernation and rebind-at-wake

Idle standing capacity is not free, so the mesh can hibernate an idle process and respawn it on wake.
The subtle part is the model: while a process slept, its envelope's model may have changed. Rather
than switching models mid-stream, a **rebind-at-wake** wake compares the envelope to what the process
was built with and respawns from the *current* envelope if they differ. Rebind wakes are reported in
mesh events (`Rebind=true`) — never applied mid-stream.

## Mid-run model fallback

Rate limits no longer end a run. When the model endpoint reports a rate-limit verdict, the dispatch
leg can continue the *same run* on a configured fallback model — see
[Mid-run model fallback](./mid-run-model-fallback.md).

## Where to go next

- [Supported agent runtimes](./supported-agents.md) — the full conformant type list
- [Scheduling policy & warm pools](./scheduling-policy.md) — how standing capacity is decided
- [Observability: distributed tracing](./observability/distributed-tracing.md) — how traces cross the mesh
