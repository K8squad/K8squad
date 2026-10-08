// Dispatch working-state (ISI-5174) — the room's "an agent is working…" affordance.
//
// From ISI-5173 (Henrik): posting an @-mention gave NO visible signal that an
// agent was dispatched and is running, so the room "felt like a forum / personal
// notes". The reply-capability fix (ISI-5138 / deploy ISI-5152) makes agents
// eventually reply, but a run takes time; without an interim affordance the room
// still reads as dead. This module is the PURE state machine behind that
// affordance: given the human's own post and the messages already flowing over
// the ONE 8.2 SSE channel (liveFeed.ts), it tracks a per-(message,agent) working
// entry through `working → replied | failed`.
//
// It is deliberately transport-free and opens NO second status stream (§13 "one
// bus, no polling"): the working state is SEEDED optimistically from the post the
// human just made and RESOLVED by the agent's reply landing in-thread over the
// same channel every other live surface rides, or EXPIRED to a failure state by a
// caller-supplied clock. That honors ISI-5174 AC4 ("reuse the run-status source
// rather than a parallel one") — we add no parallel EventSource — while staying
// FE/UX-only: the reply transport itself is ISI-5138/ISI-5152.
//
// Correlation note: postMessage does not return the minted run id (dispatch is a
// server-side decision keyed by (messageId, agent) AFTER the post commits — see
// internal/discussion/dispatch.go), and the roster DTO carries the agent NAME,
// not the object UID the team-status stream keys by (internal/apiserver/org.go
// AgentStatusDelta.AgentID). So the reliable FE-side signals are (1) the set of
// dispatchable agents the post @-mentions — computed here the same way the
// backend resolves them — and (2) the agent's reply arriving in-thread, matched
// by author identity. Both are pure functions of data the console already holds.

import type { Message } from "./types";

/** The lifecycle of one dispatch working entry surfaced in the room. */
export type WorkingPhase = "working" | "replied" | "failed";

/**
 * One tracked dispatch: a specific agent the human's post @-mentioned, and the
 * phase of its (optimistically assumed) run. `key` is the stable render key
 * `${messageId}:${agentNameLower}` — one entry per (triggering message, agent),
 * mirroring the backend's one-dispatch-per-(messageId, agent) de-dupe guardrail.
 */
export interface DispatchWatch {
  key: string;
  /** The dispatched agent's roster name (the @-mention token / display identity). */
  agentName: string;
  /** The message whose posting dispatched the agent (the render anchor). */
  messageId: string;
  phase: WorkingPhase;
  /** Epoch ms when the watch was seeded — the clock the failure timeout reads. */
  since: number;
}

/** A minimal roster row (name + optional status) — the dispatch-eligibility input. */
export interface DispatchRosterAgent {
  id: string;
  name: string;
  status?: string;
}

// A paused or blocked agent is never auto-dispatched (the backend's opt-out
// guardrail, dispatch.go). We mirror that here so the affordance never promises
// work from an agent the server would skip.
const NON_DISPATCHABLE: ReadonlySet<string> = new Set(["paused", "blocked"]);

// The backend caps an EXPLICIT @-mention post's fan-out at
// maxMentionDispatchPerMessage=5 (dispatch.go rate/cost guardrail). We cap the
// optimistic affordance identically so it never shows more working rows than the
// server would actually dispatch for a mention post.
const MAX_DISPATCH_PER_MESSAGE = 5;

// A whole-room broadcast — a HUMAN party post with NO @-mention (ISI-5265) —
// rides the larger squad-sized cap maxBroadcastDispatchPerMessage=25
// (dispatch.go). We mirror that so a human "ask the room" post shows a working
// row per dispatchable agent (bounded at 25), not zero rows as the stale
// pre-ISI-5265 model did.
const MAX_BROADCAST_PER_MESSAGE = 25;

function isDispatchable(agent: DispatchRosterAgent): boolean {
  const s = (agent.status ?? "").toLowerCase();
  return !NON_DISPATCHABLE.has(s);
}

/** Escape a roster name for use inside a RegExp (names may carry `-` etc.). */
function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** Options for {@link dispatchTargets} — the one bit the backend routes on that
 * the body+audience+roster don't carry: who authored the triggering post. */
export interface DispatchOptions {
  /**
   * True ⇒ the triggering post was authored by an agent (provenance
   * `authorAgentId` present). Governs the bare-party path ONLY: a human
   * bare-party post broadcasts to the room (ISI-5265), an agent bare-party post
   * dispatches nobody (loop-safety). Defaults to false — the seed path only ever
   * fires on the human's own composer post.
   */
  authoredByAgent?: boolean;
}

/** Dispatchable roster agents the body @-mentions, roster order, de-duped.
 * No cap applied here — the caller caps per routing mode. */
function mentionedTargets(
  body: string,
  roster: readonly DispatchRosterAgent[],
): string[] {
  const out: string[] = [];
  for (const agent of roster) {
    if (!isDispatchable(agent)) continue;
    const re = new RegExp(
      `(^|[^A-Za-z0-9_-])@${escapeRegExp(agent.name)}(?![A-Za-z0-9_-])`,
      "i",
    );
    if (re.test(body)) out.push(agent.name);
  }
  return out;
}

/**
 * Resolve the set of agent NAMES a post dispatches, the same way the backend
 * does (dispatch.go). Four routing modes, mirrored exactly:
 *   - `direct:{agentId}` — ONLY that target, body @-mentions ignored;
 *   - party WITH @-mentions — each @-mentioned dispatchable agent, capped at
 *     maxMentionDispatchPerMessage=5;
 *   - party, NO @-mention, HUMAN author — a whole-room BROADCAST to every
 *     dispatchable roster agent, capped at maxBroadcastDispatchPerMessage=25
 *     (ISI-5265: "party" = talk to the whole room; a human at hop 0 is the one
 *     turn allowed to wake the squad);
 *   - party, NO @-mention, AGENT author — nobody (loop-safety: broadcast is a
 *     hop-0 human privilege; an agent bare-party post would N²-loop the squad).
 * Paused/blocked agents are skipped and each fan-out is capped, mirroring the
 * server guardrails. Matching is case-insensitive and de-duped, order preserved.
 *
 * NOTE: the ISI-5283 multi-mention→coordinator routing (a party post resolving
 * 2+ @-mentions dispatches the Team coordinator ONCE, not each agent) is NOT
 * modeled here — the console roster DTO (RosterAgentDTO) carries no coordinator
 * flag, so the FE cannot name the coordinator the server would pick. The working
 * affordance therefore over-shows for that specific case; a wrong name is worse
 * than an extra row, so we keep the mentioned-agent rows rather than guess.
 */
export function dispatchTargets(
  body: string,
  audience: string | undefined,
  roster: readonly DispatchRosterAgent[],
  opts?: DispatchOptions,
): string[] {
  // Direct audience (`direct:{agentId}` — the roster id, which mirrors name):
  // exactly that agent, and only if dispatchable. The body's @-mentions are
  // irrelevant for a direct post (the server dispatches the target alone).
  const direct = audience?.startsWith("direct:")
    ? audience.slice("direct:".length)
    : null;
  if (direct) {
    const target = roster.find(
      (a) => a.id.toLowerCase() === direct.toLowerCase() && isDispatchable(a),
    );
    return target ? [target.name] : [];
  }

  // Party post WITH @-mentions: every dispatchable roster agent the body
  // @-mentions, capped at the per-message mention cap. We test each roster name
  // against the body rather than parsing free tokens, so a name carrying allowed
  // punctuation still matches and a non-roster @token is ignored.
  const mentioned = mentionedTargets(body, roster);
  if (mentioned.length > 0) {
    return mentioned.slice(0, MAX_DISPATCH_PER_MESSAGE);
  }

  // Party post with NO @-mention. ISI-5265: a HUMAN bare-party post broadcasts
  // to the whole dispatchable roster (bounded at the broadcast cap); an AGENT
  // bare-party post dispatches nobody (loop-safety). This is the exact bug the
  // stale pre-ISI-5265 model got wrong — it returned [] for both.
  if (opts?.authoredByAgent) return [];
  const out: string[] = [];
  for (const agent of roster) {
    if (out.length >= MAX_BROADCAST_PER_MESSAGE) break;
    if (isDispatchable(agent)) out.push(agent.name);
  }
  return out;
}

/** The stable per-(message, agent) render key. */
export function watchKey(messageId: string, agentName: string): string {
  return `${messageId}:${agentName.toLowerCase()}`;
}

/**
 * Seed a `working` watch per dispatched agent for a just-posted message,
 * idempotent by key (re-seeding the same (message, agent) never duplicates or
 * resets an already-tracked entry). Input list is not mutated.
 */
export function seedWatches(
  existing: readonly DispatchWatch[],
  seed: { messageId: string; agentNames: readonly string[]; now: number },
): DispatchWatch[] {
  if (seed.agentNames.length === 0) return existing.slice();
  const have = new Set(existing.map((w) => w.key));
  const next = existing.slice();
  for (const name of seed.agentNames) {
    const key = watchKey(seed.messageId, name);
    if (have.has(key)) continue;
    have.add(key);
    next.push({
      key,
      agentName: name,
      messageId: seed.messageId,
      phase: "working",
      since: seed.now,
    });
  }
  return next;
}

/** True when a message was authored by an agent (provenance is server-stamped). */
function isAgentAuthored(m: Message): boolean {
  return typeof m.authorAgentId === "string" && m.authorAgentId !== "";
}

/**
 * Resolve `working` watches when an agent's reply lands in-thread. The reply is
 * matched to a watch by author identity (`authorPrincipal` === the dispatched
 * agent name, case-insensitive) — the reliable FE-side correlation, since the
 * minted run id never reaches the console. As a fallback for the common single
 * @-mention turn, when the author matches no tracked name but EXACTLY ONE watch
 * is still `working`, that lone watch is resolved (a reply arrived, and there is
 * only one thing it can be answering). A human-authored message never resolves
 * anything. Input list is not mutated.
 */
export function applyReply(
  existing: readonly DispatchWatch[],
  message: Message,
): DispatchWatch[] {
  if (!isAgentAuthored(message)) return existing.slice();
  const principal = (message.authorPrincipal ?? "").toLowerCase();
  const active = existing.filter((w) => w.phase === "working");
  if (active.length === 0) return existing.slice();

  const byName = active.filter((w) => w.agentName.toLowerCase() === principal);
  let resolve: ReadonlySet<string>;
  if (byName.length > 0) {
    resolve = new Set(byName.map((w) => w.key));
  } else if (active.length === 1) {
    resolve = new Set([active[0].key]);
  } else {
    return existing.slice(); // ambiguous — let the timeout decide
  }

  return existing.map((w) =>
    resolve.has(w.key) ? { ...w, phase: "replied" as const } : w,
  );
}

/**
 * Expire `working` watches older than `timeoutMs` to `failed` ("agent could not
 * respond"), so the room never leaves a working indicator spinning forever when
 * a run dies or never posts back (ISI-5174 AC3). Pure in `now`: the caller owns
 * the clock (a component timer), which keeps this trivially testable. Input list
 * is not mutated.
 */
export function expireStale(
  existing: readonly DispatchWatch[],
  now: number,
  timeoutMs: number,
): DispatchWatch[] {
  return existing.map((w) =>
    w.phase === "working" && now - w.since >= timeoutMs
      ? { ...w, phase: "failed" as const }
      : w,
  );
}

/** Group watches by their triggering message id, for per-message rendering. */
export function groupByMessage(
  watches: readonly DispatchWatch[],
): Record<string, DispatchWatch[]> {
  const out: Record<string, DispatchWatch[]> = {};
  for (const w of watches) {
    (out[w.messageId] ??= []).push(w);
  }
  return out;
}

/** True when any watch is still active — the room-level "something is working" bit. */
export function hasActiveWork(watches: readonly DispatchWatch[]): boolean {
  return watches.some((w) => w.phase === "working");
}
