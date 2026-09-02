---
title: "Observability for the agent bootstrap path (context assembly + task-io)"
issue: ISI-3592
parent: ISI-3588
implements_for: ISI-3590 (wire Context Assembler + task-io skill)
feeds: ISI-3562 (o11y for platform-E2E), ISI-3559 (BMAD E2E)
author: Observability Agent
date: 2026-09-02
status: spec-complete
type: observability-spec / instrumentation contract
---

# Observability spec — agent bootstrap path

This spec defines the **traces, metrics, and logs** that must be emitted by the two
bootstrap surfaces the ISI-3588 study recommended finishing:

1. **PUSH context assembly** — `pkg/contextasm.Assembler.Assemble`, wired into
   `pkg/controller/rundrive` dispatch (ISI-3590 S1). *Which content classes
   resolved, sizes, truncation, snapshot writes.*
2. **task-io calls** — the predefined `task-io` skill + coord API seam (ISI-3590
   S2/S3): `get-task` / `post-comment` / `update-status` / `checkout`. *Latencies +
   failures.*

It is an **instrumentation contract**: ISI-3590 folds these hook points in as it
wires the path; the KPIs in §7 are consumed by ISI-3562 / ISI-3559 as pass/fail
gates. Nothing here emits until ISI-3590 lands — this spec is deliberately written
*before* the wiring so the instrumentation is added with the code, not bolted on.

---

## 0. Grounding — what exists today

- **Trace/log spine exists.** `pkg/telemetry` (`telemetry.Setup`) installs a
  `TracerProvider` + W3C propagator + otelslog `LoggerProvider`. Every log with a
  span-bearing ctx is already `trace_id`/`span_id`-correlated. Exporter is stdout
  today; OTLP is a one-line swap in `Setup` (ISI-3103).
- **Run span exists.** `pkg/controller/rundrive/driver.go:213` opens exactly one
  `run.reconcile` span per drive pass with attributes `ksquad.run.id`,
  `ksquad.run.work_item_ref`, `ksquad.run.namespace`, `ksquad.run.name`. **All
  bootstrap spans below are children of `run.reconcile`** via the passed `ctx`.
- **⚠ No MeterProvider yet.** `telemetry.Setup` installs trace + log providers only;
  there is **no `telemetry.Meter()`**. Every metric in §4/§5 is therefore blocked on
  a small prerequisite: add a `MeterProvider` (stdout→OTLP, mirroring the existing
  pattern) + a `Meter()` accessor. **Tracked as the child of this issue.** The
  **trace** half (§3) has no such prerequisite and can land with ISI-3590 S1 today.
- **contextasm surface is stable.** `Assemble(ctx, AssembleRequest) (*AssembleResult,
  error)` already gathers the 4 sources, tier-stamps, budgets (`ApplyBudget`), and
  snapshots (`buildSnapshot` → `Run.status.contextSnapshot`). The hook points below
  reference real symbols in `pkg/contextasm/assembler.go` + `budget.go`.

---

## 1. Instrumentation scopes & naming

| Scope | Instrumentation name | Emitted from |
|-------|----------------------|--------------|
| operator (context assembly) | `github.com/K8squad/K8squad` (existing) | `pkg/contextasm`, `pkg/controller/rundrive` |
| coord task-io API (server) | `github.com/K8squad/K8squad` | coord API handlers (ISI-3590 S2) |
| task-io skill (agent-side client) | `github.com/K8squad/K8squad/skills/task-io` | injected skill runtime |

Attribute namespace: **`ksquad.*`**, extending the existing `ksquad.run.*`. New
sub-namespaces: `ksquad.contextasm.*`, `ksquad.taskio.*`. See §6 for the registry.

---

## 2. Trace topology (one distributed trace per Run bootstrap)

```
run.reconcile                                (exists — driver.go:213)
└── contextasm.assemble                       (S1 — new; child of run.reconcile)
    ├── contextasm.source.work_item           (coord DB read — Sources.WorkItem)
    ├── contextasm.source.project_meta        (Project CRD read — Sources.ProjectMeta)
    ├── contextasm.source.memory_recall       (memory service 6.6 — Sources.MemoryRecall)
    ├── contextasm.source.artifacts           (SCM mirror 5.4 — Sources.Artifacts)
    ├── [event] contextasm.budget.resolved    (ResolveBudget)
    ├── [event] contextasm.budget.applied     (ApplyBudget — truncation outcome)
    └── [event] contextasm.snapshot.written   (buildSnapshot + pinRecallIDs)

taskio.<op>                                   (S2/S3 — separate trace, joined via W3C)
   op ∈ {get_task, post_comment, update_status, checkout}
```

**Context propagation for task-io.** The agent pod is env-injected (`KSQUAD_COORD_URL`,
run-scoped token, `WORK_ITEM_ID`, `RUN_ID`) — mirror Paperclip. Stamp the Run's
`traceparent` into the pod env (or the dispatched envelope metadata) at dispatch and
have the skill `telemetry.Extract` it, so each `taskio.<op>` span is a child of the
Run trace. `telemetry.Inject`/`Extract` already exist for exactly this. The operator
Run pipeline already emits a real W3C trace (per ISI-3562), so `taskio.*` spans join
the same `run.name`-filterable trace in Dynatrace.

### 2.1 `contextasm.assemble` span attributes

Emit only **counts / sizes / revisions / ids** — **never element `.Content`** (PII, §8).

| Attribute | Type | Source |
|-----------|------|--------|
| `ksquad.run.id` | string | `req.Run.UID` |
| `ksquad.run.work_item_ref` | string | `req.Run.Spec.WorkItemRef` |
| `ksquad.project` | string | `req.Run.Spec.ProjectRef.Name` |
| `ksquad.team` | string | `req.TeamID` |
| `ksquad.contextasm.resume` | bool | `req.Existing != nil` (re-entrant vs fresh) |
| `ksquad.contextasm.context_window` | int | `req.ContextWindow` |
| `ksquad.contextasm.elements.authoritative` | int | count of `TierAuthoritative` elements |
| `ksquad.contextasm.elements.untrusted_recall` | int | count of `TierUntrustedRecall` |
| `ksquad.contextasm.elements.untrusted_external` | int | count of `TierUntrustedExternal` |
| `ksquad.contextasm.tokens.work_item` | int | budgeted tokens, authoritative task |
| `ksquad.contextasm.tokens.project_docs` | int | budgeted tokens, projectMeta |
| `ksquad.contextasm.tokens.memory_recall` | int | budgeted tokens, recall |
| `ksquad.contextasm.tokens.artifacts` | int | budgeted tokens, artifacts |
| `ksquad.contextasm.truncated_tiers` | string[] | tiers with `el.Truncated == true` |
| `ksquad.contextasm.dropped_elements` | int | elements set to `truncateMarker` |
| `ksquad.contextasm.recall_docs.returned` | int | `len(recall)` from source |
| `ksquad.contextasm.recall_docs.kept` | int | `len(snapshot.MemoryDocIDs)` after budget |
| `ksquad.contextasm.snapshot.work_item_revision` | string | `snapshot.WorkItemRevision` |
| `ksquad.contextasm.snapshot.goal_revision` | string | `snapshot.GoalRevision` |
| `ksquad.contextasm.fail_closed` | bool | true on `ErrMustIncludeExceedsWindow` |

**Status:** on any error return, `span.RecordError(err)` + `span.SetStatus(codes.Error,…)`.
`ErrMustIncludeExceedsWindow` (budget.go:71) is the load-bearing failure — always set
`ksquad.contextasm.fail_closed=true` and increment the fail-closed counter (§4).

### 2.2 `contextasm.source.*` span attributes

Each of the 4 gather calls (the real latency/failure points — DB, CRD, memory svc,
SCM mirror) gets a child span:

| Attribute | Type |
|-----------|------|
| `ksquad.contextasm.source` | string — `work_item`\|`project_meta`\|`memory_recall`\|`artifacts` |
| `ksquad.contextasm.pinned` | bool — reading a pinned revision/doc-set (resume) vs latest |
| `ksquad.contextasm.result_count` | int — rows/docs/artifacts returned (0 for scalars) |

Error → `RecordError` + error status. A pinned-revision mismatch (snapshot reuse
guard, assembler.go:98) must surface as an error span, never a silent fallback.

### 2.3 `taskio.<op>` span attributes

| Attribute | Type |
|-----------|------|
| `ksquad.taskio.op` | string — `get_task`\|`post_comment`\|`update_status`\|`checkout` |
| `ksquad.run.id`, `ksquad.taskio.work_item_id` | string |
| `ksquad.taskio.status.from` / `.to` | string — **only** for `update_status` |
| `http.response.status_code` | int — coord API HTTP status |
| `ksquad.taskio.error_class` | string — `auth`\|`conflict`\|`timeout`\|`server`\|`client` (on error) |

---

## 3. Metrics — context assembly (blocked on MeterProvider prerequisite)

All histograms use the OTel default duration buckets unless noted. **Labels are
bounded** — no `run.id`/`work_item.id` as metric labels (§8 cardinality).

```yaml
metrics:
  - name: ksquad.contextasm.assemble.duration
    type: histogram
    unit: s
    description: End-to-end Assemble() wall time.
    labels: [outcome, resume, project]        # outcome: success|error; resume: true|false

  - name: ksquad.contextasm.source.read.duration
    type: histogram
    unit: s
    description: Per-source gather latency (DB/CRD/memory/SCM).
    labels: [source, outcome]                  # source: work_item|project_meta|memory_recall|artifacts

  - name: ksquad.contextasm.elements
    type: histogram
    unit: "{element}"
    description: Element count per tier per assemble (distribution, not gauge).
    labels: [tier]                             # authoritative|untrusted_recall|untrusted_external

  - name: ksquad.contextasm.tokens
    type: histogram
    unit: "{token}"
    description: Budgeted estimated tokens per tier per assemble.
    labels: [tier]

  - name: ksquad.contextasm.truncations.total
    type: counter
    description: Elements truncated or dropped by the budgeter.
    labels: [tier, kind]                       # kind: truncated|dropped

  - name: ksquad.contextasm.fail_closed.total
    type: counter
    description: Runs that failed closed (must-include > window).
    labels: [reason]                           # must_include_exceeds_window|over_window_tier

  - name: ksquad.contextasm.snapshot.writes.total
    type: counter
    description: contextSnapshot writes to Run status.
    labels: [outcome]

  - name: ksquad.contextasm.recall.docs
    type: histogram
    unit: "{doc}"
    description: Memory recall docs returned vs kept-after-budget.
    labels: [phase]                            # returned|kept
```

---

## 4. Metrics — task-io (blocked on MeterProvider + ISI-3590 S2)

```yaml
metrics:
  - name: ksquad.taskio.request.duration
    type: histogram
    unit: s
    description: task-io call latency (client-observed; also emit server-side).
    labels: [op, outcome]                      # op: get_task|post_comment|update_status|checkout

  - name: ksquad.taskio.requests.total
    type: counter
    labels: [op, outcome, code_class]          # code_class: 2xx|4xx|5xx

  - name: ksquad.taskio.errors.total
    type: counter
    labels: [op, error_class]                  # auth|conflict|timeout|server|client

  - name: ksquad.taskio.status.transitions.total
    type: counter
    description: Observed status write-backs (feeds BMAD loop health).
    labels: [from, to]                         # bounded status enum
```

---

## 5. Logs

No new logging framework — the otelslog bridge already correlates. Requirements:

- Emit `slog.InfoContext(ctx, …)` (span-bearing ctx) at: assemble start/end,
  each fail-closed, each task-io error. These inherit `trace_id`/`span_id` for free.
- **Never log element `.Content`, comment bodies, or recall text.** Log the same
  counts/sizes/ids/revisions as the span attributes. This is the primary PII seam
  on the bootstrap path (work-item descriptions, comments, and memory recall are
  free-form user text).

---

## 6. Semantic conventions (weaver registry fragment)

Custom `ksquad.*` attributes registered so ISI-3562/3559 validate against a schema
rather than magic strings. To be added to the k8squad semconv registry:

```yaml
groups:
  - id: ksquad.contextasm
    type: attribute_group
    brief: Context Assembler bootstrap attributes.
    attributes:
      - id: ksquad.contextasm.resume
        type: boolean
        brief: Re-entrant resume (pinned snapshot) vs fresh assembly.
      - id: ksquad.contextasm.source
        type: { members: [work_item, project_meta, memory_recall, artifacts] }
        brief: Which of the 5 §8.5 content-class sources this span read.
      - id: ksquad.contextasm.fail_closed
        type: boolean
        brief: Must-include context exceeded the model window (story 5.9).
      - id: ksquad.contextasm.tier
        type: { members: [authoritative, untrusted_recall, untrusted_external] }
        brief: Provenance trust tier (arch §8.5/§7.3).
  - id: ksquad.taskio
    type: attribute_group
    brief: task-io skill call attributes.
    attributes:
      - id: ksquad.taskio.op
        type: { members: [get_task, post_comment, update_status, checkout] }
      - id: ksquad.taskio.error_class
        type: { members: [auth, conflict, timeout, server, client] }
```

---

## 7. SLOs / KPIs — test-consumable contract for ISI-3562 / ISI-3559

Machine-readable so the E2E harnesses assert these as pass/fail after driving the
bootstrap path. DQL sketches assume OTLP→Dynatrace (tenant per ISI-3539/3562).

```yaml
slos:
  - id: bootstrap.context_assembled
    objective: "Every dispatched Run emits exactly one contextasm.assemble span with outcome=success"
    signal: span
    assert: "count(contextasm.assemble where outcome=success) == count(dispatched runs)"
    consumers: [ISI-3559, ISI-3562]

  - id: bootstrap.no_silent_undercontext
    objective: "Assembled envelope carries >= 1 authoritative element (task actually present)"
    signal: span_attr
    assert: "ksquad.contextasm.elements.authoritative >= 1"
    rationale: "Guards the exact G-gap: agent must not start with title+body-only."

  - id: bootstrap.fail_closed_visible
    objective: "Budget fail-closed is observable, never silent"
    signal: metric
    assert: "on ErrMustIncludeExceedsWindow -> ksquad.contextasm.fail_closed.total increments AND span status=error"

  - id: bootstrap.assemble_latency
    objective: "p95 contextasm.assemble < 2s (single-Run bootstrap budget)"
    signal: metric
    assert: "histogram_p95(ksquad.contextasm.assemble.duration) < 2"

  - id: taskio.status_writeback_works
    objective: "update-status round-trips (the write-back the study said is missing today)"
    signal: metric
    assert: "ksquad.taskio.status.transitions.total > 0 AND ksquad.taskio.errors.total{op=update_status} == 0"
    consumers: [ISI-3559]

  - id: taskio.latency
    objective: "p95 task-io call < 500ms"
    signal: metric
    assert: "histogram_p95(ksquad.taskio.request.duration) < 0.5"
```

DQL validation examples (5-step query gate applies before trace validation):
```
// context assembled per run
fetch spans | filter span.name == "contextasm.assemble"
  | summarize c = count(), by:{ksquad.contextasm.resume}
// fail-closed watch
fetch spans | filter ksquad.contextasm.fail_closed == true
// task-io error rate by op
fetch spans | filter span.name startsWith "taskio."
  | summarize errs = countIf(span.status == "error"), total = count(), by:{ksquad.taskio.op}
```

---

## 8. Cardinality & PII guardrails (my core rules applied)

- **Cardinality is the enemy.** `run.id` / `work_item.id` / `traceparent` are
  **span/exemplar-only, never metric labels.** Metric label domains are all bounded
  enums (source×4, tier×3, op×4, outcome×2–3, code_class×3, status-enum). `project`
  is the one semi-open label (bounded by tenant project count) — acceptable on
  `assemble.duration` only; drop it if project count grows unbounded.
- **PII detection & cleanup.** The bootstrap path is the highest-PII surface in the
  system: work-item descriptions, comment threads, and memory recall are free-form
  user text. **Content never leaves as an attribute, label, or log field** — only
  counts, byte/token sizes, revisions, ids, hashes. buildEnvelope must not be
  instrumented in a way that materializes `.Content` into telemetry.
- **Sampling.** Bootstrap is once-per-Run and low-volume relative to agent tool
  calls — recommend **head-based AlwaysOn** for `contextasm.assemble` and `taskio.*`
  (do not sample away the bootstrap trace the E2E asserts on). Revisit only if Run
  volume changes the calculus.

---

## 9. Instrumentation hook points (for ISI-3590)

Precise, mechanical add-ons — fold into ISI-3590 as it wires the path:

1. **`Assemble` span** — wrap the body of `Assembler.Assemble`
   (`pkg/contextasm/assembler.go:161`) in `ctx, span := telemetry.Tracer().Start(ctx,
   "contextasm.assemble", …)`; set §2.1 attributes from `wi`, `meta`, `recall`,
   `budget`, `budgeted`, `snapshot` before return; `RecordError` on every existing
   `return nil, err`.
2. **Source spans** — wrap the 4 `a.sources.*` calls (assembler.go:179-194) each in a
   `contextasm.source.*` child span with §2.2 attributes.
3. **Budget/snapshot events** — `span.AddEvent("contextasm.budget.applied", …)` after
   `ApplyBudget`; count truncated/dropped by scanning `budgeted.Elements` for
   `el.Truncated` / `truncateMarker` (budget.go:197/256).
4. **Metrics** — after the MeterProvider prerequisite lands, record §3 instruments at
   the same points.
5. **task-io (S2/S3)** — server handlers open `taskio.<op>` spans + record §4 metrics;
   the skill client `telemetry.Extract`s the injected `traceparent` and opens the
   client span. Add `traceparent` to the dispatch env-injection alongside
   `KSQUAD_COORD_URL`.

**Prerequisite (child of ISI-3592):** add a `MeterProvider` + `telemetry.Meter()` to
`pkg/telemetry` (stdout→OTLP, mirroring the existing Tracer/Logger providers). The
trace half (steps 1–3, 5-spans) needs no prerequisite and can land immediately.
