# Platform-E2E harness — BMAD squad deploy + crew builds todo app

`hack/platform-e2e-bmad.sh` is the scripted apply-and-assert harness for the K8squad
happy path (ISI-3560, parent ISI-3559). It deploys the BMAD squad **entirely via CRDs**,
wires the toolchain catalog, creates a Project, and drives the crew to build a real
todo/post-it app ticket-driven through BMAD phases — one PR per story. It is the
functional counterpart to the ISI-3534 UI Playwright suite (which drives the Console);
this one drives the **operator + Run pipeline**.

**Source of truth** for phases, success criteria and terminology is the ISI-3559 scenario
document (key `scenario`). Read it before running.

## What it does — Phases 0–6

| Phase | Assertion | Success criterion |
|-------|-----------|-------------------|
| 0 Preflight | cluster reachable, operator Available, toolchain catalog present, credential Secrets exist | gates the run |
| 1 Deploy squad | apply `examples/bmad-team` (minus plaintext creds) with `repo.url` → `TARGET_REPO_URL` | — |
| 2 Reconcile | all squad objects `Ready=True`, 0 degraded | **SC-1** |
| 3 Crew + toolchain | agents runnable, `bmad`+`github` skills + toolchain refs resolve | **SC-2** |
| 4 Story → PR | seed backlog + drive ≥1 BMAD story to a PR | **SC-3** |
| 5 App buildable | ≥1 landed PR with buildable code | **SC-4** |
| 6 Report | per-phase PASS/FAIL/BLOCKED table + artifacts | **SC-5**, **SC-6** |

Minimum bar = SC-1..SC-3 PASS **and** SC-4 PASS for ≥1 story. Partials (e.g. SC-1/SC-2
pass, SC-3/SC-4 blocked) are reported honestly — **never faked green**.

## Params (no secrets)

Everything the run needs is an env var. There is **no** hardcoded host/user/token/
kubeconfig in the script — it is safe to commit and publish.

| Var | Default | Meaning |
|-----|---------|---------|
| `KUBECONFIG` | (current context) | live `k8squad-test` kubeconfig |
| `TARGET_REPO_URL` | *(required)* | `Project.spec.repo.url` — the app repo the crew writes to |
| `TARGET_REPO_REF` | `main` | tracked branch |
| `NAMESPACE` | `bmad-squad` | squad namespace |
| `CP_NS` | `k8squad-system` | control-plane / toolchain-catalog namespace |
| `GH_SECRET_NAME` | `github-writepath-token` | write-path Secret (created out-of-band, §4) |
| `MODEL_SECRET_NAME` | `model-credentials` | model token Secret (created out-of-band, §4) |
| `RECONCILE_TIMEOUT` | `300` | Phase 2 per-object Ready wait (s) |
| `RUN_TIMEOUT` | `3600` | Phase 4 Run terminal wait (s) |
| `SEED_CMD` | *(unset)* | Phase-4 seam — see below |
| `TRIGGER_CMD` | *(unset)* | Phase-4 seam — see below |
| `OUT_DIR` | `.e2e-out` | artifact output dir (gitignored) |

## SECURITY (§4 / SC-6)

- Credential Secrets (`model-credentials`, `github-writepath-token`) are created
  **out of band by ProxOps** and referenced **by name** only. The harness asserts they
  exist; it **never reads or echoes a token value**.
- The plaintext `01-credentials.yaml` (`REPLACE_ME` Secrets) is **excluded** from the
  applied manifest, and the harness refuses to apply any manifest still containing a
  `Secret` doc or `REPLACE_ME` token.
- The board's leaked write-path PAT must be **rotated** before use (tracked on ISI-3559).

## Phase-4 trigger seam (the §9.1 feasibility risk)

Work items are coordination-Postgres rows and Run creation is operator/coordinator-driven
— there is **no clean public "create work item + create Run" API**. Rather than hardcode
an unproven path, Phase 4 uses a pluggable seam the runner supplies:

- **`SEED_CMD`** — seeds the DB+backend+frontend backlog for the project and prints **one
  work-item id** (the `workItemRef`) to stdout.
- **`TRIGGER_CMD`** — optional; given the work-item id as `$1`, dispatches the Run. If
  unset, the harness applies a default `Run` CR referencing
  `teamRef=bmad-squad / projectRef=bmad-demo-project / workItemRef=<id>`.

If `SEED_CMD` is **unset**, Phases 4–5 are reported **BLOCKED** with the §9.1 finding — the
CRD/operator plane (Phases 0–3, SC-1/SC-2) is still fully validated and reported. DevOps
(ISI-3563 / consult) owns confirming the seam; ProxOps (ISI-3561) supplies it at run time.

## Run it (ProxOps, on live k8squad-test)

```bash
export KUBECONFIG=~/.config/capmox/k8squad-test.kubeconfig
export TARGET_REPO_URL=https://github.com/K8squad/sympozium-todo-demo
# ProxOps: create the two Secrets out of band first, then:
./hack/platform-e2e-bmad.sh
# artifacts land in .e2e-out/ — attach phase-status.md/.json + condition dumps to the
# Paperclip run issue (SC-5). Do NOT publish to GitHub (ISI-3534 no-leak constraint).
```

Exit code is 0 iff the minimum bar clears; a partial/fail exits non-zero while the
per-phase table distinguishes **BLOCKED ≠ FAIL** for triage.

## Reporting (SC-5) & failure capture

- Attach `.e2e-out/phase-status.md` + `.json` + `phase2-conditions.txt` + any
  `phase4-operator.log` to the **Paperclip** run issue — not GitHub.
- On any failure, hand the failure window (start/end UTC, traceId, failing request) to
  Observability per the ISI-3534 correlation contract (coordinate shape on ISI-3562).
