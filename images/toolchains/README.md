# Toolchain images (`ghcr.io/k8squad/toolchains/*`)

Image factory for the default Toolchain catalog (ISI-3413). Every entry in
`config/helm/values.yaml` → `tools.defaultCatalog.entries` references an image under
`ghcr.io/k8squad/toolchains/` — the pipeline wired here builds and publishes all of
them.

## Layout

- **`Dockerfile.toolchain-<tool>`** (repo root) — minimal binary-staging image per
  tool, same naming convention as the platform `Dockerfile.<component>` images
  (`Dockerfile.operator`, `Dockerfile.apiserver`, …). Build context is the repo root.
  Three patterns:
  - **static binary staged from upstream** (kubectl, gh, dtctl, helm, yq: download the
    pinned release artifact; go, node, uv: `COPY --from` the official image)
  - **apk package on alpine 3.21** (git, jq, curl, make)
  - **rebase of an official minimal image** (python → `python:3.12-alpine`,
    docker-cli → `docker:29-cli`)
- **`images/toolchains/matrix.json`** (this directory) — build detail per tool:
  Dockerfile path, context, and pinned upstream patch (`buildArgs`). The canonical
  tool **list** (name + catalog tag) is declared in
  [`component-matrix.yml`](../../.github/workflows/component-matrix.yml) (the
  `toolchains` family, ISI-2742 single-source-of-truth pattern); the
  [`build-images.yml`](../../.github/workflows/build-images.yml) plan leg joins the two
  and fails on drift in either direction.

`docker-cli` ships the **client only** — the daemon stays the existing dockerd sidecar.

## Tag semantics

`ghcr.io/k8squad/toolchains/<tool>:<version>` — `<version>` is the **catalog tag** from
`values.yaml` and tracks an upstream *minor line* (e.g. `kubectl:1.36`); the exact patch
is pinned in `matrix.json` `buildArgs` (e.g. `KUBECTL_VERSION=v1.36.4`). apk-based tools
track the alpine 3.21 package (a floor for the catalog minor). A `sha-<commit>` tag is
published alongside for traceability; the ADR (ISI-3283) reproducibility form is the
digest pin in `values.yaml`, applied after each publish.

> **dtctl:** catalog tag `1.0` tracks the upstream `dynatrace-oss/dtctl` 0.x line —
> upstream has not cut a 1.0 release (v0.38.0 at authoring). Revisit when it does.

## Publishing

The toolchain lane lives inside
[`build-images.yml`](../../.github/workflows/build-images.yml) (job `build-toolchain`),
not a separate workflow. Publishes (multi-arch amd64+arm64, signed) happen on:

- push to `main` that touches a `Dockerfile.toolchain-*`, `images/toolchains/
  matrix.json`, `build-images.yml`, or `component-matrix.yml` (the plan leg diffs the
  push range; unrelated main pushes skip the lane so 14 multi-arch legs don't starve
  the self-hosted fleet)
- release tags (`v*`) — full set
- manual `workflow_dispatch` — full set (first-publish path for a new tool)

Every publish: SBOM (Syft) → Trivy gate (CRITICAL/HIGH fixable, curated `.trivyignore`)
→ Grype cross-check → cosign keyless sign + SBOM attest → SLSA build provenance. Unlike
the platform lane (which gates Grype/sign/SLSA on `v*` tags), the toolchain lane runs
them on every publish: the rolling catalog tag IS the release artifact — there are no
git tags for toolchain images.

## Adding a tool

1. Write `Dockerfile.toolchain-<tool>` at the repo root (patterns above).
2. Add one row to `matrix.json` (`tool`, `version` = catalog tag, `dockerfile`,
   `context: "."`, `buildArgs` pinning the upstream patch).
3. Add the tool to the `toolchains` family in
   [`component-matrix.yml`](../../.github/workflows/component-matrix.yml) (same `tool`
   name + `version` — the build-images plan leg fails on drift).
4. Add the catalog entry in `config/helm/values.yaml`.
5. After the first publish, digest-pin the entry (`@sha256:…`).
