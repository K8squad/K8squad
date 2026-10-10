// ISI-5613 (ISI-5589 WS-E follow-up) — the party-session card renders the
// numbered-round breakdown (Gap 1) and the full post-close takeaways from a
// terminal session object (Gap 2), and still degrades to the transcript-only
// takeaways when no session object is reachable.
import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";
import { PartySessionCard } from "@/components/discussion/PartySessionCard";
import type { Message, PartySession } from "@/lib/discussion/types";

afterEach(cleanup);

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
    authorPrincipal: "agent",
    body: "hi",
    createdAt: "2026-10-08T10:05:00Z",
    ...over,
  };
}

const opener = msg({
  id: "m0",
  kind: "party_start",
  authorAgentId: null,
  partySessionId: "s1",
  partyRound: 0,
  partyRoundKind: "opener",
  createdAt: "2026-10-08T10:00:00Z",
});

const roundMessages = [
  opener,
  msg({ id: "f1", authorPrincipal: "facil", authorAgentId: "ag-facil", partySessionId: "s1", partyRound: 1, partyRoundKind: "round" }),
  msg({ id: "v1", authorPrincipal: "alice", authorAgentId: "ag-alice", partySessionId: "s1", partyRound: 1, partyRoundKind: "round" }),
  msg({ id: "f2", authorPrincipal: "facil", authorAgentId: "ag-facil", partySessionId: "s1", partyRound: 2, partyRoundKind: "round" }),
  msg({ id: "v2", authorPrincipal: "bob", authorAgentId: "ag-bob", partySessionId: "s1", partyRound: 2, partyRoundKind: "round" }),
];

describe("PartySessionCard — per-round grouping (Gap 1)", () => {
  it("lists the numbered facilitator rounds with their voices (opener omitted)", () => {
    render(<PartySessionCard session={session({ phase: "active", round: 1 })} opener={opener} messages={roundMessages} />);
    const rounds = screen.getAllByTestId("party-round");
    expect(rounds).toHaveLength(2); // round 0 opener omitted
    expect(rounds[0]).toHaveAttribute("data-round", "1");
    expect(within(rounds[0]).getByText("Round 1")).toBeTruthy();
    // round 1 shows the facilitator + alice
    expect(within(rounds[0]).getByText("facil")).toBeTruthy();
    expect(within(rounds[0]).getByText("alice")).toBeTruthy();
    expect(within(rounds[1]).getByText("bob")).toBeTruthy();
  });

  it("renders no round breakdown when messages carry no round linkage", () => {
    render(
      <PartySessionCard
        session={session({ phase: "active" })}
        opener={null}
        messages={[msg({ id: "plain", authorAgentId: "ag-x" })]}
      />,
    );
    expect(screen.queryByTestId("party-round-breakdown")).toBeNull();
  });
});

describe("PartySessionCard — terminal takeaways (Gap 2)", () => {
  it("shows the real phase/round/paid-run tally from a terminal session object", () => {
    render(
      <PartySessionCard
        session={session({ phase: "converged", round: 2, paidRunsUsed: 7, closedAt: "2026-10-08T11:00:00Z" })}
        opener={opener}
        messages={roundMessages}
      />,
    );
    const outcome = screen.getByTestId("party-outcome");
    expect(outcome.textContent).toMatch(/converged/i);
    expect(outcome.textContent).toMatch(/2 rounds/);
    expect(outcome.textContent).toMatch(/7\/12 paid runs/);
    // and the rounds are still grouped
    expect(screen.getAllByTestId("party-round")).toHaveLength(2);
  });

  it("degrades to transcript-only takeaways (no numbers) when no session object is reachable", () => {
    render(<PartySessionCard session={null} opener={opener} messages={roundMessages} />);
    expect(screen.getByTestId("party-session")).toHaveAttribute("data-phase", "ended");
    const outcome = screen.getByTestId("party-outcome");
    expect(outcome.textContent).toMatch(/ended/i);
    expect(outcome.textContent).not.toMatch(/paid runs/);
    // Gap 1 grouping still works off the surviving transcript tags
    expect(screen.getAllByTestId("party-round")).toHaveLength(2);
  });
});
