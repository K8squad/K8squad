# L1 — Feature / Functional test registry (Story 14.1)

This file is the **auditable map** from the testing-strategy §3.3 *Epic → L1 obligation*
table to the **concrete test files** that satisfy it, per component. It exists so a
reviewer (Testing Architect) can see at a glance that every shipped story's GWT
acceptance criteria has an L1 case — and that every *un*landed component is
**skip-with-reason, never silently omitted** (§3.3, §10.4).

## What "L1" is and where it runs

Testing-strategy §3.1 defines the L1 layer as **each component's units + integration**.
Those two halves run in two lanes, both fanned out from the reusable
[`component-matrix.yml`](../../.github/workflows/component-matrix.yml) primitive
(ISI-2742), so the component list is declared in exactly one place:

| Half | Lane | Command | What runs |
|------|------|---------|-----------|
| **Unit / pure-Go functional** | `ci.yml` → `go / <component>` | `go test -race ./...` (untagged) | reconciler/spine/memory/event units, table-driven, fake client — no services |
| **Integration (service-backed)** | `l1.yml` → `L1 / go integration` | `make l1-integration` = `go test -p 1 -tags=integration,discussion_integration ./...` | real Postgres (pgvector) + NATS/JetStream feature suites |
| **Console** | `l1.yml` → `L1 / node / console` | `make l1-node` (Vitest) | skip-with-reason until `console/package.json` lands |

`make l1` runs all three locally. Each integration suite **SKIPs** when its
`DATABASE_URL` / `MEMORY_TEST_DATABASE_URL` / `NATS_URL` is unset, so the target is
safe to run without services (you get skips, not failures).

## Epic → L1 case mapping (§3.3)

Legend: ✅ landed + covered · ⏭️ skip-with-reason (component not landed) · L2/gate = covered by a different, named lane.

| Epic | Component | L1 obligation (§3.3) | Test file(s) | Lane |
|------|-----------|----------------------|--------------|------|
| 1 CRD Foundation | operator, apiserver | CRD schema validation, admission, API scaffolding | `api/v1alpha1/crd_types_test.go`, `admission_rules_test.go`, `agentruntime_types_test.go`, `groupversion_test.go`, `otelconfig_webhook_test.go`, `internal/webhook/v1alpha1/validator_test.go` ✅ · **envtest** CRD-apply integration ⏭️ (operator controllers not landed — `make setup-envtest` hook ready) | unit |
| 2 Coordination Spine | apiserver `pkg/coord` | claim/renew/reclaim/fence units (+ PG integration feeds L2) | `pkg/coord/resume_test.go`, `frb3_no_chat_contract_test.go` ✅ · claim/fence/dispatch/outbox chaos → **L2 `spine-chaos.yml`** (`-tags=chaos`) · DDL self-checks → **`ci.yml` `db / migrations`** (`db/migrations/000{1,2,3}_*_test.sql`) | unit / L2 / migrations |
| 3 Run reconcile & warm-pool | operator | Run lifecycle transitions, retry/resume, kill; SandboxPool ready-count | `pkg/warmpool/sizing_test.go` ✅ · Run-reconcile transitions ⏭️ (operator controllers not landed) | unit |
| 4 Sandbox & workspace | operator | teardown-and-replace, per-principal PVC scoping | ⏭️ (operator controllers not landed) | — |
| 5 Shims & A2A | shims | Agent Card generation, capability negotiation, deterministic `a2a_task_id` dedup | ⏭️ (shim entrypoints `cmd/shim-*` not landed — `shim / <runtime>` legs skip-with-reason) | — |
| 6 Memory service | memory | MCP tool surface, pgvector search, provenance on read | `internal/memory/integration_test.go` (`-tags=integration`, `MEMORY_TEST_DATABASE_URL`) ✅ · `internal/index/attribution_test.go` ✅ | integration / unit |
| 7 Credentials & pause/resume | operator, shims | credential injection (never logged), Paused→resume | ⏭️ (operator/shims not landed) | — |
| 8 Console | console + apiserver `pkg/search` | UI/BFF Vitest units + E2E; BFF authZ; sub-ticket tree; dual-view board; global search; responsive matrix | `console/e2e/auth/a5-password-reset.spec.ts` (Playwright) ✅ · **Vitest** BFF/tree/board/search units ⏭️ (no `console/package.json` yet — §3.2) | node (skip) |
| 10 Discussion rooms | apiserver, memory | discussion schema, memory-projection, not-a-coordination-channel guard | `internal/discussion/store_test.go`, `handler_test.go` ✅ · `internal/discussion/integration_test.go` (`-tags=discussion_integration`, `DATABASE_URL`) ✅ · `db/migrations/0004_discussion_schema_test.sql` | unit / integration / migrations |
| 11 Source-control sync | operator, apiserver | repo-sync reconciler, webhook ingress, mirror mapping | `internal/webhook/attribution_test.go`, `attribution_crd_test.go` ✅ (webhook ingress) · repo-sync reconciler ⏭️ (operator not landed) | unit |
| 12 Plugin architecture | apiserver `pkg/events` | outbox transactional append; delivery retry/dead-letter/circuit-breaker; read-only observer guard | `pkg/events/capture_test.go`, `relay_test.go`, `subject_test.go` ✅ · `pkg/events/integration_test.go` (outbox C1/C2/C4, `DATABASE_URL`), `pkg/events/jetstream/integration_test.go` (relay at-least-once, `NATS_URL`) both `-tags=integration` ✅ | unit / integration |

### Deliberately-separate gates (documented so they are not mistaken for omissions)

- **Auth session / RBAC** (`pkg/auth/a5_authsession_contract_test.go`, `a5_password_reset_test.go`): auth-service unit layer per §6.7 — runs in the unit lane; the full RBAC matrix / cross-user isolation cases are **absorbed into L4** (`14.4`, §6.7 / story 15.8).
- **Build-browser KPIs** (`internal/observability/buildbrowser/*_test.go`, `-tags=kpi`): the Epic 8.7c observability acceptance step — the files themselves state *"do NOT add `-tags kpi` to the default CI gate"*. Runs once ISI-2168 instrumentation lands, not in L1.
- **Perf** (`pkg/perfgate/gate_test.go`, `pkg/*/perf_bench_test.go`): **L3** (`14.3`, `perf.yml`).
- **Coordination-spine chaos** (`pkg/coord/*_chaos_test.go`, `-tags=chaos`): **L2** (`spine-chaos.yml`).

## Adding an L1 case

1. Put pure-Go / fake-client feature tests in the component's package with **no build tag** — they join the unit lane automatically.
2. Put service-backed feature tests behind `//go:build integration` (Postgres/NATS) or `//go:build discussion_integration`, and make them **SKIP when their DSN/URL env is unset** (mirror the existing suites). `l1.yml` provisions the services and sets the env.
3. When operator controllers land, add controller-integration tests behind `//go:build envtest`, add `make setup-envtest` to the `l1.yml` go-integration job, and export `KUBEBUILDER_ASSETS` — then flip the Epic 1/3/4 ⏭️ rows to ✅ here.
