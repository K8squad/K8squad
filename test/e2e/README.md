# `test/e2e` — Run-path conformance harness (`TestSquadSmoke`)

The live end-to-end proof of the squad **Run plane**. It is the deliverable of
**ISI-3475 / ISI-2114** and the precondition the QA gate
[ISI-3290](../../) depends on — **AC-2** and the in-Run half of **AC-4**.

## What it proves

On a kind cluster with the operator + default toolchain catalog installed, it
drives **one live Run** that requires a version-pinned `kubectl` + the
`github-mcp` MCP server and asserts the five conformance properties:

| # | Property | How it is asserted |
|---|----------|--------------------|
| 1 | `kubectl` **version-pinned on PATH** in the sandbox | `command -v kubectl` + `kubectl version --client` inside the Run pod, and `status.capabilityManifest.toolchains` |
| 2 | **Scoped MCP config** for `github-mcp` only | exactly one `status…mcpEndpoints`, effective allow set non-empty, secret header NOT inlined, credential by `credentialSecretRef`; rendered config in-pod names only that server and inlines no token |
| 3 | **Credentials mounted** from the referenced Secret | pod spec mounts the Secret as volume / `envFrom` / `valueFrom` |
| 4 | **Egress honored** | in-sandbox probes: arbitrary dst refused, allowlisted upstream reachable via the team proxy (mirrors [`test/blast-radius/cases/s4-1`](../blast-radius/cases/s4-1-egress-default-deny.sh)) |
| 5 | **tool / MCP / skill spans + metrics** emitted during the Run | scrape the OTel sink `/metrics` and aggregate with `internal/apiserver` (mirrors [`internal/a2a/telemetrysink_e2e_test.go`](../../internal/a2a/telemetrysink_e2e_test.go)) |

The kubectl pin is **resolved from the installed catalog at runtime**
(preferring `1.31`, falling back to the catalog's declared version; override with
`KUBECTL_TOOLCHAIN_VERSION`). The invariant under test is *"the pinned version
resolves and lands on PATH"* — version-pinned, not a specific magic number.

## Running it

```sh
# Against whatever cluster your KUBECONFIG points at (kind, in-cluster, …):
go test -tags=e2e -run 'TestSquadSmoke' ./test/e2e/... -v
```

The harness is gated behind the **`e2e` build tag** — the untagged unit lane
(`go test ./...`) never compiles or runs it (`?  test/e2e  [no test files]`).

Optional environment knobs:

| Env | Default | Purpose |
|-----|---------|---------|
| `KUBECTL_TOOLCHAIN_VERSION` | prefer `1.31`, else catalog pin | force a specific kubectl pin (must be in the catalog) |
| `S4_ARBITRARY_EGRESS_IP` | `1.1.1.1` | the "must be refused" egress target (shared with s4-1) |
| `MCP_CONFIG_PATH` | probes a candidate set | exact in-pod path of the rendered MCP config |
| `OTEL_SINK_SERVICE` / `_NAMESPACE` / `_PORT` / `_PATH` | unset / `e2e-squad-smoke` / `9090` / `/metrics` | OTel sink `/metrics` location for the telemetry subtest |

## Skip-with-reason, never silently dropped

Following the repo-wide L1/E2E convention (§3.3, §10.4,
[`test/l1/README.md`](../l1/README.md), and `e2e.yml`'s own skeleton gates),
every precondition the harness cannot satisfy yet surfaces as a `t.Skip` with a
precise reason rather than a silent pass or a hard failure:

- **no reachable cluster** → skip (the `e2e-ollama` lane provisions kind).
- **operator CRDs absent** → skip (`ksquad.io` Run CRD not registered).
- **default catalog absent** → skip (no `kubectl` Toolchain).
- **Run never resolves its capability manifest** → skip (operator Run-assembly
  not driving — the operator Deployment is not chart-deployed yet).
- **sandbox pod / OTel sink not up** → the tier-2 subtests skip individually,
  while the tier-1 capability-manifest assertions still run.

So the harness runs **green-and-partial** and fills in as the operator, the
sandbox dispatch, and the OTel sink land — nothing is ever silently omitted.

## CI wiring

`TestSquadSmoke` is the `test/e2e-conformance` half of the `e2e.yml`
`e2e-ollama` lane skip-guard. That half clears now that this directory exists;
the lane flips to `present=true` once the opencode shim (Story 5.8) also lands.
When present, the lane installs the CRDs + default toolchain catalog via
`config/helm` and runs this test. The operator Deployment install (and the OTel
sink) land with the operator chart wiring in a later epic — until then the live
sandbox/telemetry assertions skip-with-reason as described above.
