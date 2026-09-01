# ADR-0002 — CRD lifecycle management via Helm (upgrade-safe CRDs)

- **Status:** Accepted (Option B) — *supersedes the Option-A revision of 2026-09-01*
- **Date:** 2026-09-01 (revised)
- **Author:** Winston (System Architect)
- **Issue:** ISI-3517 (parent ISI-3516)
- **Implements via:** ISI-3518 (DevOps) · ISI-3519 (Docs/website)
- **Decision authority:** board (`local-board`) directive on ISI-3516 — *"the
  CRDs must be a different helm chart so the k8squad control plane can be
  upgraded separately from the CRDs."* The A-vs-B question is **closed: B.**

## Context

K8squad ships 11 `ksquad.io` CRDs (`agents, agentruntimes, egresspolicies,
mcpservers, otelconfigs, projects, roles, runs, skills, teams, toolchains`).
They currently live in `config/helm/crds/` — Helm's **special `crds/`
directory**.

Helm treats that directory specially and by documented design:

- Installs the CRDs **once**, on `helm install`, before templates render.
- **Never upgrades, patches, or reconciles them on `helm upgrade`.**
- Ignores `--set` values, hooks, and templating for those files.

We have had steady CRD churn — Skills, OtelConfig, EgressPolicy, revision
annotations, CEL validation rules. Every existing install that came up on an
older chart is now running a **stale CRD schema**: new fields are silently
dropped by the API server and CRs that use them are rejected. There is no
supported Helm path to fix this in place; operators must `kubectl apply` CRDs
by hand.

Generation/sync today: `make manifests` emits `config/crd/bases/*.yaml`;
`make helm-sync-crds` `cp`s them into `config/helm/crds/`; `make verify-codegen`
fails CI on drift between `api/`, `config/crd/bases/`, and the chart copy.

## Decision

**Adopt Option B — a standalone `k8squad-crds` Helm chart, independently
versioned, that owns all CRD lifecycle.** The control-plane chart (`k8squad`,
`config/helm`) stops shipping CRDs entirely.

Rationale (board directive): the control plane and the CRD schema must be
**upgradeable independently**. A single chart couples them into one release
whose upgrade rewrites both at once; the board requires operators (and our
GitOps/multi-tenant story) to roll the CRD schema forward or hold it back on
its own cadence. A separate, independently-versioned chart is the standard
pattern for this (cert-manager's split CRD chart, prometheus-operator CRDs,
Flux) and is what we build.

Two properties make it upgrade-safe:

- CRDs live as **ordinary templates** in the CRD chart, so
  `helm upgrade k8squad-crds` reconciles their schema via three-way merge —
  the whole point of the ticket, now decoupled from the control-plane release.
- Every CRD template carries `helm.sh/resource-policy: keep` (gated by
  `.Values.keep`, default **true**), so `helm uninstall k8squad-crds` never
  deletes the CRDs or the user's CRs.

### Why not Option A (templated CRDs inside the control-plane chart)

Option A (the prior revision of this ADR) kept everything in one chart and made
`helm upgrade` of the control-plane chart reconcile CRD schema. It preserves a
one-command install but **couples** CRD-schema changes to every control-plane
upgrade and vice-versa — precisely the coupling the board ruled out. Superseded.

### Why not a subchart

A `config/helm/charts/k8squad-crds` **subchart** would satisfy "different
`Chart.yaml`" on paper but is installed and upgraded as part of the **same
parent release** — you cannot `helm upgrade` the subchart independently of the
parent. That fails the board's core requirement (separate upgrade cadence).
Rejected. The CRD chart is a **standalone, independently-releasable** chart.

## 1. Chart layout changes

New standalone chart, sibling to the canonical control-plane chart:

```
config/
  helm/                         # control-plane chart (name: k8squad)
    crds/                       # REMOVED — CP chart ships NO CRDs
    templates/…                 # unchanged (services, catalog CR, namespace)
  helm-crds/                    # NEW standalone chart (name: k8squad-crds)
    Chart.yaml                  # name: k8squad-crds, OWN version line
    values.yaml                 # keep: true
    values.schema.json          # { keep: bool } additionalProperties:false
    README.md                   # install-first ordering + skew policy
    templates/
      _helpers.tpl
      ksquad.io_agents.yaml     # 11 CRDs as guarded templates
      … (11 files)
```

- The generated CRD YAML is **unchanged**; only its home and a small
  keep-annotation wrapper differ.
- `k8squad-crds/Chart.yaml` carries its **own `version`** (start `0.1.0`),
  bumped whenever any CRD schema changes — this is the version operators pin
  and roll independently.
- The control-plane chart's `config/helm/crds/` directory is **deleted**; the
  `k8squad` chart no longer installs CRDs by any path.

## 2. Values keys (`k8squad-crds` chart)

```yaml
# k8squad-crds/values.yaml
# Annotate CRDs with helm.sh/resource-policy: keep so `helm uninstall`
# never deletes the CRDs or the user's custom resources. Leave true in
# production; set false only for throwaway/test clusters.
keep: true
```

No `crds.install` toggle is needed on this chart — installing the CRD chart *is*
the opt-in; not installing it (or `helm uninstall k8squad-crds --set keep=false`)
is the opt-out. The **control-plane** chart gains no CRD values keys (it owns no
CRDs). `values.schema.json` for the CRD chart is a single `keep` boolean,
default true, `additionalProperties: false`.

## 3. Install / upgrade ordering (two charts)

**CRDs first, control plane second** — always.

```bash
# install
helm install k8squad-crds oci://charts.k8squad.io/k8squad-crds --wait
helm install k8squad      oci://charts.k8squad.io/k8squad      --wait

# upgrade (roll CRD schema forward before the control plane that uses it)
helm upgrade k8squad-crds oci://charts.k8squad.io/k8squad-crds --wait
helm upgrade k8squad      oci://charts.k8squad.io/k8squad      --wait
```

Rationale: the API server must have the CRDs **established** before the
control-plane chart creates any CR (e.g. `toolchain-default-catalog.yaml`'s
`Toolchain`). Installing/upgrading `k8squad-crds` first with `--wait` guarantees
registration ordering across the two releases; within the CRD chart, all
resources are the same kind so intra-release ordering is trivial.

Getting-started (ISI-3519) documents both as an explicit two-step, and MAY offer
a convenience wrapper (`Makefile`/script) that runs them in order — but the two
charts stay independently installable and upgradeable.

## 4. Version-skew policy between the two charts

- **Contract:** the control-plane chart declares the **minimum `k8squad-crds`
  version** it requires. Record it as `annotations."k8squad.io/min-crds-version"`
  in `config/helm/Chart.yaml` and in the CP chart README/NOTES.
- **Enforcement (best-effort, non-fatal):** CP chart NOTES.txt prints the
  required CRD-chart version and a `kubectl get crd` hint so operators can
  self-check. A hard preflight (fail the CP release if CRDs are older) is
  **out of scope** for this ADR — it needs a lookup the board hasn't asked for;
  DevOps MAY add a `helm.sh/hook` preflight later if skew bites in practice.
- **Skew tolerance, grounded in §5's additive-only rule:**
  - **CRD chart newer than CP requires → always safe.** New CRD fields are
    optional/additive; an older control plane simply doesn't populate them.
    This is what makes "upgrade CRDs first" correct.
  - **CRD chart older than CP requires → unsupported.** A control plane that
    writes a field whose CRD schema predates it will have that field rejected.
    Operators must roll `k8squad-crds` to at least `min-crds-version` first.
- **Bump discipline:** any CRD schema change bumps `k8squad-crds` `version`; a CP
  change that *depends* on a new CRD field bumps
  `k8squad.io/min-crds-version` to match.

## 5. CRD versioning / deprecation policy

Current served/storage version for all kinds: **`v1alpha1`** (single version).

While pre-1.0 (`v1alphaN`):

- **Additive-only within a version.** New optional fields, new enum values,
  relaxed validation → ship in `v1alpha1`; `helm upgrade k8squad-crds`
  propagates them. This is the common case; bump only the CRD chart `version`.
- **Breaking changes** (remove/rename a field, tighten validation that would
  reject stored objects, change a field's type/semantics) require a **new API
  version** (`v1alpha2`/`v1beta1`), served alongside the old one, with a
  **conversion path** (none/webhook) before the old version is deprecated.
- **Deprecation:** mark the outgoing version `deprecated: true` with a
  `deprecationWarning` in the CRD `versions[]` entry. Keep it **served** until
  no client uses it.
- **Storage version migration:** never drop a version that is still
  `storage: true` or that still has stored objects. Promote the new version to
  storage, migrate stored objects, *then* drop the old version in a later CRD
  chart release.
- **Never** shrink schema on `helm upgrade` in a way that would orphan or reject
  existing CRs. The invariant below is absolute.

## 6. Invariants (non-negotiable)

1. `helm upgrade k8squad-crds` **must** propagate CRD schema changes to existing
   installs, **independently** of the control-plane chart's release cadence.
2. **No path** — upgrade, uninstall, rollback of either chart — may **delete a
   user's custom resources**. `resource-policy: keep` + never removing a
   stored/served version enforce this. Uninstalling the *control-plane* chart
   leaves CRDs and CRs untouched because it owns neither.
3. Generated CRD YAML remains the single source of truth
   (`config/crd/bases/`); `verify-codegen` keeps the CRD chart copy in lockstep.
4. Install/upgrade order is **CRDs-first**; the version-skew contract (§4) holds.

## 7. Acceptance criteria for DevOps implementation (ISI-3518)

- **AC-1** New standalone chart `config/helm-crds` (name `k8squad-crds`) created
  with its own `Chart.yaml` (independent `version`, start `0.1.0`),
  `values.yaml` (`keep: true`), `values.schema.json` (`keep` boolean, default
  true, `additionalProperties:false`), and `README.md`.
- **AC-2** All 11 CRDs live as templates under `config/helm-crds/templates/`,
  each carrying `helm.sh/resource-policy: keep` gated by `{{- if .Values.keep }}`.
  `config/helm/crds/` is **removed**; the `k8squad` control-plane chart ships
  **zero** CRD objects (assert via `helm template config/helm | grep -c
  CustomResourceDefinition` → 0).
- **AC-3** `make helm-sync-crds` retargets to `config/helm-crds/templates/` and
  injects the keep-annotation wrapper on sync; `make verify-codegen` checks the
  new path and passes clean (no drift) after `make manifests`.
- **AC-4 (the core proof)** CI test on kind: `helm install k8squad-crds` at an
  **older** chart revision whose `runs` CRD lacks a recently-added field, then
  `helm upgrade k8squad-crds` to HEAD **without touching the control-plane
  chart**, then assert `kubectl get crd runs.ksquad.io -o jsonpath=...` shows
  the new field. Proves independent CRD upgrade — the whole ADR.
- **AC-5** `helm uninstall k8squad-crds` with `keep=true` (default) leaves all 11
  CRDs **and** any existing CRs intact; with `keep=false` the CRDs are removed.
  Separately assert `helm uninstall k8squad` (control plane) leaves all CRDs/CRs
  intact regardless. All cases in CI.
- **AC-6** Ordering test: `helm install k8squad-crds --wait` then
  `helm install k8squad --wait` succeeds with the `toolchain-default-catalog`
  `Toolchain` CR admitted. Reversing the order (CP first) fails fast with a
  clear "no matches for kind" — documented as expected.
- **AC-7** `config/helm/Chart.yaml` gains
  `annotations."k8squad.io/min-crds-version"`; CP chart `NOTES.txt` prints the
  required CRD-chart version and a self-check hint (§4).
- **AC-8** Both charts publish to `charts.k8squad.io` (gh-pages index) as
  separate chart entries; `helm lint` + `helm template` clean for both; release
  tooling bumps `k8squad-crds` `version` on any CRD change.

## 8. `deploy/helm/ksquad` disposition

`deploy/helm/ksquad` is the **services** chart and ships **no CRDs**. Under
Option B it depends on the CRDs being installed by the standalone `k8squad-crds`
chart (documented as a prerequisite), exactly like the canonical `config/helm`
control-plane chart now does. It needs **no** CRD-lifecycle change. Whether
`deploy/helm/ksquad` is formally deprecated or converged into `config/helm`
remains a separate chart-consolidation decision, out of scope here.

## Consequences

- **Positive:** CRD schema rolls forward (or holds) **independently** of the
  control plane, satisfying the board directive and our GitOps/multi-tenant
  story; standalone `k8squad-crds` version is the single pin operators track;
  `resource-policy: keep` guarantees CRs survive any uninstall; matches
  cert-manager/prometheus-operator/Flux norms.
- **Negative / accepted:** two charts to install and upgrade (mitigated by a
  documented two-step + optional wrapper, §3); an ordering contract operators
  must follow (CRDs-first, §3) and a version-skew contract to honor (§4);
  release tooling must publish and version two charts.
