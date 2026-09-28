# Runbook — Shared-database migration contract & deploy-skew guards

**Scope:** any schema change to the single `k8squad` Postgres database that is
read or written by **more than one ksquad service binary**. Codifies the
migration contract that would have prevented the ISI-5109 cluster-wide dispatch
outage, and the guards adopted against the recurrence of that *deploy-skew*
failure family.

Owner: DevOps / release on-call. Related incidents: **ISI-5109** (root cause),
**ISI-5110** (live remediation), **ISI-4916** (operator image mismatch),
**ISI-5051** (coord-schema auto-migrate runner). Related code:
`internal/memory` (`store.go`, `migrate.go`, `migrations/`), `db/dbmigrate`
(`db/migrations/`), `cmd/memory`, `cmd/operator`, `cmd/apiserver`,
`cmd/event-relay`.

---

## 0. Why this runbook exists — the ISI-5109 outage in one paragraph

A **type-changing** migration on a **shared** column
(`memory.memory_records.project_id` `uuid → text`, migration
`internal/memory/migrations/0004_project_id_text.sql`) was applied by one
service image (`ksquad-memory` `sha-5883fe5`) while a **co-consumer that queries
the same table directly** (`ksquad-operator` `sha-b3e695a`) was left on an older
binary whose embedded query code still cast `::uuid`. Postgres rejected every
`text = uuid` comparison with **SQLSTATE 42883**, which aborted
`rundrive → coord.Dispatch → contextasm: memory recall` for **every run,
cluster-wide** — silently, one per-run reconcile at a time. The migration itself
was correct. The failure was that two binaries sharing one schema were not
rolled in lockstep, and **nothing failed closed at boot** — the mismatch only
surfaced as noisy per-reconcile errors.

---

## 1. What is actually shared

The whole cluster runs against **one** Postgres database (`k8squad`, secret
`k8squad-db-app`). Four service binaries open it via `DATABASE_URL`:

| Service | Reads `DATABASE_URL` | Imports `internal/memory` | Runs migrations |
|---------|:---:|:---:|:---:|
| `cmd/apiserver` | ✅ | — | ✅ coord (`db/dbmigrate.Apply`, ISI-5051) |
| `cmd/memory` | ✅ | ✅ | ✅ memory (`internal/memory` via `Open`) |
| `cmd/operator` | ✅ | ✅ | ✅ memory (`internal/memory` via `Open`) |
| `cmd/event-relay` | ✅ | — | — |

Two logical schemas live in that one DB, each with its **own** forward-only
runner and ledger:

| Schema | Migration source | Runner | Ledger | Owner binary | Direct co-consumers |
|--------|------------------|--------|--------|--------------|---------------------|
| `memory` | `internal/memory/migrations/*.sql` | `internal/memory/migrate.go` (`applyMigrations`, called from `memory.Open`) | `memory.schema_migrations` | `cmd/memory` | **`cmd/operator`** (queries `memory.memory_records` directly) |
| `public` (coord) | `db/migrations/*.sql` | `db/dbmigrate.Apply` | `public.schema_migrations` | `cmd/apiserver` | `cmd/operator`, `cmd/event-relay` |

### 1.1 The sharp edge

`memory.Open()` is called by **both** `cmd/memory` *and* `cmd/operator`
(`cmd/operator/main.go` ≈ L489). So **both binaries embed and apply the memory
migration set from their own copy of `internal/memory/migrations`**, against a
**single shared `memory.schema_migrations` ledger**.

- Whichever pod starts (or reconciles) first with the newer migration set
  **applies the retype** for the whole cluster.
- The lagging binary's readiness gate only asserts the **base** migration
  (`internal/memory/store.go` `Ready`: `... WHERE version =
  'migrations/0001_memory.sql'`). It does **not** assert its own embedded HEAD,
  so it comes up **green** while running stale query code against the newly
  retyped column.

That is exactly why the outage was silent: the lagging binary passed its own
health check and only failed at query time, per reconcile, forever.

---

## 2. The migration contract (MANDATORY for shared columns)

A **shared column** is any column in a table that is read or written directly by
more than one binary in the table above (`memory.memory_records.*`, coord tables
touched by operator/event-relay/apiserver).

1. **Expand / contract — never a bare destructive retype in one release.**
   A type change (or drop/rename) of a shared column MUST be staged so a
   one-image-behind consumer *degrades* instead of hard-failing:
   - **Release N (expand):** make both representations readable. Add the new
     column, or write queries that accept both the old and new type
     (e.g. compare with a form both types coerce to, such as `::text` on both
     sides), and ship that read-compatible code to **every** co-consumer first.
   - **Release N+1 (contract):** only after every co-consumer is on ≥ Release N
     may the destructive migration run.
   `0004_project_id_text.sql` was a single-step contract with no expand phase —
   that is the pattern this rule forbids for shared columns going forward.

2. **Roll co-consumers in lockstep.** When a migration changes a shared column
   *and* the query code that reads it, the migration and **all** binaries that
   import the changed shared-schema package MUST reach the cluster together:
   - Memory-schema retype ⇒ roll `ksquad-memory` **and** `ksquad-operator`
     together (both import `internal/memory`).
   - Coord-schema type change ⇒ roll `ksquad-apiserver`, `ksquad-operator`,
     `ksquad-event-relay` together.

3. **Verify by DIGEST, not tag** (ISI-4916, ISI-4745). `pullPolicy: IfNotPresent`
   + a reused tag silently serves a stale cached image. After the roll, confirm
   the running pod's image **digest** matches the intended `main` SHA for
   *every* co-consumer — see `docs/runbooks` / the ISI-4916 note.

4. **Forward-only, one tx per migration, no auto-baseline.** Unchanged from
   ISI-5051: runners never mark an unrun migration "applied" on a pre-existing
   schema; a broken migration fails closed at boot rather than half-applying.

---

## 3. Guards adopted

| # | Guard | Status | Owner |
|---|-------|--------|-------|
| 1 | **Release lockstep** for shared-schema co-consumers (§2.2) | **Adopted** — this runbook + `Build Images` builds all services every `main` push; enforce the co-consumer roll + digest check on any shared-column change | DevOps / release |
| 2 | **Startup schema-compat probe** — each binary asserts, at boot, that the shared columns it queries have the types its code expects (or that the shared ledger is ≥ its own embedded HEAD) and **fails closed** with a clear message | **Recommended** — tracked as a follow-up child; would have turned the silent 42883 storm into a loud boot failure | platform / Architect |
| 3 | **Expand/contract migrations** for shared columns (§2.1) | **Adopted as convention** — required in review for any shared-column retype/drop | authors + reviewers |
| 4 | **Auto-apply migration runner, fail-closed at boot** (ISI-5051) | **Already shipped** — `db/dbmigrate` (coord) and `internal/memory/migrate` (memory) | platform |

### 3.1 The gap guard #2 closes

Guard #4 already makes a *missing* or *broken* migration fail closed. It does
**not** catch the ISI-5109 case, where the migration applied cleanly and the
*lagging reader's code* was wrong. Guard #2 is the missing piece: a boot-time
type assertion (e.g. `SELECT data_type FROM information_schema.columns WHERE
table_schema='memory' AND table_name='memory_records' AND
column_name='project_id'` compared against the type the binary's queries assume)
or a ledger-floor check (`memory.schema_migrations` HEAD ≥ the binary's own
embedded HEAD). Replacing the base-only `Ready` assertion
(`internal/memory/store.go`) with a HEAD-floor assertion is the smallest
concrete version.

---

## 4. Checklist — before merging any shared-DB migration

- [ ] Does the migration change the **type**, drop, or rename a column that any
      *other* binary reads directly? (Cross-check §1's co-consumer columns.) If
      no → normal additive migration, skip the rest.
- [ ] If yes → is it staged **expand → contract** across two releases (§2.1)?
- [ ] Are **all** co-consumer binaries updated to read the new shape in the
      *expand* release, and do they ship *before* the *contract* release?
- [ ] Release plan names every co-consumer image to roll in lockstep (§2.2).
- [ ] Post-roll: digest of every co-consumer pod verified against the intended
      `main` SHA (§2.3).

---

## 5. Incident response — "42883 / 22P02 storm after a deploy"

Symptom: a specific query family fails cluster-wide with `operator does not
exist: text = uuid` (42883) or `invalid input syntax for type uuid` (22P02),
one per reconcile, while pods report healthy.

1. Identify the shared column and the migration that retyped it
   (`memory.schema_migrations` / `public.schema_migrations` applied_at vs the
   deploy timeline).
2. Identify the **lagging** co-consumer: the binary whose image SHA predates the
   matching query-code fix. Compare running-pod **digests** (§2.3).
3. Remediate by rolling the lagging binary to a `main` SHA that includes the
   query-code fix — this is the ISI-5110 remediation shape. Verify by digest,
   then re-run one affected work item to confirm recovery.
4. File / update the expand-contract follow-up if the migration was a bare
   destructive retype (§2.1).
