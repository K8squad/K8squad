# Scheduling Policy & Warm Pools

Standing agent capacity — warm sandboxes waiting for work — has to be *decided* by something: which
roles need standing agents, and how many. That decision is a **policy**, and the loop that enforces
it belongs to the mesh, not to any one policy.

## The scheduling seam

Beside `RuntimeAdapter` (mechanism) and `PlatformHost` (substrate), the mesh exposes a third
pluggable surface: the **SchedulingPolicy**. The split is deliberate:

- the **loop** is the mesh's — a generic scheduler runs one deterministic pass per tick over every
  pool the resolved policy set produces;
- the **decisions** are the policy's — target counts, floors, and envelopes arrive pre-resolved, so
  the scheduler never second-guesses a policy's math.

## RolePool target-N prescheduling (policy #1)

KSquad's built-in policy resolves `RolePool` CRs into pool plans: each `RolePool` declares a
target count (`targetN`) for a Role × AgentRuntime pair, and the policy turns that into a
**pool band** — a floor (coordinators and always-up agents are floored at their minimum) plus a
saturation ceiling — with the standing envelope pre-loaded. The generic scheduler then reconciles
live standing capacity toward those bands:

- one tick = one full pass, deterministic team/name order, stable fan-out;
- pools reconcile independently — one bad pool never blocks the others; its error is retried next
  tick;
- the standing read-back is bounded (a count, never a process list); a counter outage degrades to
  `StandingKnown=false` rather than failing the pass — floors still reconcile.

The floor rules a policy owns include the **coordinator floor**, the **always-up exemption**, and
**team-flag gating**.

## Why a seam and not a loop?

Because a second host should be able to express its own declarative source — a YAML roster, an
autoscaler signal — as another policy *without the loop changing*. RolePool is simply policy #1; the
loop it used to own now serves any policy.

## Related

- [AgentMesh architecture](./agent-mesh.md) — where this seam sits
- [Mid-run model fallback](./mid-run-model-fallback.md) — what happens when a warm run hits a rate limit
