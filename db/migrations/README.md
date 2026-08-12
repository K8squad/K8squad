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
| 0001 | `0001_coord_schema.sql` | `coord` — `work_item`, `comment`, `artifact`, `claim`, `run_event` | Story 2.1 (ISI-2191), Arch §6.1 |

### Name mapping (Story 2.1 wording ↔ Arch §6.1 authoritative names)

Story 2.1 lists the tables loosely; the durable names follow §6.1 (singular, `coord` schema) and the
executable mechanism SQL in §6.2/§6.4:

| Story 2.1 wording | Durable table | Notes |
|-------------------|---------------|-------|
| `work_items` | `coord.work_item` | + `parent_id` adjacency (§6.1 r24), board `state` enum (r25) |
| `comments` | `coord.comment` | append-only, provenanced |
| `artifacts` | `coord.artifact` | `UNIQUE(work_item_id, run_id, kind)` upsert key |
| `checkouts` | `coord.claim` | one row per item (PK); `fence_token` ≡ "lease_epoch" |
| `run_events` / `audit_log` | `coord.run_event` | append-only audit + shim Run-event stream (§6.5/§8.11) |

## Structural invariants enforced by 0001 (not by application discipline)

- **Exactly one active claim row per work item** — `claim.work_item_id` PK + an `AFTER INSERT` trigger
  that auto-provisions one unheld claim row per work item.
- **Append-only history** — a `BEFORE UPDATE OR DELETE` trigger rejects any mutation of `comment` and
  `run_event` (backs the AC's "no UPDATE/DELETE path in code" with a DB-level guard).
- **Artifact idempotency under re-entry** — the `UNIQUE(work_item_id, run_id, kind)` upsert key.
- **Orphans-as-roots** — `work_item.parent_id … ON DELETE SET NULL`.
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
