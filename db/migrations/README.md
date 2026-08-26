# `db/migrations` — versioned, forward-only SQL

Postgres is the single source of truth for all durable state (ADR-001). Every app-data schema
(`coord`, `auth`, `scm`, discussion, memory) is created and evolved here by **numbered, forward-only
SQL migrations** applied in filename order.

## Conventions

- **Naming:** `NNNN_short_description.sql`, zero-padded, monotonically increasing (`0001_…`, `0002_…`).
- **Forward-only:** there are **no down migrations**. A mistake is corrected by a new forward
  migration, never by rolling back a released one. The coordination spine (Story 2.1) is forward-only
  by contract.
- **Applied once, in order:** the apiserver migration runner tracks applied versions and applies each
  file exactly once on startup — the same discipline the `auth` schema uses (§12.3). Do not edit a
  migration after it has merged/shipped; add a new one.
- **One concern per file** where practical; the initial schema for a bounded context may land as a
  single file (e.g. `0001_coord_schema.sql`).
- **`*_test.sql` companions** are runnable self-checks (plain SQL, no framework), not migrations —
  the runner ignores them. See below.

## Current migrations

| Version | File | Schema | Story / Arch |
|--------:|------|--------|--------------|
| 0001 | `0001_coord_schema.sql` | `coord` — `work_item`, `comment`, `artifact`, `claim`, `audit_log` | Story 2.1 (ISI-2191), Arch §6.1 |
| 0002 | `0002_coord_dispatch.sql` | `coord` — `dispatch` (§6.4 idempotent-dispatch marker for the coordinator loop) | Story 2.9 (ISI-2526), Arch §6.4/FR-B3 |
| 0003 | `0003_coord_outbox.sql` | `coord` — `outbox` (§12.1 transactional domain-event relay) | Story 12.1 (ISI-2260), Arch §12.1 |
| 0004 | `0004_discussion_schema.sql` | `discussion` — `thread`, `message` (per-Project room; append-only, server-stamped provenance, NO room table) | Story 10.1 (ISI-2702/ISI-2709), Arch §7.5/ADR-019 |
| 0005 | `0005_reconcile_step.sql` | `coord` — `reconcile_step` (§6.4 crash-safe Run reconcile state machine) | Story 3.1 (ISI-2535/ISI-2655), Arch §6.4 |
| 0006 | `0006_auth_schema.sql` | `auth` — `user`, `session` (local-cred identity + fail-closed session store; the `ksquad_session` → AuthorContext backing) | ISI-2758 (split from ISI-2750), Arch §12.3/ADR-033 |
| 0012 | `0012_work_item_search.sql` | `coord` — `work_item.search_tsv` generated tsvector + GIN index (Postgres FTS corpus for global search) | Story 8.18 (ISI-2912), ADR-039 |

### Name mapping (Story 2.1 wording ↔ Arch §6.1 authoritative names)

Story 2.1 lists the tables loosely; the durable names follow §6.1 (singular, `coord` schema) and the
executable mechanism SQL in §6.2/§6.4:

| Story 2.1 wording | Durable table | Notes |
|-------------------|---------------|-------|
| `work_items` | `coord.work_item` | + `parent_id` adjacency (§6.1 r24), board `state` enum (r25) |
| `comments` | `coord.comment` | append-only, provenanced |
| `artifacts` | `coord.artifact` | `UNIQUE(work_item_id, run_id, kind)` upsert key |
| `checkouts` | `coord.claim` | one row per item (PK); `fence_token` ≡ "lease_epoch" |
| `audit_log` | `coord.audit_log` | immutable low-volume coordination audit (§6.5) |
| `run_events` | *(no table)* | high-volume shim trace firehose (§10.1) — rides **SSE live + opt-in OTel export (§17.2)**, not a coord table (ADR-040) |

**Why the firehose gets no table (ISI-2339 F1 → ISI-2340, ADR-040):** the immutable coordination audit
(§6.5: claim/comment/artifact/state-transition/completion events) and the high-volume shim trace
firehose (§10.1: `tool_call`/`llm_call`/`build_output`/`error`) have opposite retention semantics.
Merging them made the firehose **unprunable** (the append-only DELETE trigger blocks retention) and
interleaved the monotonic audit sequence with trace noise. Resolution: Story 2.1 lands only
`audit_log` (immutable, monotonic, retention-free — coordination-event volume is bounded), and the
firehose **does not get a Postgres table in v1 at all** — it rides **SSE live** (ephemeral) plus
**opt-in OTel export (§17.2)**, whose backend owns its retention. This keeps `audit_log` structurally
immutable *without qualification* and avoids re-storing telemetry OTel already owns. Story 8.11 wires
the shim trace to SSE + OTel emission, **not** to a coord table (so 8.11 needs no migration).
*Rejected:* a time-partitioned `coord.run_trace` table (a new stateful surface re-storing OTel data;
`DROP PARTITION` retention would bypass this table's immutability claim) — that was the F1 defect, not
the fix.

## Structural invariants enforced by 0001 (not by application discipline)

- **Exactly one active claim row per work item** — `claim.work_item_id` PK + an `AFTER INSERT` trigger
  that auto-provisions one unheld claim row per work item.
- **Append-only history** — `BEFORE UPDATE OR DELETE` (row) **and** `BEFORE TRUNCATE` (statement)
  triggers reject any mutation *or* whole-table wipe of `comment` and `audit_log` (backs the AC's
  "no UPDATE/DELETE path in code" with a DB-level guard that `TRUNCATE` can't evade).
- **Artifact idempotency under re-entry** — the `UNIQUE(work_item_id, run_id, kind)` upsert key.
- **Orphans-as-roots** — `work_item.parent_id … ON DELETE SET NULL`.
- **Tenancy stays one predicate** — a `BEFORE INSERT/UPDATE` trigger rejects a child whose `project_id`
  differs from its parent's and inherits the parent's `team_id` (§6.1(c)/§12.1); the reconciler
  enforces the same, so cross-Project re-parenting is blocked at two layers.
- **Board-state integrity** — `CHECK (state IN (backlog,todo,in_progress,in_review,done))`; `blocked`
  is an orthogonal condition (`blocked_reason`), never a board state.

There is deliberately **no `message` table** — no agent-to-agent channel exists in the schema
(structural enforcement of no-P2P, I4).

## Running the self-check

```bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
  -f db/migrations/0001_coord_schema.sql \
  -f db/migrations/0001_coord_schema_test.sql
```

The test wraps its assertions in a transaction and `ROLLBACK`s, so it leaves no residue. The live
concurrency/fencing guarantees (§6.2/§6.3, cases C1..C7) are exercised separately by the Go chaos
suite (`.github/workflows/spine-chaos.yml`, Story 2.7).
