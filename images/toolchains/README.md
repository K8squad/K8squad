# Toolchain images (`ghcr.io/k8squad/toolchains/*`)

Image factory for the default Toolchain catalog (ISI-3413). Every entry in
`config/helm/values.yaml` → `tools.defaultCatalog.entries` references an image under
`ghcr.io/k8squad/toolchains/` — this directory builds and publishes all of them.

## Layout

- `matrix.json` — **single source of truth** for the tool set: catalog tag, Dockerfile
  path, and pinned upstream version per tool. The
  [`toolchain-images.yml`](../../.github/workflows/toolchain-images.yml) workflow fans
  its build matrix out from this file.
- `<tool>/Dockerfile` — minimal binary-staging image per tool. Two patterns:
  - **static binary staged from upstream** (kubectl, gh, dtctl, helm, yq: download the
    pinned release artifact; go, node, uv: `COPY --from` the official image)
  - **apk package on alpine 3.21** (git, jq, curl, make)
  - **rebase of an official minimal image** (python → `python:3.12-alpine`,
    docker-cli → `docker:27-cli`)

`docker-cli` ships the **client only** — the daemon stays the existing dockerd sidecar.

## Tag semantics

`ghcr.io/k8squad/toolchains/<tool>:<version>` — `<version>` is the **catalog tag** from
`values.yaml` and tracks an upstream *minor line* (e.g. `kubectl:1.31`); the exact patch
is pinned in `matrix.json` `buildArgs` (e.g. `KUBECTL_VERSION=v1.31.14`). apk-based tools
track the alpine 3.21 package (a floor for the catalog minor). A `sha-<commit>` tag is
published alongside for traceability; the ADR (ISI-3283) reproducibility form is the
digest pin in `values.yaml`, applied after each publish.

> **dtctl:** catalog tag `1.0` tracks the upstream `dynatrace-oss/dtctl` 0.x line —
> upstream has not cut a 1.0 release (v0.38.0 at authoring). Revisit when it does.

## Publishing

Publishes (multi-arch amd64+arm64, signed) happen on:

- push to `main` touching `images/toolchains/**`
- weekly scheduled rebuild (base-image/apk CVE refresh)
- manual `workflow_dispatch`

PRs touching this directory build all images (no push/sign) as a smoke gate.

Every publish: SBOM (Syft) → Trivy gate (CRITICAL/HIGH fixable, curated `.trivyignore`)
→ cosign keyless sign + SBOM attest → SLSA build provenance — same posture as
`build-images.yml`.

## Adding a tool

1. `mkdir <tool>` and write a minimal Dockerfile (patterns above).
2. Add one row to `matrix.json` (`tool`, `version` = catalog tag, `dockerfile`,
   `context`, `buildArgs` pinning the upstream patch).
3. Add the catalog entry in `config/helm/values.yaml`.
4. After the first publish, digest-pin the entry (`@sha256:…`).
