# ADR-0007 — Pod-side-supervisor topology + Bind→pod task-io credential delivery

- **Status:** Accepted — *implementation delegated*
- **Date:** 2026-09-03
- **Author:** Winston (System Architect)
- **Issue:** ISI-3635 (unblocks ISI-3614; base seam ISI-3601 / PR #237, merged `286b840`)
- **Coordinates with:** `pkg/controller/rundrive/dispatch.go` (operator-spawned topology 1),
  `pkg/warmpool` (Boot/Bind), `pkg/coord` (`ProdEffects.BindSandbox`), `pkg/taskio`
  (`Minter`), `pkg/telemetry` (`Inject`). Supersedes the "own ADR" deferral markers in
  `dispatch.go` (topology comment §topology-decision, `shimCommand` header).
- **Design input:** DevOps note `docs/design/bind-path-taskio-token-delivery.md` (commit
  `ec4a5ca`). This ADR **overrides** that note's favor-**C** delivery recommendation; see
  Consequences §"Overriding the design note."
- **Scope:** decides (1) the warmpool/sandbox dispatch topology and (2) the Bind→pod
  channel that delivers the run-scoped task-io credential. NO new authz surface — the
  task-io token stays the ONE run-scoped credential, minted by `taskio.Minter`; the
  apiserver/`/api/task-io` handler stays the authorization choke point.

## Context

ISI-3601 (S2, PR #237, now on `main`) gave an agent subprocess a **run-scoped** credential
to call back to coordination over `/api/task-io`:

| Var | Meaning |
|---|---|
| `KSQUAD_COORD_URL` | address of the `/api/task-io` mount |
| `KSQUAD_COORD_TOKEN` | HS256 run-scoped token (`taskio.Minter`), binds `(RUN_ID, WORK_ITEM_ID, principal)`; issuer `ksquad-taskio` so a session JWT can't be replayed |
| `WORK_ITEM_ID`, `RUN_ID` | the bound work item / Run uid |
| `TRACEPARENT` / `TRACESTATE` | W3C carrier (`telemetry.Inject`) joining subprocess spans onto the Run trace |

**Topology 1 (operator-spawned, live v1)** already delivers this. `dispatch.go shimCommand`
mints per task and injects the set into the `shim run` child's **minimal env** — at dispatch
time `RUN_ID`/`WORK_ITEM_ID` are known, and no `os.Environ`/`DATABASE_URL` leaks (asserted in
test). That path is unchanged by this ADR.

**Topology 2 (warmpool/sandbox)** cannot mirror it via env:

- `warmpool.sandboxEnv` (`pkg/warmpool/kube.go`) renders the container env at **Boot**, which
  runs on the warm path *before* any Run is bound — there is no `RUN_ID`/`WORK_ITEM_ID` yet, so
  the run-scoped token does not exist to mint. A warm Boot rides a carrier-less context, so
  even the run traceparent is empty at Boot.
- **Container env (and pod `volumes`) are immutable after pod creation.** `Pool.Bind`
  (`pkg/warmpool/pool.go`) pops a Ready pod and does **zero cluster calls** on the warm path —
  it never touches the booted pod. So no existing path hands run-scoped material to the pod at
  Bind.

The credential (and the per-run traceparent) must therefore be delivered at **Bind**, over a
channel that is **not** container env — and that channel does not exist. Separately, nothing
yet *drives* the bound pod: `Pool.Bind`'s `sandbox_ref` is recorded by
`coord.ProdEffects.BindSandbox` into `coord.sandbox_bind (run_id, work_item_id, sandbox_ref,
bound_by)` but is **never consumed by a dispatcher**. `dispatch.go` explicitly defers the
"pod-side supervisor" topology (shim + runtime *inside* the pod, operator bridging the Task
envelope over the kube API) to "its own ADR." This is that ADR.

### The load-bearing fact: `sandbox_ref` == pod name, known at Boot *and* Bind

`Pool.Bind` returns the pool-assigned `sandbox_ref`, which **is** the pod name (`sandboxID`,
set at Boot — `pkg/warmpool/kube.go` names the pod `sandboxID`). So the pod knows its own
`sandbox_ref` at Boot (it is its own name), and the operator learns the same `sandbox_ref` at
Bind (return value of `Pool.Bind`, persisted in `coord.sandbox_bind`). **`sandbox_ref` is a
stable join key available on both sides of the immutability boundary.** That is what makes a
kube-native, mutation-free credential drop possible without any pod-identity attestation
service (see Decision D2 / rejected option B).

## Decision

Two coupled decisions. **D1** picks the topology; **D2** picks the credential channel and is
the part that unblocks ISI-3614. They are deliberately *decoupled at the transport layer*: the
credential is a **bootstrap** concern, not a per-task concern, so it does not ride the Task
envelope.

### D1 — Topology: pod-side supervisor, Task envelope bridged over the kube API

For topology 2 the **shim + runtime run inside the sandbox pod** under an in-pod **supervisor**
(the pod's PID 1 / entrypoint, exposing the existing `/health`,`/ready` on :8080). The operator
becomes a **dispatch bridge**, not a process parent: it reaches the bound pod and hands it the
`wire.Task`, reusing the **identical `a2a` wire contract** (`wire.Task` in, JSONL/SSE run-events
out) that `StdioTransport` speaks today — only the transport changes.

- **Transport:** a new `PodProxyTransport` (sibling of `StdioTransport`) that POSTs the Task and
  reads the SSE run-event stream over the **apiserver pod-proxy subresource**
  (`pods/proxy`, or a direct pod-IP dial where network policy allows). Chosen over SPDY
  `pods/exec` streaming because exec streams have no reconnection/at-most-once semantics and die
  on apiserver restarts; an HTTP+SSE endpoint on the supervisor reuses the same event framing the
  operator already consumes and degrades honestly (a failed POST is a retryable dispatch error,
  not a half-open pipe).
- **Consumer of `sandbox_ref`:** the dispatcher gains a warmpool branch that, for a bound Run,
  looks up `sandbox_ref` from `coord.sandbox_bind` and dials that pod via `PodProxyTransport`
  instead of spawning a local `shim`. `NewOperatorDispatcher` stays honest: no supervisor
  reachable → retryable dispatch error, never a silent no-op (mirrors the shim-absent degrade).
- **One pod = one Run** (§9.3 teardown-and-replace) is preserved; the supervisor may serve
  multiple *tasks/work-items* of that one Run over its lifetime.

D1's bridge is **larger, separable work** and is *not* on ISI-3614's critical path — because D2
does not depend on it.

### D2 — Credential channel: **(A) projected per-sandbox Secret volume**, decoupled from the envelope

At **Bind**, the operator mints the run-scoped token and writes it into a Kubernetes **Secret
named `<sandbox_ref>`**; the pod mounts that Secret (by its own name) as a **projected volume**
at a fixed path; the supervisor reads the token from the **path**, not env.

This is candidate **(A)** from the design note — chosen over the note's favored **(C)**. The
decisive criterion is the one the ticket names: *which channel lets ISI-3614 deliver working
task-io credentials to topology-2 pods with the least new, unbuilt machinery?*

- **(A)** needs only: mount an *optional* Secret at Boot + mint-and-write the Secret at Bind.
  Both are small and mechanical, and depend only on already-merged `taskio.Minter` +
  the `(run_id, work_item_id, sandbox_ref)` that `BindSandbox` already has. **A can ship before
  the D1 bridge exists.**
- **(C)** folds the token into the Task envelope — but the envelope bridge (D1) is the hardest,
  least-built part. Coupling the credential to it means the credential cannot ship until the
  whole bridge ships, and inherits the bridge's reliability/auth risk. A bearer token in a POST
  body over a bespoke channel is also a *weaker* secret posture than a RBAC-scoped, encrypted-
  at-rest Secret.
- **(B)** (bootstrap pull with pod attestation) is clean but needs a new operator bootstrap
  endpoint + per-pod identity (SA `TokenReview` + pod→`sandbox_ref` resolver) for **no security
  gain over A** — A already gets Secret RBAC + encryption-at-rest, and the `sandbox_ref`==pod-name
  join makes attestation unnecessary.

**The credential is a bootstrap secret; the envelope carries task payloads. They use different
transports on purpose.** This is not a "second task-io path" in the sense the design note warns
against — minting and trace injection are still the *one* shared helper (below); only the
*carrier* of the bootstrap secret differs from the carrier of task work, because the two have
different reliability, lifecycle, and secret-handling needs.

## Bind→pod seam contract

| Role | Who | What |
|---|---|---|
| **Mint** | operator, at Bind | `taskio.Minter.MintWithScopes(runID, workItemID, principal, scopes)` — the **same** minter, issuer, claims, and scopes `dispatch.shimCommand` uses. Inputs come from `coord.sandbox_bind` (`run_id`, `work_item_id`, `bound_by` = principal), known once `Pool.Bind` returns. |
| **Trace** | operator, at Bind | `telemetry.Inject(ctx, carrier)` on the **Bind** context (the run.reconcile span is live at Bind, unlike warm Boot) → real run `TRACEPARENT`/`TRACESTATE`. |
| **Write/serve** | operator, at Bind | create-or-update Secret `name=<sandbox_ref>`, namespace = sandbox namespace, keys: `KSQUAD_COORD_URL`, `KSQUAD_COORD_TOKEN`, `WORK_ITEM_ID`, `RUN_ID`, `TRACEPARENT`, `TRACESTATE`. `OwnerReference` → the sandbox pod (auto-GC on teardown; `TearDown` already deletes the pod). Idempotent: Bind is idempotent on `runID`, so a reattach rewrites byte-identical content. |
| **Read** | in-pod supervisor | read token + siblings from the mounted **path** (`/var/run/ksquad/coord/`), *waiting/watching* for the token file to appear (it is written at Bind, after Boot); then `Extract` the traceparent and initialize the task-io client against `KSQUAD_COORD_URL`. |
| **RBAC** | operator SA | `create`/`update`/`get` on `secrets` **scoped to the sandbox namespace** (not cluster-wide). A pod can mount only the Secret named in its own (operator-authored) spec — kubelet enforces; no cross-sandbox read. Secret honors cluster encryption-at-rest. |
| **Minimal-env invariant** | preserved & strengthened | container env at Boot still carries **no operator secret** (no `DATABASE_URL`, no `os.Environ`). The coord token arrives via a dedicated Secret volume containing **only** the run-scoped token + trace carrier + IDs — nothing else. The topology-1 minimal-env test property is mirrored for topology 2. |

**Shared helper (no divergence):** extract the env/claim construction currently inline in
`dispatch.shimCommand` into a `taskio` helper (e.g. `taskio.RunCredential{URL,Token,WorkItemID,
RunID,Traceparent,Tracestate}` + a builder that mints and injects). `shimCommand` renders it to
**env**; the Bind path renders the *same struct* to **Secret keys**. Both topologies stay
byte-for-byte identical in what the credential contains — the only difference is env vs. file.

## Sandbox-contract change (name it so ISI-3614 wires it mechanically)

The **one** downstream behavioral change: the sandbox image/supervisor moves from
**"read `KSQUAD_COORD_TOKEN` from env"** to **"read the coord credential from
`/var/run/ksquad/coord/` (path), waiting for it to appear."** Concretely, ISI-3614:

1. **Boot** (`warmpool.kube.go` `Boot`/`sandboxEnv`): add an **optional** projected Secret
   volume + `volumeMount` at `/var/run/ksquad/coord`, `secretName: <sandboxID>`,
   `optional: true` (Boot must not block on a Secret that does not exist until Bind). Container
   env is otherwise unchanged; the Boot-time `TRACEPARENT` stays for the pool/warm-boot trace,
   but the **run-scoped** traceparent is the one delivered in the Secret at Bind.
2. **Bind** (operator Bind path, alongside/after `coord.ProdEffects.BindSandbox`): mint + write
   the Secret `<sandbox_ref>` per the seam contract above, using the shared `taskio` helper.
3. **Supervisor/image:** read the credential from the mounted path (block/watch until present),
   `Extract` the traceparent, start the task-io client. This is the contract line the sandbox
   image owner must land with ISI-3614.

D1's `PodProxyTransport` + dispatcher warmpool branch is tracked as separable follow-up work and
is **not** required for ISI-3614's credential delivery to function (a pod-side supervisor that
polls/reattaches its Run can already use the coord credential the moment the Secret lands).

## Consequences

**Positive**
- ISI-3614 becomes mechanical and can land **now**, independent of the (larger) D1 bridge.
- Kube-native: no new push protocol, no attestation service, no bespoke secret channel. Secret
  RBAC + encryption-at-rest give a *stronger* posture than an inline-token envelope.
- Credential lifecycle is GC'd for free via the pod OwnerReference; Bind idempotency makes writes
  safe under reattach/concurrent leaders.
- Trace continuity (D1/ISI-3348 finding 3) is preserved: the run traceparent is injected at Bind
  (where the span is live), so supervisor spans join the Run trace exactly as shim spans do.

**Negative / accepted**
- **Secret-propagation latency.** Kubelet propagates a newly-created Secret into an already-
  mounted volume on its next sync (seconds under the default watch strategy, up to the sync
  period otherwise). This adds a small first-task delay on the warm path (partially offsetting the
  warmpool latency win, NFR-PERF1). Accepted: it applies once per Run, not per task, and the
  supervisor waits on the file rather than racing it. `ponytail:` if this latency ever bites,
  option B (bootstrap pull) is the upgrade path — but do not pre-build it.
- **Per-sandbox Secret churn.** One Secret per Run in the sandbox namespace. Bounded by live Runs
  and auto-GC'd; acceptable.
- **Two transports** (Secret for credential, pod-proxy for task). Deliberate — see D2 rationale.

**Overriding the design note.** `docs/design/bind-path-taskio-token-delivery.md` favored **C**
(fold the token into the envelope) with A as fallback. This ADR selects **A** as primary and
keeps the envelope for task payloads only. Reason: the ticket's success criterion is unblocking
ISI-3614 with the least unbuilt machinery, and C makes the credential hostage to the unbuilt D1
bridge while offering a weaker secret posture. The note's *core* guidance is honored in full —
reuse `taskio.Minter` + `telemetry.Inject`, mint at Bind against the now-known
`(RUN_ID, WORK_ITEM_ID)`, one shared helper, minimal-env invariant preserved.

## Verification (smallest proof for the wiring, ISI-3614)

- **Unit:** shared `taskio` helper renders the same `RunCredential` to env (topology 1) and to
  Secret keys (topology 2) — assert byte-identical values for a fixed `(runID, workItemID,
  principal, ctx)`.
- **Unit:** Bind path writes Secret `<sandbox_ref>` with the 6 keys, an OwnerReference to the
  pod, and is idempotent across a second (reattach) Bind — assert no second/divergent write.
- **Invariant test (mirror topology 1):** the sandbox pod spec carries **no** operator secret in
  container env; the coord token appears **only** in the mounted Secret.
- **Trace:** a Bind on a traced context yields a non-empty `TRACEPARENT` in the Secret; a warm
  Boot alone yields an empty run traceparent (carrier-less) — proving the run trace is delivered
  at Bind, not Boot.
