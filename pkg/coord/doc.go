// Package coord is the K8squad coordination spine: the Go mechanism that turns
// the coordination-record schema (db/migrations/0001_coord_schema.sql, Story 2.1
// / ISI-2191) into safe claim / renew / reclaim / fence / outbox operations.
//
// It is the single most correctness-critical component of v1 (PRD R10 / Arch §6).
// Every state-mutating write is fence-guarded so a zombie holder (a Run that lost
// its lease but is still executing) can never clobber the current holder's work.
//
// The mechanism, pinned to the Architecture §6 statements the design spikes
// falsified (Stories 2.2–2.6, ISI-2192/2193/2194/2196):
//
//   - §6.2 claim: a single-statement `SELECT … FOR UPDATE SKIP LOCKED LIMIT 1`
//     queue-pull, or a targeted acquire of a specific item, both gated by the
//     conditional CAS guard `WHERE holder_principal IS NULL OR lease_expires_at <
//     now()`, both bumping the monotonic fence_token. No application-level lock.
//   - §6.2 renew: the lease heartbeat — `WHERE holder = me AND fence = myFence AND
//     lease_expires_at > now()`. A stale holder cannot renew.
//   - §6.3 reclaim: the crash-safe, fence-before-release protocol — the surviving
//     holder is fenced at the resource layer (reclaim_fenced_at stamped) strictly
//     before the release that bumps the fence, so a survivor's late write loses.
//   - §6.3 fenced write: complete / status writes carry `AND fence_token = myFence
//     AND state = 'claimed'`, so a stale-fence writer and a done item are rejected.
//   - §6.4 idempotent dispatch (the outbox, Story 2.5): an external effect is
//     guarded by a durable per-run marker, so a re-driven dispatch returns the
//     same recorded task id instead of firing the side effect twice.
//
// The chaos gate ISI-2347 (spine-chaos.yml, cases C1–C7) binds to this package
// through the SUT interface via [NewForTest]; that suite runs the guarantees
// against real Postgres with the Go race detector on and is a required CI gate.
package coord
