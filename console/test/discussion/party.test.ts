// ISI-5589 WS-E — party-session projection coverage.
import { describe, expect, it } from "vitest";
import type { Message, PartySession } from "@/lib/discussion/types";
import { KIND_PARTY_START } from "@/lib/discussion/types";
import {
  deriveTakeaways,
  endedTakeaways,
  isPartyStart,
  isTerminalPhase,
  newestTerminalSession,
  partyRounds,
  partySessionView,
  partyVoices,
  phaseLabel,
} from "@/lib/discussion/party";

function session(over: Partial<PartySession> = {}): PartySession {
  return {
    id: "s1",
    threadId: "t1",
    projectId: "ns/proj",
    teamId: "team1",
    startedBy: "human:henrik",
    topicMessageId: "m0",
    round: 0,
    budget: { maxRounds: 3, maxVoicesPerRound: 3, paidRunBudget: 12 },
    paidRunsUsed: 0,
    phase: "active",
    openedAt: "2026-10-08T10:00:00Z",
    closedAt: null,
    ...over,
  };
}

function msg(over: Partial<Message> = {}): Message {
  return {
    id: "m",
    threadId: "t1",
    authorPrincipal: "agent:alice",
    body: "hi",
    createdAt: "2026-10-08T10:05:00Z",
    ...over,
  };
}

describe("party kind + phase helpers", () => {
  it("isPartyStart matches only the party_start kind", () => {
    expect(KIND_PARTY_START).toBe("party_start");
    expect(isPartyStart({ kind: "party_start" })).toBe(true);
    expect(isPartyStart({ kind: "text" })).toBe(false);
    expect(isPartyStart({ kind: undefined })).toBe(false);
  });

  it("isTerminalPhase is true for every non-active phase", () => {
    expect(isTerminalPhase("active")).toBe(false);
    expect(isTerminalPhase("closed")).toBe(true);
    expect(isTerminalPhase("converged")).toBe(true);
    expect(isTerminalPhase("budget_exhausted")).toBe(true);
  });

  it("phaseLabel distinguishes the terminal reasons", () => {
    expect(phaseLabel("active")).toMatch(/progress/i);
    expect(phaseLabel("converged")).toMatch(/converged/i);
    expect(phaseLabel("budget_exhausted")).toMatch(/budget/i);
    expect(phaseLabel("closed")).toMatch(/closed/i);
  });
});

describe("partySessionView", () => {
  it("shows the opening round (counter 0 → round 1) while active", () => {
    const v = partySessionView(session({ round: 0, phase: "active" }));
    expect(v.currentRound).toBe(1);
    expect(v.maxRounds).toBe(3);
    expect(v.roundProgress).toBeCloseTo(1 / 3);
    expect(v.isActive).toBe(true);
    expect(v.isTerminal).toBe(false);
  });

  it("advances the current round with the server counter, clamped to maxRounds", () => {
    expect(partySessionView(session({ round: 2, phase: "active" })).currentRound).toBe(3);
    // counter can momentarily exceed before close; display never exceeds maxRounds
    expect(partySessionView(session({ round: 5, phase: "active" })).currentRound).toBe(3);
  });

  it("reports rounds reached (not +1) for a terminal session", () => {
    const v = partySessionView(session({ round: 3, phase: "closed" }));
    expect(v.currentRound).toBe(3);
    expect(v.isActive).toBe(false);
    expect(v.isTerminal).toBe(true);
    expect(v.isExhausted).toBe(false);
  });

  it("computes the paid-run meter, headroom, and exhausted flag", () => {
    const v = partySessionView(
      session({ paidRunsUsed: 9, phase: "active" }),
    );
    expect(v.remainingPaidRuns).toBe(3); // 12 - 9
    expect(v.paidRunProgress).toBeCloseTo(9 / 12);

    const done = partySessionView(
      session({ paidRunsUsed: 12, phase: "budget_exhausted" }),
    );
    expect(done.remainingPaidRuns).toBe(0);
    expect(done.paidRunProgress).toBe(1);
    expect(done.isExhausted).toBe(true);
    expect(done.isTerminal).toBe(true);
  });

  it("never goes negative or divides by zero on degenerate budgets", () => {
    const v = partySessionView(
      session({
        round: 0,
        paidRunsUsed: 5,
        budget: { maxRounds: 0, maxVoicesPerRound: 0, paidRunBudget: 0 },
      }),
    );
    expect(v.roundProgress).toBe(0);
    expect(v.paidRunProgress).toBe(0);
    expect(v.remainingPaidRuns).toBe(0);
  });
});

describe("partyVoices (client-derived, thread-window correlation)", () => {
  const s = session({
    openedAt: "2026-10-08T10:00:00Z",
    closedAt: "2026-10-08T11:00:00Z",
    phase: "closed",
  });

  it("collects distinct agent voices and counts contributions", () => {
    const voices = partyVoices(s, [
      msg({ id: "a1", authorPrincipal: "alice", authorAgentId: "ag-alice", createdAt: "2026-10-08T10:10:00Z" }),
      msg({ id: "a2", authorPrincipal: "alice", authorAgentId: "ag-alice", createdAt: "2026-10-08T10:20:00Z" }),
      msg({ id: "b1", authorPrincipal: "bob", authorAgentId: "ag-bob", createdAt: "2026-10-08T10:15:00Z" }),
    ]);
    expect(voices).toHaveLength(2);
    // alice (2) before bob (1): ordered by contributions desc
    expect(voices[0]).toMatchObject({ agentId: "ag-alice", contributions: 2 });
    expect(voices[1]).toMatchObject({ agentId: "ag-bob", contributions: 1 });
  });

  it("excludes humans (no authorAgentId)", () => {
    const voices = partyVoices(s, [
      msg({ id: "h1", authorPrincipal: "henrik", authorAgentId: null, createdAt: "2026-10-08T10:05:00Z" }),
      msg({ id: "a1", authorPrincipal: "alice", authorAgentId: "ag-alice", createdAt: "2026-10-08T10:10:00Z" }),
    ]);
    expect(voices).toHaveLength(1);
    expect(voices[0].agentId).toBe("ag-alice");
  });

  it("excludes messages outside the session window", () => {
    const voices = partyVoices(s, [
      msg({ id: "before", authorAgentId: "ag-x", createdAt: "2026-10-08T09:59:00Z" }),
      msg({ id: "after", authorAgentId: "ag-y", createdAt: "2026-10-08T11:30:00Z" }),
      msg({ id: "in", authorAgentId: "ag-z", createdAt: "2026-10-08T10:30:00Z" }),
    ]);
    expect(voices.map((v) => v.agentId)).toEqual(["ag-z"]);
  });

  it("counts to the present for a still-active session (null closedAt)", () => {
    const live = session({ openedAt: "2026-10-08T10:00:00Z", closedAt: null });
    const voices = partyVoices(live, [
      msg({ id: "a1", authorAgentId: "ag-a", createdAt: "2026-10-08T23:00:00Z" }),
    ]);
    expect(voices).toHaveLength(1);
  });

  it("orders ties by principal ascending", () => {
    const voices = partyVoices(s, [
      msg({ id: "z", authorPrincipal: "zoe", authorAgentId: "ag-z", createdAt: "2026-10-08T10:10:00Z" }),
      msg({ id: "a", authorPrincipal: "amy", authorAgentId: "ag-a", createdAt: "2026-10-08T10:11:00Z" }),
    ]);
    expect(voices.map((v) => v.principal)).toEqual(["amy", "zoe"]);
  });
});

describe("deriveTakeaways (from a session object)", () => {
  it("summarizes a terminal session from session + transcript", () => {
    const s = session({
      round: 3,
      paidRunsUsed: 11,
      phase: "converged",
      openedAt: "2026-10-08T10:00:00Z",
      closedAt: "2026-10-08T10:45:00Z",
    });
    const t = deriveTakeaways(s, [
      msg({ id: "a1", authorPrincipal: "alice", authorAgentId: "ag-alice", createdAt: "2026-10-08T10:10:00Z" }),
      msg({ id: "b1", authorPrincipal: "bob", authorAgentId: "ag-bob", createdAt: "2026-10-08T10:20:00Z" }),
      msg({ id: "late", authorAgentId: "ag-late", createdAt: "2026-10-08T12:00:00Z" }), // outside window
    ]);
    expect(t.terminalReasonKnown).toBe(true);
    expect(t.phaseLabel).toMatch(/converged/i);
    expect(t.roundsCompleted).toBe(3);
    expect(t.paidRunsUsed).toBe(11);
    expect(t.paidRunBudget).toBe(12);
    expect(t.voices.map((v) => v.agentId)).toEqual(["ag-alice", "ag-bob"]);
    expect(t.closedAt).toBe("2026-10-08T10:45:00Z");
  });
});

describe("partyRounds (ISI-5613 Gap 1 — server-stamped round grouping)", () => {
  it("groups messages into numbered rounds, opener at round 0", () => {
    const rounds = partyRounds([
      msg({ id: "m0", partySessionId: "s1", partyRound: 0, partyRoundKind: "opener", authorAgentId: null }),
      msg({ id: "f1", partySessionId: "s1", partyRound: 1, partyRoundKind: "round", authorPrincipal: "facil", authorAgentId: "ag-facil" }),
      msg({ id: "v1", partySessionId: "s1", partyRound: 1, partyRoundKind: "round", authorPrincipal: "alice", authorAgentId: "ag-alice" }),
      msg({ id: "f2", partySessionId: "s1", partyRound: 2, partyRoundKind: "round", authorPrincipal: "facil", authorAgentId: "ag-facil" }),
      msg({ id: "v2", partySessionId: "s1", partyRound: 2, partyRoundKind: "round", authorPrincipal: "bob", authorAgentId: "ag-bob" }),
    ]);
    expect(rounds.map((r) => r.round)).toEqual([0, 1, 2]);
    expect(rounds[0].kind).toBe("opener");
    // round 1 holds the facilitator + alice as distinct voices
    expect(rounds[1].voices.map((v) => v.agentId).sort()).toEqual(["ag-alice", "ag-facil"]);
    expect(rounds[2].voices.map((v) => v.agentId).sort()).toEqual(["ag-bob", "ag-facil"]);
  });

  it("skips messages with no party-round linkage (non-party / pre-ISI-5616 wire)", () => {
    expect(partyRounds([msg({ id: "plain" })])).toEqual([]);
    expect(
      partyRounds([msg({ id: "x", partyRound: 2 })]), // round but no sessionId → skipped
    ).toEqual([]);
  });

  it("counts a voice's repeat posts within a round and orders by count then name", () => {
    const rounds = partyRounds([
      msg({ id: "a1", partySessionId: "s1", partyRound: 1, authorPrincipal: "alice", authorAgentId: "ag-alice" }),
      msg({ id: "a2", partySessionId: "s1", partyRound: 1, authorPrincipal: "alice", authorAgentId: "ag-alice" }),
      msg({ id: "z1", partySessionId: "s1", partyRound: 1, authorPrincipal: "zoe", authorAgentId: "ag-zoe" }),
    ]);
    expect(rounds).toHaveLength(1);
    expect(rounds[0].voices[0]).toMatchObject({ agentId: "ag-alice", contributions: 2 });
    expect(rounds[0].voices[1]).toMatchObject({ agentId: "ag-zoe", contributions: 1 });
    expect(rounds[0].messages.map((m) => m.id)).toEqual(["a1", "a2", "z1"]);
  });

  it("separates rounds that belong to different sessions", () => {
    const rounds = partyRounds([
      msg({ id: "b1", partySessionId: "sB", partyRound: 1, authorAgentId: "ag-b" }),
      msg({ id: "a1", partySessionId: "sA", partyRound: 1, authorAgentId: "ag-a" }),
    ]);
    // ordered by session id asc, then round
    expect(rounds.map((r) => r.sessionId)).toEqual(["sA", "sB"]);
  });
});

describe("newestTerminalSession (ISI-5613 Gap 2 — pick the closed debate)", () => {
  it("returns the first terminal session in a newest-first list", () => {
    const closed = session({ id: "new", phase: "converged" });
    const older = session({ id: "old", phase: "closed" });
    expect(newestTerminalSession([closed, older])?.id).toBe("new");
  });

  it("skips a leading active session to find the terminal one", () => {
    const active = session({ id: "live", phase: "active" });
    const closed = session({ id: "done", phase: "budget_exhausted" });
    expect(newestTerminalSession([active, closed])?.id).toBe("done");
  });

  it("returns null when every session is still active, or the list is empty", () => {
    expect(newestTerminalSession([session({ phase: "active" })])).toBeNull();
    expect(newestTerminalSession([])).toBeNull();
  });
});

describe("endedTakeaways (degraded, transcript-only post-close)", () => {
  it("derives voices from the opener onward, without round/budget numbers", () => {
    const opener = msg({ id: "m0", kind: "party_start", authorAgentId: null, createdAt: "2026-10-08T10:00:00Z" });
    const t = endedTakeaways(opener, [
      opener,
      msg({ id: "before", authorAgentId: "ag-pre", createdAt: "2026-10-08T09:50:00Z" }), // before opener
      msg({ id: "a1", authorPrincipal: "alice", authorAgentId: "ag-alice", createdAt: "2026-10-08T10:10:00Z" }),
      msg({ id: "a2", authorPrincipal: "alice", authorAgentId: "ag-alice", createdAt: "2026-10-08T10:12:00Z" }),
    ]);
    expect(t.terminalReasonKnown).toBe(false);
    expect(t.phaseLabel).toMatch(/ended/i);
    expect(t.roundsCompleted).toBeUndefined();
    expect(t.paidRunsUsed).toBeUndefined();
    expect(t.voices.map((v) => v.agentId)).toEqual(["ag-alice"]); // ag-pre excluded (pre-opener)
    expect(t.voices[0].contributions).toBe(2);
  });
});
