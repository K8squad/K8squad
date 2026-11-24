# Instrumentation Score — deployment & operations notes

How the instrumentation-score conformance pipeline (ISI-7741 plan, phases
1–5 = [ISI-7747 → ISI-7751]) is deployed and verified. The scoring engine and
its $0 offline lane are documented in `pkg/telemetry/instscore/README.md`;
this page covers the **live** path: collector tap → apiserver ingestion →
Postgres → read API → console.

```
workloads ──OTLP──▶ OTel gateway ──redaction/processors──┬──▶ vendor backend (Dynatrace)
                                                          └──▶ otlphttp/instscore tap
                                                                  │ OTLP/HTTP + bearer token
                                                                  ▼
                              apiserver /api/instscore/v1/{traces,metrics,logs}
                                  └─ internal/instscoreingest → Postgres (migration 0042)
                                        └─ GET /api/v1/instscore/services* → console /instscore
```

Design invariants worth knowing before touching config:

- **The tap is a side channel, not a critical path.** It is a second exporter
  on the gateway that mirrors the *same redacted, post-processor* batch the
  vendor backend receives. As an exporter it sits after
  `k8sattributes`/resource processors and downstream of redaction *by
  construction*. It uses bounded retry (drain across an apiserver brownout)
  and file-backed queueing so a slow apiserver never blocks the vendor path.
- **Report-only.** v1 scores and reports; it enforces nothing.
- **Fail-closed surfaces.** Without `KSQUAD_INSTSCORE_TOKEN` the apiserver
  logs `instscore seam disabled` and simply does not mount the ingestion
  route; the read endpoints answer a documented 501 (not a silent 404 shape
  change).

## Components

| Piece | Where | Notes |
|-------|-------|-------|
| Vendored rules | `third_party/instrumentation-score/` | Pin `ef080a8a`; upgrade = re-vendor + fix the `rulesync` test. |
| Scoring engine | `pkg/telemetry/instscore/` | Pure/deterministic; shared by CLI and live ingestion. |
| Offline CLI + CI | `cmd/instscore`, `.github/workflows/instscore.yml`, `make instscore` | $0 lane; fixture scores (100 / 17.14 / 61.43) are assertions. |
| Gateway tap | `deploy/otel/chart` (`collector.instscore.*`) | `otlphttp/instscore` exporter + auth header from a Secret. |
| Ingestion | `internal/instscoreingest` | OTLP/HTTP (protobuf or JSON, gzip ok), folds batches into per-service evidence windows and refreshes verdicts. |
| Store | `db/migrations/0042_instscore_ingest.sql` | Schema `instscore`: `evidence_window`, `rule_verdict`, `score_history`. |
| Read API | `internal/apiserver/instscoreread.go` | `GET /api/v1/instscore/services` (list) and `.../services/{name}` (rule-by-rule verdicts + remediation guidance + score history). |
| Console | `console/app/(app)/instscore` | Nav → **Instrumentation** (`/instscore`); server-side data via `console/app/api/instscore/*`. |

## Enable it

Prereq: the shared database has run migration `0042` (see the
[shared-db migration runbook](../runbooks/shared-db-migration.md) for the
migration contract and deploy-skew guards).

1. **Apiserver token.** Set `KSQUAD_INSTSCORE_TOKEN` on the apiserver
   deployment (helm `config/helm` control-plane values or your overlay). A
   non-empty value mounts `/api/instscore/v1/{traces,metrics,logs}` and wires
   the SQL read model; empty disables both (read side returns the documented
   501).

2. **Collector-side secret.** In the namespace the OTel chart installs into
   (default `observability`), create the token Secret with the **same value**:

   ```bash
   kubectl -n observability create secret generic instscore-ingest \
     --from-literal=token="$(openssl rand -hex 32)"
   # then set KSQUAD_INSTSCORE_TOKEN on the apiserver to the SAME value
   ```

   The token is compared in constant time against a SHA-256 digest on both
   sides; it never appears in chart values.

3. **Chart values.** In `deploy/otel/chart/values.yaml` (or a `-f` overlay):

   ```yaml
   collector:
     instscore:
       enabled: true   # adds otlphttp/instscore to every pipeline's exporters
       endpoint: "http://ksquad-apiserver.ksquad-system.svc/api/instscore"
       authSecretRef:
         name: instscore-ingest
         key: token
   ```

   The default endpoint targets the ksquad chart's apiserver Service
   (`<fullname>-apiserver.<ns>.svc`, port 80) — keep it in lockstep if you
   rename either release.

4. **(Re-)install the chart.**

   ```bash
   helm upgrade k8squad-otel deploy/otel/chart -n observability
   ```

   The tap renders in both `operator` and `deployment` modes. In `external`
   mode the chart renders no gateway, so there is no tap — mirror batches
   from your own collector to the same ingestion route if you want scoring
   there (the route is the contract, the tap is just wiring). A full rendered
   example lives in `deploy/otel/reference/gateway-collector.yaml`.

## Verify

```bash
# 1. Ingestion route is up and auth-gated (expect 401 without the token):
curl -s -o /dev/null -w "%{http_code}\n" \
  http://ksquad-apiserver.ksquad-system.svc/api/instscore/v1/traces

# 2. Read surface lists scored services (through the apiserver's normal auth):
curl -s http://ksquad-apiserver.ksquad-system.svc/api/v1/instscore/services | jq .

# 3. One service, rule-by-rule verdicts + guidance + score history:
curl -s http://ksquad-apiserver.ksquad-system.svc/api/v1/instscore/services/payments | jq .

# 4. Console: open /instscore (nav: Instrumentation).

# 5. Offline sanity on a captured dump (no cluster needed):
go run ./cmd/instscore some-dump.json
```

From the gateway side, `kubectl logs` on the collector shows
`otlphttp/instscore` exporter errors if the token mismatches (401s) — a
value mismatch between the Secret and `KSQUAD_INSTSCORE_TOKEN` is the
usual suspect.

## Operations notes

- **Storage growth.** Evidence windows and score history accumulate in the
  `instscore` schema; `evidence_window` is indexed by
  `(service_name, window time)` and `received_at` for the read queries.
  Retention/pruning of old windows is not automated in v1 — size accordingly
  or prune out-of-band.
- **Upstream rule updates.** Bump the vendored pin in
  `third_party/instrumentation-score/`, then fix
  `pkg/telemetry/instscore/rules.go` and `guidance.go` until
  `go test ./pkg/telemetry/instscore/...` (the `rulesync` test) is green —
  drift is designed to surface as a visible test failure.
- **CI.** `.github/workflows/instscore.yml` (self-hosted runner) runs the
  engine tests, the CLI over fixtures, and asserts the fixture scores. Docs
  changes don't affect it; engine changes must keep the deterministic scores.
