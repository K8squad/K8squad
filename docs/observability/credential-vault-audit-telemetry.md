# Credential Vault — Audit & Telemetry (WS-9 S7)

**Issue:** ISI-5820 (WS-9 S7). **Parent:** ISI-5777 Credential Vault (design
`docs/bmad/features/isi-5777-credential-vault-design.md`, threat-model **T7**).
**Owner:** Observability.

This document is the audit/telemetry contract for the three mutating credential-
vault verbs — **write (create), rotate, delete** — materialized in
`internal/apiserver/secretwrite.go` (S2 ISI-5815, S3 ISI-5816). It pairs with the
companion artifacts in this directory:

- `credential-vault-slos.yaml` — test-consumable KPI/alert definitions (DQL +
  threshold + comparator), for the Testing Architect and release gates.
- `credential-vault-audit-dashboard.json` — Dynatrace dashboard-as-code
  (import via the Dashboards app or `dtctl`).

> **Scope note.** The run-time admission signal `ksquad.credential.gate_check`
> (S5, ISI-5850) is a *separate* span family documented in its own spec; this
> file covers only the HTTP mutating ops on `/api/credentials`.

---

## 1. The guarantee (acceptance)

1. **Every mutating op emits an audit span with a masked payload** — the span
   carries the non-secret tuple *(actor, credential type, project, namespace,
   outcome)* and nothing else that identifies *who* did *what* to *which*
   credential.
2. **No secret value appears in any span, log record, or metric** — not on the
   success path, not on any error branch. The credential material rides only
   inside `Secret.Data`, never through telemetry (design NFR-2 / §5.3).
3. **Dashboards and alerts are documented** — this file + the two companions.

---

## 2. Telemetry surface

### 2.1 Audit spans (one per mutating op)

| Verb | HTTP | Span name |
|------|------|-----------|
| write (materialize) | `POST /api/credentials` `{type}` | `ksquad.credential.create` |
| rotate | `PUT /api/credentials` | `ksquad.credential.rotate` |
| delete | `DELETE /api/credentials` | `ksquad.credential.delete` |

Each span carries the **audit tuple** as span-only attributes (stamped in the
single shared resolution seam `resolveToolCredTarget`, so the three verbs can
never drift):

| Attribute | Example | Meaning |
|-----------|---------|---------|
| `credential.principal` | `user:alice` | **actor** — the authenticated caller |
| `credential.type` | `github` | credential type from the binding registry |
| `credential.project` | `widget` | canonical Project name (not raw path input) |
| `k8s.namespace.name` | `ksquad-team-beta` | namespace the Secret lives in |
| `team.id` | `2222…` | caller's Team UID |
| `outcome` | `created` | bounded terminal outcome (see §2.3) |

These are **unbounded identifiers → SPAN attributes ONLY**, never metric labels,
matching the `team.id` discipline documented in `obsfunnel.go`. The credential
**value is absent from every attribute** — it is never read back on the write
path and never resolved on rotate/delete.

### 2.2 Audit log line (structured, keyed by principal + type + ns)

Each verb emits a structured `slog` record on success and on every forbidden/
store-error branch, keyed by `namespace`, `name`, `type`, `project`, `principal`
— **never `value`**. On a `Forbidden` the line is an operator runbook pointer
(`check apiserver-rbac.yaml grant + admission policy`). The otelslog bridge
exports these as OTel log records, so they correlate with the span by trace id.

### 2.3 Metrics (bounded `outcome` enum)

| Metric | Verb |
|--------|------|
| `ksquad.credential.create.requests` | write |
| `ksquad.credential.rotate.requests` | rotate |
| `ksquad.credential.delete.requests` | delete |

Single label **`outcome`**, a closed per-verb enum (from `obsfunnel.go`):

| Outcome | HTTP | Meaning |
|---------|------|---------|
| `created` / `rotated` / `deleted` | 201/200 | success terminal |
| `unauthenticated` | 401 | no principal at the choke point |
| `invalid` | 400/422 | decode/validation/unknown-type |
| `no_namespace` | 404 | project/tenancy unresolved (existence-hiding) |
| `conflict` | 409 | create: already exists / rotate: delete-in-progress |
| `not_found` | 404 | delete: no managed credential of this type (existence-hiding) |
| `rejected` | 502 | **RBAC/admission forbade the write** — drift signal |
| `store_error` | 502 | credential store (kube) unavailable |

`rejected` and `store_error` are the two actionable failure outcomes and drive
the alerts in §4.

---

## 3. No-value-leakage verification

The guarantee is enforced in CI, not by inspection:

- `internal/apiserver/obsfunnel_test.go` →
  **`TestNFR2ToolCredentialAuditSpansNoLeak`** drives a full
  `create → rotate(fresh value) → delete` lifecycle through an in-memory span
  exporter **and** the otelslog log bridge, then asserts:
  - *positive controls*: all three `ksquad.credential.*` spans are captured and
    carry the audit-tuple attribute keys **and** their non-secret values
    (`user:alice`, `github`, `widget`, `ksquad-team-beta`) — so the sweep cannot
    pass vacuously;
  - *NFR-2*: **neither** the original value (`toolCredCanary`) **nor** the
    rotated value (`toolCredRotatedCanary`) appears in any span or log record.
- `TestNFR2SecretNeverInTelemetry` does the same for the paste-key create path
  (ISI-3669); `TestNFR2CredentialTestSecretNeverInTelemetry` for the test-probe
  path. Together the three sweeps cover every credential-bearing endpoint.

A regression that stamped a value onto a span attribute or into a log line turns
these tests red.

---

## 4. Alerts (documented; see `credential-vault-slos.yaml` for DQL)

| Alert | Condition | Why | Runbook |
|-------|-----------|-----|---------|
| **Credential write rejected** | `sum(outcome==rejected)` over 5m `> 0` on any of the three metrics | apiserver SA lost its `secrets:create`/`delete` grant, or the ValidatingAdmissionPolicy is denying the managed-credential write — the vault is failing closed | the matching `slog` ERROR names the file: re-apply `deploy/helm/ksquad/templates/apiserver-rbac.yaml`; cf. RBAC-drift recurrences ISI-5765/5767 |
| **Credential store errors** | `sum(outcome==store_error)` rate over 5m above baseline | kube API / credential store unavailable — writes and rotations are dropping | check apiserver → kube-apiserver connectivity |
| **Audit-trail gap** | count of `ksquad.credential.*` spans `<` count of successful mutations (`created+rotated+deleted`) over 1h | a mutating op completed without emitting its audit span — an audit hole | should be impossible by construction (span + metric recorded in one `funnelOutcome` call); a gap means the tracer provider is mis-wired in the deployed env |

Alerts use the Davis **ui** analyzer style from the ISI-5488 dashboard work (see
the `meo7740d` tenant notes) — static-threshold metric events on the bounded
`outcome` dimension, not auto-baselining on unbounded identifiers.

---

## 5. Dashboard (documented in `credential-vault-audit-dashboard.json`)

24-column grid, import-ready template (no tenant IDs). Tiles:

1. **Audit trail** — `fetch spans | filter` the three span names → table of
   *timestamp, verb, principal, type, project, namespace, outcome*. This is the
   human-readable "who changed which credential" log, value-free by contract.
2. **Mutations by verb & outcome** — stacked `timeseries … by:{outcome}` over
   the three counters.
3. **Rejected / store_error (SLO = 0 rejected)** — single-value tiles with
   red/green thresholds, the alert conditions made visible.
4. **Rotations vs deletes** — operational volume for capacity/behaviour review.

---

## 6. Validation status (honesty)

The DQL in the companion files reuses the exact `timeseries` / `filter` /
`fetch spans` grammar already **live-validated against the DT dev tenant**
(`oat05854.dev.apps.dynatracelabs.com`) for the sibling github-sync dashboard
(`github-sync-health-dashboard.json`). The credential metrics/spans render only
once an apiserver carrying S2/S3 is deployed to the DT-exporting env **and**
real vault traffic flows; until then every query returns empty, which a harness
must treat as **not yet measurable**, not pass/fail — identical to the
github-sync KPI posture.
