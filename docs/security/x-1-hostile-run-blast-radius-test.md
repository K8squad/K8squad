# Story X.1: Hostile-Run blast-radius test (the S4 containment gate)

Status: ready-for-dev

<!-- ISI-2240. Cross-cutting security-verification story (Epic X / arch §4.3, §17.1). -->

> **🧨 THIS STORY IS THE EXECUTABLE PROOF THAT THE ISOLATION MODEL HOLDS — "tested, not asserted"
> (arch §17.1, F6/F7).** Every prior isolation story *builds* a boundary: 4.1 namespace tenancy, 4.2
> the RuntimeClass kernel boundary, 4.5 teardown + per-principal PVC scoping, 4.6 default-deny egress,
> 6.x memory provenance. This story is the **adversary** that tries to punch through all of them at
> once. It gives the architecture's **S4 blast-radius claim** teeth: *given a Run executing arbitrary
> LLM-authored code, when it reaches for another squad's / another principal's / a prior Run's
> **network, secrets, or workspace**, then the attempt FAILS.* A posture that merely *looks* isolated
> but leaks on one axis is a **security failure, not a bug ticket**. This is a **required CI artifact**
> (§4.3 gate policy, L4 Security).

## Gate status (read first)

**GATE ISI-2113 (RuntimeClass pick) — CLEARED.** The hostile-Run threat model is decided *before*
this test: gVisor default / Kata opt-in / `runc` rejected for untrusted code (Story 4.2, ratified on
hardware ISI-2292/ISI-2294; arch §9.1). This blast-radius test is the **S4 layer that sits on top of
that kernel boundary** — it verifies namespace + NetworkPolicy + RBAC + per-principal workspace +
teardown-and-replace, *assuming* the syscall boundary already holds. The story is therefore unblocked.

## Story

As **the security architect who must ship the S4 blast-radius gate the architecture promises**,
I want **an executable, differential harness that puts a hostile Run against a squad boundary and
derives — not asserts — whether it can reach another squad's network, another principal's secrets,
or a prior Run's workspace residue**,
so that **cross-squad / cross-principal / reuse-residue containment (NFR-SEC1/SEC4/SEC5, arch §12/§17.1)
is proven on every CI run, and any regression that opens a blast-radius path fails the build loudly
instead of shipping a silent leak.**

## Context & prerequisites (read first)

- **PRD:** `docs/bmad/02-prd.md` — **NFR-SEC1** (cross-squad isolation), **NFR-SEC2** (untrusted-code
  threat model), **NFR-SEC4** (egress control), **NFR-SEC5** (per-principal workspace), **S4** (the
  named hostile-Run containment test), **§1 tiebreaker** (isolation > latency).
- **Architecture:** `docs/bmad/03-architecture.md` — **§4.3 / §17.1** (agent-as-adversary threat
  model, "tested not asserted"), **§12.1** (squad = namespace, per-principal Secret isolation),
  **§12.2** (default-deny egress + model-endpoint allowlist + egress proxy), **§9.3** (teardown-and-
  replace hygiene), **§9.4** (per-principal PVC subPath scoping). **Testing:** `05-testing-strategy.md`
  **§6.5** (the S4 blast-radius case table this story implements).
- **Depends on (build, not re-test):** 4.1 (namespace + default-deny + least-priv RBAC), 4.5
  (teardown-and-replace + per-principal PVC subpath), 4.6 (default-deny egress + allowlist). This
  story is the *composed adversary*; it does not re-derive those provisioners (that is
  `tenancy-isolation-check.py`, Story 4.1). It derives **reachability** across the boundary they build.

## Acceptance criteria

1. **AC1 — cross-squad network contained (NFR-SEC1).** A hostile Team-A Run cannot reach a Team-B
   service; the harness *evaluates* this via K8s egress semantics, and a posture with no egress policy
   is detected as breached.
2. **AC2 — default-deny egress + named audited hole (NFR-SEC4, F11).** Arbitrary internet egress is
   denied; the **one** allowlisted model endpoint is reachable **but audited** (egress proxy) — a
   *silent* allowlist hole is a breach, not containment.
3. **AC3 — cross-squad AND same-squad-cross-principal Secret contained (NFR-SEC1, §12.1).** The Run
   reads only its own BYO Secret (namespaced get-by-name); a Team-B Secret (cross-ns) and a same-ns
   peer's Secret are both denied. A ClusterRole or namespace-wide `secrets: list` is detected.
4. **AC4 — per-principal workspace contained (NFR-SEC5, §9.4).** On the shared Project PVC the Run
   sees only its own per-principal subPath; a peer principal's cache subtree is unreachable; a
   root-mounted PVC is detected as a leak.
5. **AC5 — reuse-residue clean (§9.3, F6/F7).** After teardown-and-replace the fresh sandbox exposes
   **zero** prior-Run scratch/worktree/secret; an in-place reset that leaves residue is detected.
6. **AC6 — differential teeth + legitimate paths intact.** A **porous** control posture must breach
   **every** axis (NET/SEC/WS/RES) or the harness is declared toothless (fail); the **arch** posture
   must contain every attempt **and** keep DNS / control-plane / own-model / own-Secret / own-worktree
   working. **Required CI artifact**: `python3 hostile-run-blast-radius-check.py`, exit 0.

## Runnable falsification check (ships with this story)

`hack/security/hostile-run-blast-radius-check.py` — stdlib-only, deterministic, exit 0 = pass.
It is **computed, not asserted**: it carries a NetworkPolicy egress evaluator (K8s semantics), an
RBAC/Secret evaluator (namespaced Role + `resourceNames` get-by-name; ClusterRole reaches all ns), a
per-principal workspace-subPath evaluator, and a teardown-residue evaluator. Each hostile attempt's
verdict is *derived from the posture*, so weakening a control flips the verdict.

**Two postures (differential):** `posture_contained` (arch §9/§12/§17.1) must hold every invariant;
`posture_porous` (no egress policy · ClusterRole cross-ns `secrets: list` · PVC root mount · in-place
reset) must breach every axis. The harness fails if the porous posture does **not** breach all four
axes (teeth lost) or if the arch posture breaches any / breaks a legitimate path.

**Mutation teeth (verified 2026-08-14).** Each load-bearing control in the contained posture, when
neutered, flips exactly its axis RED:

| Mutation | Result |
|----------|--------|
| drop the egress policies | **RED** — 3 NET breaches (Team-B + internet reachable) |
| model hole un-audited | **RED** — 1 NET breach (silent allowlist hole) |
| RBAC → cluster + `secrets: list` | **RED** — 2 SEC breaches (cross-ns Team-B + same-ns peer) |
| RBAC → namespaced + `secrets: list` | **RED** — 1 SEC breach (same-ns peer only; cross-ns still walled) |
| PVC mounted at root | **RED** — 1 WS breach (peer principal's cache visible) |
| in-place reset leaves residue | **RED** — 1 RES breach |
| *(control)* name-scoped ClusterRole alone | **GREEN** — faithful: a `get` on `resourceName: alice-cred` still can't read another Secret |

The last row is the harness proving it models RBAC *correctly*, not loosely: a cross-namespace breach
needs cluster scope **and** wide verbs / broad `resourceNames`, exactly as Kubernetes evaluates it.

## CI wiring (as landed — no over-claim)

Landed in the **k8squad code repo** (`github.com/K8squad/K8squad`) — not the local-only BMAD repo — as a
dedicated `s4-blast-radius-model-gate` job in `.github/workflows/security.yml`. Pure stdlib, no cluster:
runs on every PR and push to `main` (`python3 hack/security/hostile-run-blast-radius-check.py`,
exit 0 required).

**Scope caveat (retro-review ISI-2537 F2):** this is a **design-level model gate** — it falsifies the
isolation *model* (two hand-authored postures), it does **not** parse `config/` and cannot fail on a real
manifest regression. `tenancy-isolation-check.py` (Story 4.1) lives only in the BMAD planning repo and is
**not** wired here; do not claim it as a k8squad CI gate until it is landed under its own story. Two
follow-ups on the L4-security lane (ISI-2245) close the gap: (1) anchor `posture_contained` to real parsed
`config/` NetworkPolicy+RBAC once those manifests exist; (2) the kind-based runtime S4 e2e (§6.5).

## Out of scope

- The **cross-principal same-Team build-browser read-authZ** case (§6.5, ISI-2166) is the **8.7d**
  gate (`GET /api/runs/{runId}/build/*` → 404), verified there; not duplicated here.
- The **memory-poisoning / provenance-forgery** covert-channel case (§7.3, F5/F6) is Epic 6's
  provenance tests; this story covers network/secret/workspace/reuse-residue blast radius.
