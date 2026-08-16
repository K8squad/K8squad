# S4 Hostile-Run blast-radius gate (ISI-2240, Story X.1)

`hostile-run-blast-radius-check.py` is the executable **S4 blast-radius containment gate** for the
agent-as-adversary threat model (architecture §4.3 / §12 / §17.1; NFR-SEC1/SEC4/SEC5). It runs in the
**L4 Security** CI lane on every PR:

```bash
python3 test/security/hostile-run-blast-radius-check.py   # exit 0 = contained + teeth intact
```

Pure stdlib, deterministic, no cluster required.

## What it is — a DESIGN-LEVEL MODEL GATE

Read this before trusting the green check (retro-review ISI-2537 F2, reconciled via ISI-2539).

This harness is an **always-on falsification of the isolation _model_**. It carries two hand-authored
postures and a set of small evaluators, and proves the §12/§17.1 blast-radius invariants are internally
sound and that weakening any one control is **detectable**. It does **not** yet parse the repo's
Kubernetes manifests, so it makes **no "runtime containment" claim** — a regression that shipped in
`config/` would not (yet) flip it. That is deliberate: `config/` currently ships only CRDs + a webhook,
so there are no NetworkPolicy/RBAC Team-namespace manifests to anchor against.

Two follow-ups are tracked on the L4 lane under **ISI-2245**:

1. **config-anchoring** — derive the contained posture from the rendered Team-namespace manifests, so a
   real regression (e.g. a `ClusterRole` granting `secrets: list`, a root-mounted workspace PVC) flips
   the gate.
2. **runtime kind-based S4 e2e** — assert containment against live NetworkPolicy/RBAC in a kind cluster.

## What it proves (computed, not asserted)

A hostile Team-A Run (principal `alice`) is *evaluated* against every blast-radius axis; each outcome is
derived from the posture, so a weakened control flips the verdict:

| Axis | Attempt → expected | Mechanism |
|------|--------------------|-----------|
| **NET** | reach Team-B service / arbitrary internet → **denied**; allowlisted model endpoint → **allowed but audited** (F11) | K8s NetworkPolicy egress evaluator |
| **SEC** | read Team-B Secret (cross-ns) / same-ns peer's Secret → **denied**; own Secret → **allowed** | RBAC get-by-name (`resourceNames`) evaluator; `ClusterRole`/`list` reaches wider |
| **WS**  | read a peer principal's cache subtree on the shared Project PVC → **denied**; own subtree → **allowed** | per-principal PVC `subPath` evaluator (§9.4) |
| **RES** | fresh sandbox after teardown-and-replace exposes prior-Run residue → **none** | teardown-residue evaluator (§9.3, F6/F7) |

## Teeth (differential + mutation)

A **porous** control posture (no egress policy · `ClusterRole` cross-ns `secrets: list` · PVC root mount
· in-place reset) must breach **all four** axes or the harness declares itself toothless and fails the
build. Verified: porous → 7 breaches across NET/SEC/WS/RES; the arch posture → 0 breaches, 0 functional
denials. Six load-bearing controls each flip their own axis RED when neutered; a name-scoped
`ClusterRole` *alone* correctly stays GREEN (a `get` on `resourceName: alice-cred` genuinely cannot read
another Secret) — the evaluator models RBAC faithfully, not loosely.

## Related gates (not duplicated here)

- Cross-squad provisioner object-set — `tenancy-isolation-check.py` (Story 4.1, BMAD bench).
- Cross-principal same-Team **build-browser** read-authZ (`404`) — the 8.7d gate (ISI-2166).
- **Memory-poisoning / provenance-forgery** covert channel — Epic 6 provenance tests.
