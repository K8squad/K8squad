# CI/CD Workflows

This directory holds the KSquad GitHub Actions pipeline. Workflows are a **matrix over every
deployable component** — `ksquad-operator`, `ksquad-apiserver`, `ksquad-memory`, `ksquad-console`,
and the per-runtime shim images — so component pipelines run in parallel and each is an independent
required status check.

| Workflow | Purpose | Trigger |
|----------|---------|---------|
| `ci.yml` | lint · build · unit + integration test · coverage, per component | PR + push to `main` |
| `spine-chaos.yml` | coordination-spine concurrency/chaos suite (claim/lease/fencing) against real Postgres in kind, race-on | PR touching spine paths · push · nightly |
| `build-images.yml` | multi-arch build → push `ghcr.io` → SBOM (Syft) → CVE scan (Trivy) → sign+attest (cosign, release) | push to `main` · tags `v*` |
| `security.yml` | govulncheck · npm audit · Trivy fs/config · gitleaks · CodeQL | PR + weekly |
| `e2e.yml` | full squad smoke via local Ollama (credit-free) + console E2E (Playwright) | nightly · release · manual |

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
