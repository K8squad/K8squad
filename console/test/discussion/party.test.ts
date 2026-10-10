// ISI-5589 WS-E — party-session projection coverage.
import { describe, expect, it } from "vitest";
import type { Message, PartySession } from "@/lib/discussion/types";
import { KIND_PARTY_START } from "@/lib/discussion/types";
import {
  deriveTakeaways,
  endedTakeaways,
  isPartyStart,
  isTerminalPhase,
  partySessionView,
  partyTurns,
  partyVoices,
  phaseLabel,
} from "@/lib/discussion/party";
import type { PartyTurnMessage } from "@/lib/discussion/party";

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
    expect(
      partySessionView(session({ round: 2, phase: "active" })).currentRound,
    ).toBe(3);
    // counter can momentarily exceed before close; display never exceeds maxRounds
    expect(
      partySessionView(session({ round: 5, phase: "active" })).currentRound,
    ).toBe(3);
  });

  it("reports rounds reached (not +1) for a terminal session", () => {
    const v = partySessionView(session({ round: 3, phase: "closed" }));
    expect(v.currentRound).toBe(3);
    expect(v.isActive).toBe(false);
    expect(v.isTerminal).toBe(true);
    expect(v.isExhausted).toBe(false);
  });

  it("computes the paid-run meter, headroom, and exhausted flag", () => {
    const v = partySessionView(session({ paidRunsUsed: 9, phase: "active" }));
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
      msg({
        id: "a1",
        authorPrincipal: "alice",
        authorAgentId: "ag-alice",
        createdAt: "2026-10-08T10:10:00Z",
      }),
      msg({
        id: "a2",
        authorPrincipal: "alice",
        authorAgentId: "ag-alice",
        createdAt: "2026-10-08T10:20:00Z",
      }),
      msg({
        id: "b1",
        authorPrincipal: "bob",
        authorAgentId: "ag-bob",
        createdAt: "2026-10-08T10:15:00Z",
      }),
    ]);
    expect(voices).toHaveLength(2);
    // alice (2) before bob (1): ordered by contributions desc
    expect(voices[0]).toMatchObject({ agentId: "ag-alice", contributions: 2 });
    expect(voices[1]).toMatchObject({ agentId: "ag-bob", contributions: 1 });
  });

  it("excludes humans (no authorAgentId)", () => {
    const voices = partyVoices(s, [
      msg({
        id: "h1",
        authorPrincipal: "henrik",
        authorAgentId: null,
        createdAt: "2026-10-08T10:05:00Z",
      }),
      msg({
        id: "a1",
        authorPrincipal: "alice",
        authorAgentId: "ag-alice",
        createdAt: "2026-10-08T10:10:00Z",
      }),
    ]);
    expect(voices).toHaveLength(1);
    expect(voices[0].agentId).toBe("ag-alice");
  });

  it("excludes messages outside the session window", () => {
    const voices = partyVoices(s, [
      msg({
        id: "before",
        authorAgentId: "ag-x",
        createdAt: "2026-10-08T09:59:00Z",
      }),
      msg({
        id: "after",
        authorAgentId: "ag-y",
        createdAt: "2026-10-08T11:30:00Z",
      }),
      msg({
        id: "in",
        authorAgentId: "ag-z",
        createdAt: "2026-10-08T10:30:00Z",
      }),
    ]);
    expect(voices.map((v) => v.agentId)).toEqual(["ag-z"]);
  });

  it("counts to the present for a still-active session (null closedAt)", () => {
    const live = session({ openedAt: "2026-10-08T10:00:00Z", closedAt: null });
    const voices = partyVoices(live, [
      msg({
        id: "a1",
        authorAgentId: "ag-a",
        createdAt: "2026-10-08T23:00:00Z",
      }),
    ]);
    expect(voices).toHaveLength(1);
  });

  it("orders ties by principal ascending", () => {
    const voices = partyVoices(s, [
      msg({
        id: "z",
        authorPrincipal: "zoe",
        authorAgentId: "ag-z",
        createdAt: "2026-10-08T10:10:00Z",
      }),
      msg({
        id: "a",
        authorPrincipal: "amy",
        authorAgentId: "ag-a",
        createdAt: "2026-10-08T10:11:00Z",
      }),
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
      msg({
        id: "a1",
        authorPrincipal: "alice",
        authorAgentId: "ag-alice",
        createdAt: "2026-10-08T10:10:00Z",
      }),
      msg({
        id: "b1",
        authorPrincipal: "bob",
        authorAgentId: "ag-bob",
        createdAt: "2026-10-08T10:20:00Z",
      }),
      msg({
        id: "late",
        authorAgentId: "ag-late",
        createdAt: "2026-10-08T12:00:00Z",
      }), // outside window
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

describe("endedTakeaways (degraded, transcript-only post-close)", () => {
  it("derives voices from the opener onward, without round/budget numbers", () => {
    const opener = msg({
      id: "m0",
      kind: "party_start",
      authorAgentId: null,
      createdAt: "2026-10-08T10:00:00Z",
    });
    const t = endedTakeaways(opener, [
      opener,
      msg({
        id: "before",
        authorAgentId: "ag-pre",
        createdAt: "2026-10-08T09:50:00Z",
      }), // before opener
      msg({
        id: "a1",
        authorPrincipal: "alice",
        authorAgentId: "ag-alice",
        createdAt: "2026-10-08T10:10:00Z",
      }),
      msg({
        id: "a2",
        authorPrincipal: "alice",
        authorAgentId: "ag-alice",
        createdAt: "2026-10-08T10:12:00Z",
      }),
    ]);
    expect(t.terminalReasonKnown).toBe(false);
    expect(t.phaseLabel).toMatch(/ended/i);
    expect(t.roundsCompleted).toBeUndefined();
    expect(t.paidRunsUsed).toBeUndefined();
    expect(t.voices.map((v) => v.agentId)).toEqual(["ag-alice"]); // ag-pre excluded (pre-opener)
    expect(t.voices[0].contributions).toBe(2);
  });
});

describe("partyTurns — sequential turn rendering (ISI-5640)", () => {
  const sess = { openedAt: "2026-10-08T10:00:00Z", closedAt: null };

  function turn(over: Partial<PartyTurnMessage> = {}): PartyTurnMessage {
    return {
      id: "m",
      authorPrincipal: "agent:alice",
      authorAgentId: "ag-alice",
      createdAt: "2026-10-08T10:05:00Z",
      ...over,
    };
  }

  it("orders agent turns in dispatch (createdAt) order with 1-based indices", () => {
    const turns = partyTurns(sess, [
      turn({
        id: "b",
        authorAgentId: "ag-b",
        createdAt: "2026-10-08T10:06:00Z",
      }),
      turn({
        id: "a",
        authorAgentId: "ag-a",
        createdAt: "2026-10-08T10:05:00Z",
      }),
      turn({
        id: "c",
        authorAgentId: "ag-c",
        createdAt: "2026-10-08T10:07:00Z",
      }),
    ]);
    expect(turns.map((t) => t.messageId)).toEqual(["a", "b", "c"]);
    expect(turns.map((t) => t.turnIndex)).toEqual([1, 2, 3]);
    expect(turns.map((t) => t.agentId)).toEqual(["ag-a", "ag-b", "ag-c"]);
  });

  it("excludes humans and messages outside the session window", () => {
    const turns = partyTurns(sess, [
      turn({ id: "human", authorAgentId: null }), // human post
      turn({
        id: "pre",
        authorAgentId: "ag-x",
        createdAt: "2026-10-08T09:59:00Z",
      }), // pre-open
      turn({
        id: "ok",
        authorAgentId: "ag-ok",
        createdAt: "2026-10-08T10:05:00Z",
      }),
    ]);
    expect(turns.map((t) => t.messageId)).toEqual(["ok"]);
    expect(turns[0].turnIndex).toBe(1);
  });

  it("respects the session close boundary", () => {
    const closed = {
      openedAt: "2026-10-08T10:00:00Z",
      closedAt: "2026-10-08T10:10:00Z",
    };
    const turns = partyTurns(closed, [
      turn({
        id: "in",
        authorAgentId: "ag-in",
        createdAt: "2026-10-08T10:05:00Z",
      }),
      turn({
        id: "after",
        authorAgentId: "ag-after",
        createdAt: "2026-10-08T10:11:00Z",
      }),
    ]);
    expect(turns.map((t) => t.messageId)).toEqual(["in"]);
  });

  it("tags each turn with its round when the ISI-5616 wire is present", () => {
    const turns = partyTurns(sess, [
      turn({
        id: "r0",
        authorAgentId: "ag-a",
        createdAt: "2026-10-08T10:05:00Z",
        partyRound: 0,
      }),
      turn({
        id: "r1",
        authorAgentId: "ag-b",
        createdAt: "2026-10-08T10:06:00Z",
        partyRound: 1,
      }),
    ]);
    expect(turns.map((t) => t.round)).toEqual([0, 1]);
  });

  it("leaves round undefined when the wire is absent (still sequenced)", () => {
    const turns = partyTurns(sess, [
      turn({
        id: "a",
        authorAgentId: "ag-a",
        createdAt: "2026-10-08T10:05:00Z",
      }),
      turn({
        id: "b",
        authorAgentId: "ag-b",
        createdAt: "2026-10-08T10:06:00Z",
      }),
    ]);
    expect(turns.every((t) => t.round === undefined)).toBe(true);
    expect(turns.map((t) => t.turnIndex)).toEqual([1, 2]);
  });

  it("breaks createdAt ties deterministically by message id", () => {
    const turns = partyTurns(sess, [
      turn({
        id: "m2",
        authorAgentId: "ag-2",
        createdAt: "2026-10-08T10:05:00Z",
      }),
      turn({
        id: "m1",
        authorAgentId: "ag-1",
        createdAt: "2026-10-08T10:05:00Z",
      }),
    ]);
    expect(turns.map((t) => t.messageId)).toEqual(["m1", "m2"]);
  });
});
