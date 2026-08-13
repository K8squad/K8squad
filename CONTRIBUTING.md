# Contributing to KSquad

First off — thank you. KSquad is a Kubernetes-native, agent-agnostic platform for
orchestrating AI agents into **squads**, and it gets better every time someone new
pokes at it. This guide gets you from a fresh clone to a merged pull request without
guesswork. It is meant to *lower* the barrier to contributing, not raise a wall of
process — if something here is unclear or wrong, opening a PR to fix this file is a
perfectly good first contribution.

By participating you agree to abide by our [Code of Conduct](./CODE_OF_CONDUCT.md)
(which adopts the CNCF Community Code of Conduct). Found a security issue? Please
**don't** open a public PR or issue — follow [SECURITY.md](./SECURITY.md) to report
it privately.

> **Status: early development.** The repository is in its skeleton phase. Some
> components have not landed yet, and CRDs/interfaces are **not** stable. CI lanes
> "skip-with-reason" for a component until its source exists, so you can build and
> test whatever *is* present without the rest getting in your way.

## Table of contents

1. [Getting started](#1-getting-started)
2. [Development setup](#2-development-setup)
3. [Project structure](#3-project-structure)
4. [Coding standards](#4-coding-standards)
5. [Testing](#5-testing)
6. [Pull request process](#6-pull-request-process)
7. [Changing CRD types](#7-changing-crd-types)
8. [Console development](#8-console-development)
9. [Documentation changes](#9-documentation-changes)
10. [Releasing](#10-releasing)
11. [Licensing & governance](#11-licensing--governance)

---

## 1. Getting started

### Prerequisites

| Tool | Version | Why |
|------|---------|-----|
| **Go** | 1.23+ | Control plane: operator, apiserver, memory server (see `go.mod`) |
| **Node.js** | 24 | Console (Next.js/TypeScript) + BFF. CI pins the Node 24 runtime |
| **Docker** | recent | Building component images; running Postgres/Ollama locally |
| **kubectl** | matches your cluster | Talking to a local Kubernetes cluster |
| **Helm** | 3.x | Installing KSquad and its dependency subcharts |
| **kind** *(or minikube)* | recent | A throwaway local Kubernetes cluster. CI uses `kind` |
| **psql** / Postgres | 16 | Applying and self-checking DB migrations |

`controller-gen` and other Go tool binaries are installed **for you** into `./bin`
by the Makefile the first time you need them — no manual install, no global pollution.
`golangci-lint` is the one Go tool you'll want on your PATH for local linting
(instructions in [§4](#4-coding-standards)).

### Clone and build

```bash
git clone https://github.com/K8squad/K8squad.git
cd K8squad

# Generate DeepCopy methods + CRD manifests from the API types, then build.
make generate manifests
go build ./...
```

`make` with no target runs `generate manifests` (the `all` target). During the
skeleton phase, `go build ./...` compiles whatever component source is present.

---

## 2. Development setup

KSquad is an **operator-based platform**: agents, teams, roles, skills, projects, and
runs are Kubernetes custom resources reconciled by controllers, backed by Postgres as
the single source of truth for durable state. A realistic dev loop therefore wants a
local cluster and a local Postgres.

### A local cluster

```bash
# kind (what CI uses)
kind create cluster --name ksquad-dev

# ...or minikube
minikube start
```

### Local Postgres for the coordination spine

The coordination record and audit log live in Postgres. The quickest way to get one
for running migrations and integration tests:

```bash
docker run --rm -d --name ksquad-pg \
  -e POSTGRES_USER=ksquad -e POSTGRES_PASSWORD=ksquad -e POSTGRES_DB=ksquad_dev \
  -p 5432:5432 postgres:16

export DATABASE_URL=postgres://ksquad:ksquad@localhost:5432/ksquad_dev
```

Apply the forward-only migrations (they run in filename order):

```bash
for f in db/migrations/*.sql; do
  case "$f" in *_test.sql) continue;; esac
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$f"
done
```

The chaos/concurrency suite ([§5](#5-testing)) expects a real Postgres inside a kind
cluster via **CloudNativePG (CNPG)**; that setup mirrors `.github/workflows/spine-chaos.yml`.

### Hot-reload

There is no committed Tilt/Skaffold config yet. If you set one up for your own loop,
propose it in a PR — a shared `Tiltfile`/`skaffold.yaml` that drives the operator +
apiserver + console against a kind cluster would be a welcome contribution. Until then,
the fast inner loop is `go build ./... && go test ./...` for backend work and the
console dev server (`npm run dev`, [§8](#8-console-development)) for frontend work.

---

## 3. Project structure

The backend is a Go monorepo using the **kubebuilder `go.kubebuilder.io/v4`** layout
(domain `ksquad.io`, module `github.com/K8squad/K8squad`). Not every directory exists
yet — the tree fills in as components land.

```
.
├── api/v1alpha1/           # CRD Go types (Team, Agent, Role, Skill, Project, Run …)
│                           #   + zz_generated.deepcopy.go (generated — do not hand-edit)
├── cmd/
│   ├── operator/           # ksquad-operator — reconcilers for the CRDs
│   ├── apiserver/          # ksquad-apiserver — coordination record, audit API, SSE bus
│   └── memory/             # ksquad-memory — MCP memory server (pgvector, provenance)
├── pkg/
│   ├── coord/              # coordination spine: claim / lease / fencing (correctness-critical)
│   ├── scm/                # source-control provider seam (GitHub mirror, etc.)
│   ├── auth/               # auth/session + RBAC
│   └── search/             # global search
├── internal/               # non-exported implementation packages (e.g. internal/coord)
├── console/                # Next.js/TypeScript operator console + BFF, Playwright E2E
├── db/migrations/          # versioned, forward-only SQL (+ *_test.sql self-checks)
├── config/crd/bases/       # generated CRD manifests (controller-gen output)
├── hack/                   # boilerplate.go.txt and dev scripts
├── test/e2e/               # Go end-to-end squad scenarios (build tag `e2e`)
├── .github/workflows/      # CI/CD: ci, spine-chaos, build-images, security, dco, e2e
├── docs/                   # public documentation
├── Dockerfile.*            # per-component image builds (operator/apiserver/memory/console/shim)
└── Makefile, go.mod, PROJECT, .golangci.yml
```

Where things live, in one line each:

- **A new CRD field?** `api/v1alpha1/*_types.go` → then regenerate ([§7](#7-changing-crd-types)).
- **Coordination logic (claim/lease/fence)?** `pkg/coord` + `internal/coord`. This is the
  most correctness-critical code in the repo — expect the chaos suite to gate it.
- **A schema change?** A new forward-only migration in `db/migrations` ([details below](#5-testing)).
- **A console screen?** `console/` ([§8](#8-console-development)).

> `bin/`, `bmad/`, and `.paperclip/` are gitignored — build output and internal
> planning artifacts, not part of the source tree.

---

## 4. Coding standards

### Go

- **Format and vet before you push:**

  ```bash
  make fmt      # go fmt ./...
  make vet      # go vet ./...
  ```

- **Lint with golangci-lint** (config in `.golangci.yml`). Install it once
  (`go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest` or your
  package manager) and run:

  ```bash
  golangci-lint run --timeout=5m
  ```

  Enabled linters: `govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused`,
  `revive` (with the `exported` rule on — export-level docs are expected on exported
  symbols), `gosec`, `misspell`, `unconvert`, `bodyclose`. Test files get a slightly
  looser pass (`errcheck`/`gosec` relaxed) for table setups.

- **K8s library versions are pinned together** in `go.mod` (`apimachinery` v0.31.x,
  `controller-runtime` v0.19.x, and `controller-tools`/`controller-gen` v0.16.x share
  a release train). Bump them as a set, never one in isolation.

- Idiomatic Go: standard `testing` + `testify`, table-driven tests, small packages,
  errors wrapped with context. Match the conventions already in `pkg/…`.

### TypeScript / console

Handled by the console toolchain — ESLint + the framework formatter. See
[§8](#8-console-development).

### General

- Keep changes focused. Boring, minimal diffs merge faster than clever, sprawling ones.
- Don't hand-edit generated files (`zz_generated.deepcopy.go`, `config/crd/bases/*`).
- No secrets in the tree — `gitleaks` and Trivy secret scanning run in CI and will
  fail the build ([§5](#5-testing)).

---

## 5. Testing

Tests are organized in four layers (from the testing strategy). You'll mostly touch L1.

| Layer | What it proves | Run it with |
|-------|----------------|-------------|
| **L1 — Feature/functional** | each component's units + integration behave | `go test ./...`, `npm test` |
| **L2 — Concurrency/chaos** | the coordination spine is correct under race/crash/pause | `go test -race -tags=chaos …` (kind + Postgres) |
| **L3 — Performance** | claim latency, warm-pool, SSE throughput hold | dedicated benches |
| **L4 — Security** | images/modules/CVEs clean, no secrets, blast radius bounded | `security.yml` (see below) |

### Go unit + integration tests

```bash
go test ./...

# What CI runs (race detector + coverage), per component:
go test -race -covermode=atomic -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

**Coverage gates:** ≥ **80%** per Go package overall; the coordination-spine package
(`pkg/coord`) is held to ≥ **90%**. CI fails the leg below the gate.

### DB migration self-checks

Every migration ships a companion `*_test.sql` — plain runnable SQL (no framework)
that asserts the schema's structural invariants and wraps itself in a transaction it
`ROLLBACK`s, so it leaves no residue:

```bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
  -f db/migrations/0001_coord_schema.sql \
  -f db/migrations/0001_coord_schema_test.sql
```

### The chaos / concurrency harness (Epic 14 → `spine-chaos.yml`)

The coordination spine (checkout / claim / lease / fencing) is the most
correctness-critical part of the platform, so it gets its own **required** suite that
runs against a real Postgres in a kind cluster with the race detector on. It exercises
named acceptance cases **C1–C7** (parallel claimers, work-pull fan-out, crash-mid-claim
reclaim, stale-holder fencing, zombie-writer-vs-PVC, double-dispatch dedup, idempotent
re-entry):

```bash
# Requires a kind cluster + CNPG Postgres (see the workflow for the exact setup).
go test -race -tags=chaos -run 'TestSpine' ./pkg/coord/... ./internal/coord/... -v
```

This suite is a **required status check for any change to coordination code** — it
cannot be skipped for spine-affecting PRs, and it fails fast if the fence-token column
or the unique-active-claim constraint is missing.

### End-to-end squad scenarios

The E2E lane runs a full Run path (claim → dispatch → shim → agent → artifact →
complete) against a local **Ollama** model, so it costs **zero paid API credits**:

```bash
go test -tags=e2e -run 'TestSquadSmoke' ./test/e2e/... -v
# Console E2E:
cd console && npx playwright test
```

E2E runs nightly, on release tags, and on manual dispatch — not on every PR.

### The one-runnable-check rule

Non-trivial logic lands with **one runnable check** that fails if the logic breaks —
a table-driven `_test.go`, or a migration's `*_test.sql` companion. Trivial one-liners
don't need one. Correctness here is tested, not assumed.

---

## 6. Pull request process

### Branch naming

Branch off `main` using a `type/isi-<ticket>-<short-slug>` shape, e.g.:

```
feature/isi-2191-coord-schema
fix/isi-2339-claim-fence
docs/isi-2357-contributor-guide
ci/github-actions-pipeline
```

Common prefixes: `feature/`, `fix/`, `docs/`, `ci/`, `test/`, `arch/`. Never push to
`main` directly.

### Commit messages — Conventional Commits

We use [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <summary>

<optional body explaining the why>

Signed-off-by: Your Name <you@example.com>
```

Types: `feat`, `fix`, `docs`, `ci`, `test`, `refactor`, `chore`, `build`. Scope is the
area touched (`coord`, `apiserver`, `console`, `security`, …). Real examples from history:

```
feat(coord): add forward-only coord schema migration (Story 2.1, ISI-2191)
fix(coord): address ISI-2339 review findings F1–F6 on the coord schema
ci(security): bump trivy-action to v0.36.0
```

### DCO sign-off

Every commit must carry a **Developer Certificate of Origin** sign-off. Just add `-s`:

```bash
git commit -s -m "feat(coord): add lease renewal"
```

This appends a `Signed-off-by:` trailer certifying you have the right to submit the
work under the project's Apache-2.0 license. Amend a missed sign-off with
`git commit --amend -s`, or a whole branch with `git rebase --signoff main`. See
[DCO.md](./DCO.md) for the full text — the DCO check (`.github/workflows/dco.yml`) is a
**required status check**, so a PR with any unsigned commit won't merge.

### Opening the PR

1. Rebase on the latest `main` and make sure your branch builds and tests green locally.
2. Open a PR against `main`. Describe **what** changed and **why**; link the ISI ticket.
3. CI must be green. On a PR you can expect:
   - **`ci.yml`** — lint · build · test · coverage, per component (operator, apiserver,
     memory, console).
   - **`spine-chaos.yml`** — the C1–C7 chaos suite, **if** your PR touches
     `cmd/apiserver/**`, `pkg/coord/**`, or `internal/coord/**`.
   - **`security.yml`** — govulncheck · npm audit · Trivy fs/config · gitleaks · CodeQL.
   - **`dco.yml`** — every commit carries a `Signed-off-by:` trailer.
4. Address review. Keep the diff scoped to the ticket; sibling cleanups belong in their
   own PR.

Branch protection requires the per-component status checks to pass before merge.

---

## 7. Changing CRD types

CRD Go types live in `api/v1alpha1/`. After editing a `*_types.go` file, **regenerate**
the derived artifacts — never hand-edit them:

```bash
make generate    # regenerates zz_generated.deepcopy.go (DeepCopy methods)
make manifests   # regenerates CRD YAML under config/crd/bases/
```

Both targets download and run `controller-gen` (pinned to `v0.16.5`) into `./bin`
automatically. Commit the regenerated `zz_generated.deepcopy.go` and
`config/crd/bases/*` alongside your type change — CI expects the generated output to
match the types.

Notes:

- The header for generated files comes from `hack/boilerplate.go.txt`.
- Keep `controller-tools` in step with the K8s libs in `go.mod` when you bump versions
  ([§4](#4-coding-standards)).
- CRDs are `v1alpha1` and **not** stable — additive, backward-compatible changes are
  strongly preferred while the API settles.

---

## 8. Console development

The console is a **Next.js / TypeScript** app (plus a BFF) under `console/`, using the
Node 24 runtime. It's the operator UI: dashboard, Kanban/List tickets, build browser,
settings — driven by the apiserver's BFF payloads over one SSE bus, behind a single
RBAC wall.

```bash
cd console
npm ci            # clean install from the lockfile (what CI runs)
npm run dev       # local dev server
npm run lint      # ESLint (a required CI check)
npm test -- --coverage   # Vitest unit tests
npx playwright test      # E2E (semantic, aria-based locators)
```

### Mock-to-implementation workflow

Screens are designed as mocks first, then implemented against those specs:

1. Start from the screen's UX spec / mock and the shared design tokens (breakpoints,
   colors, spacing) — don't hard-code pixel values that a token already defines.
2. Build the screen from the existing component system; **detect, don't impose** the
   scaffolding's choices (Vitest + ESLint; Playwright for E2E; the DnD library the repo
   already uses). Don't add a parallel framework.
3. Cover it with **Vitest units** for component logic and **Playwright E2E** (semantic
   locators) for interaction flows — including the responsive matrix (mobile `<768`,
   tablet `768–1024`, desktop `>1024`) and touch parity (≥44px targets, no hover-only
   affordances) where the screen calls for it.
4. RBAC is enforced server-side; the UI reflects it (e.g. `viewer` sees read-only
   affordances). Assert that in tests — no client-only gating.

If the console app isn't scaffolded yet when you start a screen, coordinate with the
nav-shell story and follow the emerging `console/` conventions rather than inventing new ones.

---

## 9. Documentation changes

Public documentation is plain **Markdown** under `docs/`. Preview it with any Markdown
viewer — your editor's preview, `grip` (`pip install grip && grip docs/some-file.md`)
for GitHub-flavored rendering, or just reading it on the PR. There is no static-site
generator wired up yet; if you add one, document the `preview` command here.

- Keep prose welcoming and concrete. Prefer runnable commands over description.
- The repo's own top-level docs (`README.md`, this file) count — improving them is a
  real contribution.
- **`docs/bmad/` and anything under `bmad/`/`.paperclip/` are gitignored** internal
  planning artifacts — not part of the public docs tree. Don't reference them from
  public docs or expect them in a checkout.

---

## 10. Releasing

_(Maintainers cut releases; this is here so contributors understand the mechanics.)_

- **Versioning.** Releases follow **SemVer** (`vMAJOR.MINOR.PATCH`), cut by pushing a
  `v*` git tag. The CRD API version (`v1alpha1`) is a separate axis and is **not**
  stable pre-v1 — breaking API changes are possible between releases while the platform
  is in early development.
- **Images.** On a `v*` tag, `build-images.yml` builds each component multi-arch
  (`linux/amd64,linux/arm64`), pushes to `ghcr.io/k8squad/ksquad-<component>`, generates
  an SBOM (Syft), scans for CVEs (Trivy, gating on fixable CRITICAL/HIGH), and — for
  releases only — **signs and attests** the image digests with cosign (keyless/OIDC).
  Only digests are signed, never mutable tags.
- **Changelog.** Because commits follow Conventional Commits ([§6](#6-pull-request-process)),
  the changelog is derived from commit history grouped by `feat` / `fix` / etc. Keep
  your commit summaries clean — they become the release notes.

---

## 11. Licensing & governance

- **License.** KSquad is licensed under the [Apache License 2.0](./LICENSE) (see also
  the [NOTICE](./NOTICE) file). All contributions are accepted under that license.
- **Third-party dependencies.** If you add one, record it in
  [LICENSES-third-party](./LICENSES-third-party) and make sure its license is compatible
  (Apache-2.0, MIT, BSD, and similar permissive licenses are fine; avoid copyleft
  without maintainer discussion).
- **Governance.** Project roles and decision-making are described in
  [GOVERNANCE.md](./GOVERNANCE.md); current maintainers are listed in
  [MAINTAINERS.md](./MAINTAINERS.md).
- **Code of Conduct.** All participation is governed by the
  [Code of Conduct](./CODE_OF_CONDUCT.md).

---

## Questions?

Open an issue to discuss substantial changes before you build them — it saves everyone
a round trip. For anything small, just send the PR. Welcome aboard. 🛰️
