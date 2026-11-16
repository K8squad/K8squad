# ISI-7484 — preview-deploy migrate failure root cause + repair (ownership drift)

## Symptom
Board preview deploy FAILed on `fix/isi-7484-durable-run-step-fallback` at both
`c991b00c` and `e226dbbc`: the `ksquad-migrate` Helm pre-upgrade hook Job
(apiserver image `--migrate-only` = `dbmigrate.Apply`) Errored on both pod
attempts, so `helm upgrade` aborted (old pods kept serving — the ISI-7109
fail-closed design worked as intended). GitHub-Actions checks were a separate,
unrelated failure (gosec G706 false positive, fixed in `.golangci.yml` @ e226dbbc).

## Root cause (ISI-6371 ownership-drift class)
Migration `0044_run_event_retention.sql` runs `CREATE OR REPLACE FUNCTION
coord.outbox_guard()` to add the one governed run-retention DELETE exception.
On the **k8squad-preview** CNPG DB the migrate role is `k8squad`, and:

| coord object            | owner on preview (BEFORE) |
|-------------------------|---------------------------|
| table `coord.outbox`    | `k8squad`                 |
| func `outbox_guard()`   | `postgres`  ← drift       |
| func `outbox_notify()`  | `postgres`  ← drift       |
| funcs enforce_parent_tenancy / provision_claim / reject_mutation / touch_updated_at | `postgres` ← residual drift |

`CREATE OR REPLACE FUNCTION` requires ownership of the function, so applying
0044 as `k8squad` failed with `ERROR: must be owner of function outbox_guard`.
This is hand-seeded-DB drift (dbmigrate.go notes preview/system ledgers were
"seeded by hand"): the trigger functions were created by `postgres` while the
tables are owned by the app role. A fresh chain never hits this (one role owns
everything), which is why the dbmigrate integration lane and the PG16 self-check
lane were green — the drift is DB-state-specific, not a migration defect.

## Repair applied to preview (2026-11-16, as superuser via the CNPG pod)
```sql
ALTER FUNCTION coord.outbox_guard()  OWNER TO k8squad;
ALTER FUNCTION coord.outbox_notify() OWNER TO k8squad;
```
Safe: these are SECURITY INVOKER trigger functions — owner change does not alter
runtime behavior; it only restores the intended invariant (coord objects owned
by the app/migrate role, matching the tables). Verified: 0044 then applies
end-to-end as the `k8squad` DSN role (guard REPLACE + prune CREATE + REVOKE +
partial index) with no error. The migrate hook will pass on the next deploy.

## Production roll (k8squad-system) — NO ownership repair needed
k8squad-system DB (`k8squad-db`) ledger is at **0042** (behind; apiserver pinned
`e55d0d38` per ISI-7276 predates 0043). There, `coord.outbox` table AND
`outbox_guard`/`outbox_notify` are ALL owned by **`app`** (consistent — no
drift). When the coordinated roll lifts the apiserver pin, system's migrate will
run 0043 then 0044 as `app` and both succeed. So the production roll does not
need the ownership ALTER; only preview did.

## Residual (out of ISI-7484 scope)
The other four `coord` functions on preview (enforce_parent_tenancy,
provision_claim, reject_mutation, touch_updated_at) remain `postgres`-owned. Any
future migration that CREATE-OR-REPLACEs one of them as `k8squad` will hit the
same wall. Track as an ISI-6371 follow-up to realign all coord functions on
preview; not required for 0044.
