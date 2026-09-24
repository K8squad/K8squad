"use client";

// lib/tickets/useDispatchWatch.ts — ISI-4879 (S1 of ISI-4853): the headless state machine that
// kills the dead-air gap between "assign to agent" and the first live run card.
//
// Design SoT: ksquad/docs/bmad/ux/isi-4853-run-active-signal/DESIGN-SPEC-ISI-4853.md (NAS);
// board-locked answers (interaction f3d5c62e): surface=BOTH, first label="Queued", degrade 15s/45s.
//
// THE HONEST LADDER (§3 of the story; honesty guard ADR-0013 / ISI-4760 — never claim more than the
// data proves):
//   1 queued      dispatch 200 · DispatchResult.toState=todo — CLIENT-SIDE, instant, no Run row yet
//   2 picking_up  Run row exists · phase Pending/Claiming · SSE assigned/scheduled/sandbox_bound
//   3 working     phase Running · SSE started            ← ONLY here do we ever say "Working…"
//   terminal      phase Succeeded/Failed/Cancelled · SSE ended
//
// OQ4 spike (resolved in this file, documented in the PR): the ticket-detail screen CANNOT observe a
// work-item-initiated Run on `useRunStream` at state 1 — that hook needs a `runId`, and at dispatch
// 200 no Run row exists (operator Intake mints it seconds later). So we bridge 1→2 with a bounded
// poll of the EXISTING `GET /api/runs` listing (which carries `workItemRef` on every row but has NO
// server-side work-item filter — we match client-side), then hand the discovered runId to the SHARED
// `useRunStream` SSE bus (§13 "one bus, no polling"). No backend / schema / token change (FR of S1).
//
// This module is split into a PURE reducer (`reduceDispatchWatch`) + derivations that the unit tests
// drive as the "mock signal source", and a thin hook (`useDispatchWatch`) that wires the real signal
// sources (poll → runId, SSE → lifecycle, wall-clock → degrade) onto that reducer.

import { useEffect, useReducer, useRef } from "react";
import { useRunStream } from "@/lib/useRunStream";
import type { RunPhase } from "@/lib/agents/types";

// ── Public contract (stable regardless of which signal path wins) ──────────────────────────────

export type DispatchState =
  | "queued"
  | "picking_up"
  | "working"
  | "succeeded"
  | "failed";

/** Ring/motion bucket — maps 1:1 to existing globals.css hues; the card (S2) owns the actual tokens. */
export type DispatchTone = "idle" | "pending" | "running" | "blocked";

export type DispatchDegrade = "none" | "soft" | "stalled";

export interface DispatchWatch {
  state: DispatchState;
  label: string;
  tone: DispatchTone;
  degrade: DispatchDegrade;
  runId?: string;
}

// ── Board-locked / tuning constants (interaction f3d5c62e) ─────────────────────────────────────

/** Soft degrade note fires at ~15s while STILL queued (no Run row yet). Board-locked. */
export const DISPATCH_SOFT_MS = 15_000;
/** "Stalled" fires at ~45s while STILL queued. Board-locked. */
export const DISPATCH_STALLED_MS = 45_000;
/** Run-discovery poll cadence — bounded, best-effort; stops the instant a runId is found. */
export const DISPATCH_POLL_INTERVAL_MS = 2_000;
/** Hard cap on the discovery poll so a run that never appears cannot leak a forever-timer. */
export const DISPATCH_POLL_CAP_MS = 120_000;

// ── The state machine (pure — the unit tests' signal sink) ─────────────────────────────────────

/** Internal machine state. `label`/`tone` are DERIVED (see below), not stored, so they can't drift. */
export interface DispatchMachine {
  state: DispatchState;
  degrade: DispatchDegrade;
  runId?: string;
}

/**
 * A signal from one of the real sources — or, in tests, from the mock signal source:
 *  - `run_discovered`  the discovery poll found the Run row for this work item (→ at least picking_up)
 *  - `phase`           a Run phase snapshot (from the poll row) — the honesty-authoritative source
 *  - `lifecycle`       a named SSE milestone off the shared run stream
 *  - `degrade`         a wall-clock timer fired while still queued
 *  - `reset`           a fresh dispatch (new work item) — rearm to queued
 */
export type WatchSignal =
  | { type: "run_discovered"; runId: string }
  | { type: "phase"; phase: RunPhase | string }
  | {
      type: "lifecycle";
      name: "assigned" | "scheduled" | "sandbox_bound" | "started" | "ended";
    }
  | { type: "degrade"; level: "soft" | "stalled" }
  | { type: "reset" };

/** Monotonic rank — the ladder only ever moves forward (a late/duplicate signal can't regress it). */
const RANK: Record<DispatchState, number> = {
  queued: 1,
  picking_up: 2,
  working: 3,
  succeeded: 4,
  failed: 4,
};

const isTerminal = (s: DispatchState): boolean => RANK[s] === 4;

/** The state a phase snapshot maps to. Paused = a Run row exists but is not Running → picking_up. */
function stateForPhase(phase: string): DispatchState | null {
  switch (phase) {
    case "Pending":
    case "Claiming":
    case "Paused":
      return "picking_up";
    case "Running":
      return "working";
    case "Succeeded":
      return "succeeded";
    case "Failed":
    case "Cancelled":
      return "failed";
    default:
      return null;
  }
}

/** The state a named SSE lifecycle milestone maps to. `ended` = generic finish → treat as succeeded
 *  UNLESS a Failed/Cancelled phase already resolved us (that arrives first and freezes terminal). */
function stateForLifecycle(name: string): DispatchState | null {
  switch (name) {
    case "assigned":
    case "scheduled":
    case "sandbox_bound":
      return "picking_up";
    case "started":
      return "working";
    case "ended":
      return "succeeded";
    default:
      return null;
  }
}

export function initialDispatchMachine(): DispatchMachine {
  return { state: "queued", degrade: "none" };
}

/**
 * The pure transition. Honesty guard: `working` is reachable ONLY via phase `Running` or SSE
 * `started` — never from `assigned`/`Claiming`/etc. Terminal states are frozen. Degrade is orthogonal
 * and only meaningful while still `queued`; any forward advance clears it.
 */
export function reduceDispatchWatch(
  m: DispatchMachine,
  sig: WatchSignal,
): DispatchMachine {
  if (sig.type === "reset") return initialDispatchMachine();

  // Once terminal, the card is settled — ignore all further signals (no zombie transitions).
  if (isTerminal(m.state)) return m;

  if (sig.type === "degrade") {
    if (m.state !== "queued") return m; // degrade applies ONLY while still queued
    if (m.degrade === sig.level) return m;
    return { ...m, degrade: sig.level };
  }

  if (sig.type === "run_discovered") {
    // A Run row exists ⇒ at least picking_up, and we now own the runId for the SSE hand-off.
    const next: DispatchMachine = { ...m, runId: sig.runId };
    if (RANK["picking_up"] > RANK[m.state]) {
      next.state = "picking_up";
      next.degrade = "none";
    }
    return next;
  }

  const target =
    sig.type === "phase"
      ? stateForPhase(sig.phase)
      : stateForLifecycle(sig.name);
  if (!target) return m; // unknown phase/event — render only what we understand
  if (RANK[target] < RANK[m.state]) return m; // monotonic — never regress the ladder

  return { state: target, degrade: "none", runId: m.runId };
}

// ── Presentation derivations (pure) ────────────────────────────────────────────────────────────

/** Honest per-state label (§3). "Working…" appears ONLY at state 3. */
export function dispatchLabel(state: DispatchState): string {
  switch (state) {
    case "queued":
      return "Queued";
    case "picking_up":
      return "Picking up…";
    case "working":
      return "Working…";
    case "succeeded":
      return "Finished";
    case "failed":
      return "Failed";
  }
}

/** Ring/motion bucket (§3): idle grey → paused amber → running green → terminal green/rose. */
export function dispatchTone(state: DispatchState): DispatchTone {
  switch (state) {
    case "queued":
      return "idle";
    case "picking_up":
      return "pending";
    case "working":
    case "succeeded":
      return "running";
    case "failed":
      return "blocked";
  }
}

/** Compose the public view from the machine. */
export function toDispatchWatch(m: DispatchMachine): DispatchWatch {
  return {
    state: m.state,
    label: dispatchLabel(m.state),
    tone: dispatchTone(m.state),
    degrade: m.degrade,
    runId: m.runId,
  };
}

// ── Run-discovery bridge (the OQ4 poll) ────────────────────────────────────────────────────────

/** The subset of a `GET /api/runs` row (apiserver RunListItem) the bridge reads. */
interface RunListRow {
  id?: string;
  phase?: string;
  workItemRef?: string;
  agents?: string[];
  startedAt?: string | null;
}

/**
 * Pick the Run row for this dispatch out of a `GET /api/runs` array. Matches on `workItemRef`, prefers
 * a row whose `agents` include the dispatched agent (disambiguates a re-assign to a different agent),
 * and takes the newest by `startedAt` (a just-minted Pending run may have none → falls back to order).
 */
export function pickDispatchRun(
  rows: RunListRow[],
  workItemId: string,
  agent: string,
): RunListRow | null {
  const forItem = rows.filter(
    (r) => r.workItemRef === workItemId && typeof r.id === "string" && r.id,
  );
  if (forItem.length === 0) return null;
  const matchesAgent = forItem.filter((r) => (r.agents ?? []).includes(agent));
  const pool = matchesAgent.length > 0 ? matchesAgent : forItem;
  return pool.reduce((newest, r) => {
    const a = r.startedAt ? Date.parse(r.startedAt) : NaN;
    const b = newest.startedAt ? Date.parse(newest.startedAt) : NaN;
    if (Number.isNaN(a)) return newest;
    if (Number.isNaN(b)) return r;
    return a > b ? r : newest;
  }, pool[0]);
}

type LifecycleName = "assigned" | "scheduled" | "sandbox_bound" | "started" | "ended";

/** Extract the named SSE milestone from a shared-stream event (useRunStream stamps summary
 *  as `"{name}"` or `"{name}: {step}"` on kind LIFECYCLE). */
function lifecycleName(summary: string | undefined): LifecycleName | null {
  if (!summary) return null;
  const head = summary.split(":")[0].trim();
  switch (head) {
    case "assigned":
    case "scheduled":
    case "sandbox_bound":
    case "started":
    case "ended":
      return head;
    default:
      return null;
  }
}

// ── The hook ───────────────────────────────────────────────────────────────────────────────────

/**
 * Watch a dispatch through the honest ladder. Seeded the instant `workItemId` becomes non-null (the
 * caller sets it on dispatch 200) — it emits `queued` SYNCHRONOUSLY, then advances only on real
 * signals. Returns `null` when no dispatch is in flight so callers fall back to their static UI.
 *
 * @param workItemId the dispatched work item's id (null/"" ⇒ inert)
 * @param agent      the dispatched agent NAME (disambiguates the Run row on a re-assign)
 */
export function useDispatchWatch(
  workItemId: string | null,
  agent: string,
): DispatchWatch | null {
  const active = !!workItemId;
  const [machine, dispatch] = useReducer(
    reduceDispatchWatch,
    undefined,
    initialDispatchMachine,
  );

  // Rearm synchronously when a NEW dispatch starts (workItemId changed) — so the first render after a
  // dispatch already reads `queued`, never a stale prior ladder. (React "reset state on prop change".)
  const prevId = useRef<string | null>(workItemId);
  if (prevId.current !== workItemId) {
    prevId.current = workItemId;
    dispatch({ type: "reset" });
  }

  const terminal = isTerminal(machine.state);
  const haveRun = !!machine.runId;

  // Degrade timers: fire soft@15s / stalled@45s, but ONLY while still queued. Torn down on advance,
  // on a new dispatch, and on unmount — the reducer additionally ignores degrade once past queued.
  useEffect(() => {
    if (!active || machine.state !== "queued") return;
    const soft = setTimeout(
      () => dispatch({ type: "degrade", level: "soft" }),
      DISPATCH_SOFT_MS,
    );
    const stalled = setTimeout(
      () => dispatch({ type: "degrade", level: "stalled" }),
      DISPATCH_STALLED_MS,
    );
    return () => {
      clearTimeout(soft);
      clearTimeout(stalled);
    };
  }, [active, machine.state, workItemId]);

  // Run-discovery poll (OQ4 bridge): while active, not terminal, and no runId yet, poll the existing
  // listing until the Run row for this work item appears, then STOP (hand off to SSE below). Bounded
  // by DISPATCH_POLL_CAP_MS so a never-minted run can't leak an interval.
  useEffect(() => {
    if (!active || terminal || haveRun || !workItemId) return;
    let cancelled = false;
    const startedAt = Date.now();
    let timer: ReturnType<typeof setTimeout> | null = null;

    const poll = async () => {
      try {
        const res = await fetch("/api/runs", {
          headers: { accept: "application/json" },
          cache: "no-store",
        });
        if (res.ok) {
          const body = (await res.json()) as unknown;
          const rows = Array.isArray(body) ? (body as RunListRow[]) : [];
          const row = pickDispatchRun(rows, workItemId, agent);
          if (row && row.id && !cancelled) {
            dispatch({ type: "run_discovered", runId: row.id });
            if (row.phase) dispatch({ type: "phase", phase: row.phase });
            return; // found — stop polling, SSE takes over
          }
        }
      } catch {
        // best-effort bridge — swallow and retry until the cap
      }
      if (cancelled) return;
      if (Date.now() - startedAt >= DISPATCH_POLL_CAP_MS) return; // bounded
      timer = setTimeout(poll, DISPATCH_POLL_INTERVAL_MS);
    };
    poll();

    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [active, terminal, haveRun, workItemId, agent]);

  // SSE hand-off: once a runId is known, ride the SHARED run stream for live lifecycle milestones.
  // useRunStream no-ops on an empty id, so this is inert until discovery lands a runId.
  const { events } = useRunStream(haveRun ? machine.runId! : "");
  useEffect(() => {
    if (!haveRun) return;
    for (const ev of events) {
      if (ev.kind !== "LIFECYCLE") continue;
      const name = lifecycleName(ev.summary);
      if (name) dispatch({ type: "lifecycle", name });
    }
  }, [events, haveRun]);

  if (!active) return null;
  return toDispatchWatch(machine);
}
