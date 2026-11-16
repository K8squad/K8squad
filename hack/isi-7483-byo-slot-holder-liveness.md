# ISI-7483 — BYO endpoint-slot holder liveness (starved multi-seat dispatch)

Parent: ISI-7482 (run-stream / Activity regression). Workstream A.

## Incident (live evidence, k8squad-test, 2026-11-16 08:02–09:08Z)

A single discussion comment dispatched a multi-seat batch; one seat
(`intake-0b62214a`, run uid `7a657934`), acquired the single deepseek
(`https://api.deepseek.com/v1`) slot at ~08:02:52 and failed ~08:12
(sandbox pod gone). Its in-process `EndpointGate` permit was never released:

- Every release site fires from dispatch/driver paths: clean `OnDone`,
  `FailEnter`, `cancelFinish`. An errored follow KEEPS the permit by design
  ("the agent may still be streaming"); the run then terminalized through a
  path that never touches the gate → permanent leak.
- The claim-release paths (`releaseTerminal` / `releaseDead`) released the
  claim row but not the gate permit, so the sweeper stopped even looking at
  the dead holder.
- Four queued seats (work items 8fc6e9c9-r2, 77e1eee7, 195de5ca, 724f9af8)
  sat `WaitingForEndpointSlot` for 47+ minutes, each renewed by the
  ISI-5591 reaper exemption ("exempt from zombie reap, renewing lease to
  hold queue position") with ZERO slot throughput — a liveness inversion:
  dead holder, living waiters.
- Console effect: the discussion working pill's wall-clock expiry
  (`WORKING_TIMEOUT_MS` = 5 min, `expireStale`) flips honestly-queued seats
  to "!agent could not respond".

Mitigation applied live: operator pod restart (gate is in-process; the map
repopulates only for live unsettled laps) — the repro run then executed
(r2 Succeeded; an r3 minted and Succeeded), proving the root cause.

## Fix

1. **Holder liveness backstop (operator)** — `EndpointGate.ReclaimHolders`:
   the heartbeat sweep now walks the gate's holder set every tick and evicts
   holders whose Run CR is terminal or gone (`LiveRuns.TerminalOrGone`).
   Fail-safe: a checker error counts as live (never reclaim on a maybe).
2. **Belt-and-braces** — `releaseTerminal` / `releaseDead` also call
   `ReleaseByRun` so a released claim can never leave a permit behind.
3. **Console: queued ≠ failed** — `expireStale` takes an optional
   `stillLive` probe; `DiscussionRoom` supplies per-agent live-run presence
   (overview feed, where "queued" includes `WaitingForEndpointSlot`). A seat
   provably queued/running is NEVER expired to "could not respond".

## Not in scope here

- N-slot parallelism per endpoint: `ModelEndpoint.MaxConcurrent` already
  flows through `Acquire`; raising the deepseek endpoint's declared
  concurrency is config, not code.
- The 3-week `Running`/`Collecting` zombie `intake-ae44d9df` (zai
  endpoint, unrelated to this starvation) — flagged to ISI-7482 WS-1.

## Verification

- `go test ./pkg/controller/rundrive/... ./cmd/operator/...` — green,
  including new tests: `TestEndpointGateReclaimHolders*`,
  `TestHeartbeatSweepReclaimsDeadEndpointHolders`,
  `TestHeartbeatSweepReclaimFailsSafeOnCheckerError`,
  `TestHeartbeatReleaseGateSlot`.
- `vitest run test/discussion/working.test.ts` — 30/30 green with the new
  `expireStale stillLive (ISI-7483)` block.
- Live: post-restart, all four previously-starved seats reached honest
  terminal states (Succeeded/Failed), none re-starved.
