# CI/CD Workflows

This directory holds the KSquad GitHub Actions pipeline. Workflows are a **matrix over every
deployable component** — `ksquad-operator`, `ksquad-apiserver`, `ksquad-memory`, `ksquad-console`,
and the per-runtime shim images — so component pipelines run in parallel and each is an independent
required status check.

| Workflow | Purpose | Trigger |
|----------|---------|---------|
| `component-matrix.yml` | **reusable** single-source-of-truth for the component fan-out (`go`/`shims`/`helm`/`node` matrices) | `workflow_call` (consumed by the lanes) |
| `ci.yml` | lint · build · unit + integration test · coverage · helm lint/render, per component | PR + push to `main` |
| `spine-chaos.yml` | coordination-spine concurrency/chaos suite (claim/lease/fencing) against real Postgres in kind, race-on | PR touching spine paths · push · nightly |
| `build-images.yml` | multi-arch build → push `ghcr.io` → SBOM (Syft) → CVE scan (Trivy) → sign+attest (cosign, release) | push to `main` · tags `v*` |
| `security.yml` | govulncheck · npm audit · Trivy fs/config · gitleaks · CodeQL | PR + weekly |
| `e2e.yml` | **`e2e-ollama` lane** (Story 14.8) — full squad smoke via local, digest-pinned Ollama (credit-free, $0) + console E2E (Playwright); consumes `component-matrix.yml` to single-source the opencode shim it drives; scaffolded/skipped-with-reason until the opencode shim (5.8) + conformance harness (ISI-2114) land | nightly · release · manual |

## Conventions

- **Action versions** are pinned to majors that run on the **Node 24 runtime** (Node 20 action
  runtimes are deprecated). Keep them current via Renovate/Dependabot.
- **Images** publish to `ghcr.io/${{ github.repository_owner }}/ksquad-<component>`,
  multi-arch `linux/amd64,linux/arm64`.
- **Skeleton phase:** each lane guards on the presence of its component source and emits a
  `::notice::` skip when the source has not landed yet — no lane silently disappears, and the matrix
  becomes fully active as components are implemented.
- **Coverage gates:** ≥ 80% per Go package; the coordination-spine package is held to ≥ 90%.
- **Required checks / branch protection** are configured at the repo level, and the self-hosted
  runner (`gitrunner`) is documented in [`../SELF_HOSTED_RUNNER.md`](../SELF_HOSTED_RUNNER.md).

The coordination-spine chaos suite (`spine-chaos.yml`) is the most correctness-critical gate: it
proves at-most-one-holder claims, crash-reclaim, fence-token rejection of zombie writers, and
idempotent reconcile. It is required for any change to coordination code.

## Component matrix (Story 14.7)

`component-matrix.yml` is the **reusable** fan-out primitive. It is invoked with
`uses: ./.github/workflows/component-matrix.yml` and emits four typed JSON matrices that every lane
derives its per-component jobs from via `fromJSON()`, so the component list lives in exactly one
place and cannot drift lane-to-lane. Downstream lanes (L5 e2e, supply-chain, the Ollama credit-free
lane — stories 14.5 / 14.6 / 14.8) consume it the same way `ci.yml` does:

```yaml
jobs:
  components:
    uses: ./.github/workflows/component-matrix.yml
  go:
    needs: components
    strategy:
      matrix:
        include: ${{ fromJSON(needs.components.outputs.go) }}
```

`coord` (the coordination spine) is intentionally **not** a separate unit leg — it is covered by
every go leg's `go test ./...` and gated for correctness by `spine-chaos.yml`.

### Stable check-run names (branch protection registry — ISI-2674)

Branch protection requires these exact check-run names per component. They are emitted now (with
skeleton skip-with-reason legs) so protection can be wired before every component lands, without
wedging merges:

| Check name | Lane | Component |
|------------|------|-----------|
| `go / operator` | `ci.yml` | operator |
| `go / apiserver` | `ci.yml` | apiserver |
| `go / memory` | `ci.yml` | memory |
| `shim / claude` | `ci.yml` | shim (claude runtime) |
| `shim / openclaw` | `ci.yml` | shim (openclaw runtime) |
| `shim / hermes` | `ci.yml` | shim (hermes runtime) |
| `shim / opencode` | `ci.yml` | shim (opencode runtime) |
| `helm / k8squad` | `ci.yml` | helm chart |
| `node / console` | `ci.yml` | console |
| `db / migrations self-check` | `ci.yml` | migrations |
| `DCO / Check sign-off` | `dco.yml` | all (DCO-only merges — ISI-2609) |

Renaming any of these silently drops a required check, so keep the job `name:` templates in sync
with `component-matrix.yml`.
