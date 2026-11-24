# pkg/telemetry/instscore — instrumentation-score conformance engine

Scores observed workload telemetry against the
[instrumentation-score/spec](https://github.com/instrumentation-score/spec)
rule set. The rules are vendored (pin `ef080a8a`, see
`third_party/instrumentation-score/README.md`); this package is the pure,
deterministic engine that turns OTLP evidence into per-service verdicts and a
0–100 score. It is the telemetry twin of the A2A `conformance/` suite: no
clocks, no I/O, no randomness — the same batches folded in the same order
produce the same result, so scores are CI-diffable (ISI-7741 plan; engine
ISI-7748).

## The score

```
score = Σ(pass_i × weight_i) / Σ(evaluated_i × weight_i) × 100
```

- Impact weights: Critical 40 · Important 30 · Normal 20 · Low 10.
- Qualitative bands (`Categorize`): 90–100 Excellent · 75–89 Good ·
  50–74 Needs Improvement · 0–49 Poor.

**No-data policy.** A rule whose target signal is entirely absent from the
window **fails** with evidence `no <signal> observed in window` — the
specification has no N/A, and the conservative default keeps scores
comparable across implementations. A metrics-only service therefore fails
the LOG/SPA rules (see the `no_data_metrics_only.json` fixture, 61.43), not
quietly dodges them.

**`not_evaluated` is different**: it marks a rule the implementation does
not implement — the spec-mandated disclosure — and such rules are excluded
from *both* sums (they never drag the score down). All 26 vendored rules
have evaluators here, so the status appears only in the report shape, kept
for spec fidelity.

Every rule verdict carries a human-readable `Evidence` string naming the
offending attribute/span/log/metric, so a FAIL is actionable without re-running
anything.

## Package layout

| File | Role |
|------|------|
| `rules.go`     | Registry of the 26 vendored rules (ID, impact, category) — the single Go-side list, kept in sync with `third_party/` by `rulesync_test.go`. |
| `window.go`    | `Window`/`Accumulator`: folds OTLP export requests into per-`service.name` evidence windows (spans, log records, metric data points, each linked to its resource). |
| `eval.go`      | Rule evaluators: one function per rule against a `Window`. |
| `score.go`     | The spec formula, `Verdict`, `Result`, `Categorize`. |
| `semconv.go`   | Local tables of the semantic-convention names/units the evaluators reference (no external semconv dependency). |
| `sensitive.go` | Sensitive-data detection shared by LOG-003 / MET-008 / RES-010. |
| `guidance.go`  | Rule descriptions + failed-rule remediation guidance, pinned verbatim to the vendored rules (drift = visible test failure, never a silent guidance change). |
| `dump.go`      | Loads offline OTLP/JSON dumps (collector wire format) into an `Accumulator`. |
| `testdata/`    | Pinned fixtures: `good_service.json` (100), `violations.json` (17.14), `no_data_metrics_only.json` (61.43). The fixture scores are CI assertions, not approximations. |

## Offline CLI (the $0 lane)

`cmd/instscore` scores one or more OTLP JSON dumps exactly as an operator
would on a collector capture:

```bash
go run ./cmd/instscore pkg/telemetry/instscore/testdata/good_service.json
go run ./cmd/instscore -service payments dump.json   # one service
go run ./cmd/instscore -json dump.json               # machine-readable
make instscore                                        # race tests + all fixtures
```

Exit code 0 means the dump was scored — failing rules are findings in the
data, not tool failures (v1 is report-only). Non-zero means usage/I/O/parse
failure. CI runs the lane in `.github/workflows/instscore.yml`, the twin of
`conformance.yml`: no live collector, no network, no paid credential.

## Live pipeline (overview)

```
workloads ──OTLP──▶ OTel gateway ──redaction/processors──┬──▶ vendor backend (Dynatrace)
                                                          └──▶ otlphttp/instscore tap
                                                                  │ OTLP/HTTP + bearer token
                                                                  ▼
                              apiserver /api/instscore/v1/{traces,metrics,logs}
                                  └─ internal/instscoreingest → Postgres (migration 0042)
                                        └─ GET /api/v1/instscore/services* → console /instscore
```

The engine is shared verbatim between the lanes: ingestion folds live batches
through the same `Accumulator`/`Evaluate` path the CLI uses on dumps.
Deployment, token wiring, and verification steps live in
`docs/observability/instrumentation-score.md`.
