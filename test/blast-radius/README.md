# S4 blast-radius suite — Story 14.4 (ISI-2245)

The **Half B** gate of the L4 security suite (§6.5 / §17.1 "security is tested,
not asserted"): a **kind** cluster, a **hostile-Run fixture**, and the six S4
cases — each **differential** (guard-on contains; guard-off escapes) and
**fail-loud**. Run by `.github/workflows/blast-radius.yml` on
isolation-touching paths and nightly; runnable standalone against any kind
cluster with an enforcing CNI.

The semantics are the 1:1 translation of the bench anchor
`docs/bmad/spikes/bench/blast-radius-check.py` (ksquad docs repo):

| Case | Proves | Anchor function | Mutation (AC4) |
|------|--------|-----------------|----------------|
| S4-1 | default-deny egress — arbitrary dst unreachable, allowlisted path (via proxy) reachable | `egress_reachable` | delete team-a egress NetworkPolicies |
| S4-2 | the allowlisted hole is **audited** (proxy access log, attributable by pod IP), never silent (F11) | `egress_audited` | bypass the proxy with a direct sandbox→infra allow |
| S4-3 | cross-namespace isolation — team-b Service unreachable AND team-b Secrets unreadable | `cross_ns_reachable` | drop team-b default-deny + widen egress; add a widened RoleBinding |
| S4-4 | teardown-and-replace leaves **no** prior-Run residue on the per-principal PVC subPath (§9.3) | `residue_after_teardown` | skip the teardown wipe |
| S4-5 | principal B reading A's Run build view (same Team) → **404, existence-hiding — never 403** (§9.4, 8.7d) | `build_read_status` | drop the `owningPrincipal` check (apiserver) |
| S4-6 | forged provenance rejected/restamped hostile+untrusted; memory exposes **no** coordination verbs (§7.3, 10.4/12.4) | `memory_read`, `coordination_driveable_by_memory` | accept the claimed author (memory service) |

## Layout

```
run-s4.sh          orchestrator: Calico bootstrap → fixtures → cases ×2 arms → report
lib.sh             shared result vocabulary, probes, suffix-aware apply
fixtures/*.yaml    the isolation primitives + hostile-Run fixture + tenants
mutations/*.yaml   guard-widening manifests applied by the mutation arm
cases/*.sh         the S4 drivers (conformance + mutation in each)
kind-config.yaml   CNI-less kind config (Calico bootstrapped by run-s4.sh)
```

## The two arms

- **conformance** (`run-s4.sh conformance`, namespaces `team-a`/`team-b`/
  `ksquad-infra`): guards ON. Every attack must be **contained**.
- **mutation** (`run-s4.sh mutation`, suffix `-mut` namespaces): each case's
  named guard is deleted/widened first. Every attack must now **escape** —
  a mutation that does not flip the case RED means the suite has no teeth
  and fails the build (AC4).

## Dependency gating (AC8)

S4-5 needs a kind-deployable apiserver (Epic 9 install) and S4-6 a
kind-deployable memory service. Until then both self-skip-with-reason into
the **skip ledger** printed in the report and as `::notice::` annotations —
skipped-not-passed, never a silent drop. They activate automatically when
`BLAST_RADIUS_APISERVER_URL` / `BLAST_RADIUS_MEMORY_URL` is set or the
services are discoverable in-cluster.

## Running

```sh
kind create cluster --config test/blast-radius/kind-config.yaml
test/blast-radius/run-s4.sh all        # conformance + mutation arms
kind delete cluster
```

The suite exits non-zero on any FAIL in either arm and writes
`blast-radius-report.txt` (uploaded as a workflow artifact).
