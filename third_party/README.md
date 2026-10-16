# third_party/k8sagentmesh — vendored K8sAgentMesh module

This directory vendors the `github.com/K8sAgentMesh/K8sAgentMesh` Go module
(the generic AgentMesh surface lifted out of k8squad — roadmap P3, AgentMesh
chapter §1A/§9.3, tracking issue ISI-5827) so k8squad consumes it through a
`replace` directive that needs no network credentials in CI:

    replace github.com/K8sAgentMesh/K8sAgentMesh => ./third_party/k8sagentmesh

Why a directory replace and not a version pin: the upstream repository
(github.com/K8sAgentMesh/K8sAgentMesh) is currently **private**, and this
repository is public with no private-module auth wired into CI — a version
pin would be unfetchable for every CI lane and every external contributor.
The vendored copy contains only code already public in this repository
(`pkg/a2a`, lifted nearly verbatim), so nothing is published by vendoring it
here.

## Provenance

- Source: https://github.com/K8sAgentMesh/K8sAgentMesh
- Pinned commit: `2454c8bf036bc035b88ba9ad4d4ba4817be762f5`
  (pseudo-version `v0.0.0-20261016133232-2454c8bf036b`, kept in `go.mod`'s
  `require` line as provenance for the pin)
- Synced: 2026-10-16 (ISI-6035, P3.2 — the a2a package lift)

## Sync

The module is canonical in its own repository; this copy is a build
convenience only. Re-sync after a module change:

    rsync -a --delete --exclude .git --exclude .github --exclude hack \
      /path/to/K8sAgentMesh/a2a/ third_party/k8sagentmesh/a2a/
    cp /path/to/K8sAgentMesh/{doc.go,go.mod,LICENSE,NOTICE} third_party/k8sagentmesh/

Then update the `require` line in `go.mod` to the synced commit's
pseudo-version and the provenance block above.

## Removal (P3.7 cutover)

At the P3.7 cutover (ISI-6040) this vendored copy is deleted and the
`replace` directive is swapped for a real version pin (module tag `v0.x`),
once the upstream repository is public. Until then, do NOT edit files
inside this directory directly — fix the module upstream and re-sync.
