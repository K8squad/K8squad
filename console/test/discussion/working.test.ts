import { describe, it, expect } from "vitest";
import {
  applyLivePresence,
  applyReply,
  dispatchTargets,
  expireStale,
  groupByMessage,
  hasActiveWork,
  resolveActiveRunWorkItem,
  seedWatches,
  watchKey,
  type DispatchRosterAgent,
  type DispatchWatch,
} from "@/lib/discussion/working";
import type { Message } from "@/lib/discussion/types";

const ROSTER: DispatchRosterAgent[] = [
  { id: "john", name: "john", status: "idle" },
  { id: "bmad-pm", name: "bmad-pm", status: "running" },
  { id: "napping", name: "napping", status: "paused" },
  { id: "stuck", name: "stuck", status: "blocked" },
];

function agentMsg(
  overrides: Partial<Message> & { authorPrincipal: string },
): Message {
  return {
    id: "reply-1",
    threadId: "t",
    parentId: null,
    authorAgentId: "uid-xyz",
    authorRunId: "run-1",
    body: "on it",
    createdAt: "2026-09-29T10:00:00Z",
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// dispatchTargets — mirrors the backend's dispatch-on-mention resolution.
// ---------------------------------------------------------------------------

describe("dispatchTargets", () => {
  it("resolves each @-mentioned dispatchable roster agent in a party post", () => {
    expect(
      dispatchTargets("hey @john and @bmad-pm please look", undefined, ROSTER),
    ).toEqual(["john", "bmad-pm"]);
  });

  it("broadcasts a HUMAN bare-party post to the whole dispatchable roster (ISI-5265)", () => {
    // No @-mention + party + human author ⇒ the backend broadcasts to every
    // dispatchable roster agent (paused/blocked skipped), so the affordance must
    // show a working row for each. Default author is human.
    expect(
      dispatchTargets("just a note to the room", undefined, ROSTER),
    ).toEqual(["john", "bmad-pm"]);
  });

  it("an AGENT bare-party post dispatches nobody (loop-safety)", () => {
    expect(
      dispatchTargets("just a note to the room", undefined, ROSTER, {
        authoredByAgent: true,
      }),
    ).toEqual([]);
  });

  it("skips paused and blocked agents (opt-out guardrail)", () => {
    expect(dispatchTargets("@napping @stuck @john", undefined, ROSTER)).toEqual(
      ["john"],
    );
  });

  it("matches case-insensitively", () => {
    expect(dispatchTargets("@JOHN hi", undefined, ROSTER)).toEqual(["john"]);
  });

  it("does not match an @-token that is a prefix/substring of a name", () => {
    // "@joh" must not match "john"; "@johnny" must not either. Agent-authored so
    // the bare-party broadcast path is suppressed, isolating the mention parser.
    expect(
      dispatchTargets("@joh @johnny", undefined, ROSTER, {
        authoredByAgent: true,
      }),
    ).toEqual([]);
  });

  it("does not fire on an email-like a@b", () => {
    // `john@bmad-pm.dev` must not parse as @-mentioning anyone. Agent-authored so
    // the broadcast path does not mask the parse result.
    expect(
      dispatchTargets("mail john@bmad-pm.dev", undefined, ROSTER, {
        authoredByAgent: true,
      }),
    ).toEqual([]);
  });

  it("a direct post dispatches ONLY the target, ignoring body mentions", () => {
    expect(dispatchTargets("@bmad-pm too", "direct:john", ROSTER)).toEqual([
      "john",
    ]);
  });

  it("a direct post to a paused agent dispatches nobody", () => {
    expect(dispatchTargets("hi", "direct:napping", ROSTER)).toEqual([]);
  });

  it("caps an explicit @-mention party fan-out at 5", () => {
    const big: DispatchRosterAgent[] = Array.from({ length: 8 }, (_, i) => ({
      id: `a${i}`,
      name: `a${i}`,
      status: "idle",
    }));
    const body = big.map((a) => `@${a.name}`).join(" ");
    expect(dispatchTargets(body, undefined, big)).toHaveLength(5);
  });

  it("caps a human bare-party broadcast at 25", () => {
    const big: DispatchRosterAgent[] = Array.from({ length: 30 }, (_, i) => ({
      id: `a${i}`,
      name: `a${i}`,
      status: "idle",
    }));
    // No @-mention ⇒ broadcast; the whole 30-agent roster is dispatchable but
    // the broadcast cap bounds the affordance at 25.
    expect(
      dispatchTargets("what does everyone think?", undefined, big),
    ).toHaveLength(25);
  });

  it("broadcast skips paused/blocked agents (opt-out holds on the broadcast path)", () => {
    expect(dispatchTargets("room, thoughts?", undefined, ROSTER)).toEqual([
      "john",
      "bmad-pm",
    ]);
  });

  it("an explicit @-mention still targets only mentioned agents, not a broadcast", () => {
    // A party post that DOES @-mention stays on the mention path (cap 5), never
    // falling through to the whole-room broadcast.
    expect(dispatchTargets("@john only you", undefined, ROSTER)).toEqual([
      "john",
    ]);
  });
});

// ---------------------------------------------------------------------------
// seedWatches — idempotent optimistic seed.
// ---------------------------------------------------------------------------

describe("seedWatches", () => {
  it("seeds one working watch per agent", () => {
    const w = seedWatches([], {
      messageId: "m1",
      agentNames: ["john", "bmad-pm"],
      now: 1000,
    });
    expect(w).toHaveLength(2);
    expect(w.every((x) => x.phase === "working")).toBe(true);
    expect(w[0].key).toBe(watchKey("m1", "john"));
    expect(w[0].since).toBe(1000);
  });

  it("is idempotent by (message, agent) key and preserves prior state", () => {
    const first = seedWatches([], {
      messageId: "m1",
      agentNames: ["john"],
      now: 1000,
    });
    const replied = first.map((x) => ({ ...x, phase: "replied" as const }));
    const again = seedWatches(replied, {
      messageId: "m1",
      agentNames: ["john"],
      now: 5000,
    });
    expect(again).toHaveLength(1);
    expect(again[0].phase).toBe("replied"); // not reset to working
    expect(again[0].since).toBe(1000);
  });

  it("no agents ⇒ no watches added", () => {
    expect(
      seedWatches([], { messageId: "m1", agentNames: [], now: 1 }),
    ).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// applyReply — resolve a working watch when the agent's reply lands.
// ---------------------------------------------------------------------------

describe("applyReply", () => {
  const base: DispatchWatch[] = [
    {
      key: watchKey("m1", "john"),
      agentName: "john",
      messageId: "m1",
      phase: "working",
      since: 0,
    },
    {
      key: watchKey("m1", "bmad-pm"),
      agentName: "bmad-pm",
      messageId: "m1",
      phase: "working",
      since: 0,
    },
  ];

  it("resolves by author principal (case-insensitive)", () => {
    const out = applyReply(base, agentMsg({ authorPrincipal: "John" }));
    expect(out.find((w) => w.agentName === "john")?.phase).toBe("replied");
    expect(out.find((w) => w.agentName === "bmad-pm")?.phase).toBe("working");
  });

  it("ignores a human-authored message", () => {
    const human = agentMsg({ authorPrincipal: "john", authorAgentId: null });
    expect(applyReply(base, human)).toEqual(base);
  });

  it("single-active fallback resolves the lone watch when principal is unknown", () => {
    const one: DispatchWatch[] = [base[0]];
    const out = applyReply(one, agentMsg({ authorPrincipal: "someone-else" }));
    expect(out[0].phase).toBe("replied");
  });

  it("does NOT guess when principal is unknown and multiple are active", () => {
    const out = applyReply(base, agentMsg({ authorPrincipal: "someone-else" }));
    expect(out).toEqual(base);
  });

  it("no active watches ⇒ no-op", () => {
    const replied = base.map((w) => ({ ...w, phase: "replied" as const }));
    expect(applyReply(replied, agentMsg({ authorPrincipal: "john" }))).toEqual(
      replied,
    );
  });
});

// ---------------------------------------------------------------------------
// expireStale — failure timeout.
// ---------------------------------------------------------------------------

describe("expireStale", () => {
  const w: DispatchWatch[] = [
    {
      key: "k1",
      agentName: "john",
      messageId: "m1",
      phase: "working",
      since: 0,
    },
  ];

  it("marks a stale working watch waiting once the timeout elapses (ISI-7772 honest seat)", () => {
    expect(expireStale(w, 60_000, 60_000)[0].phase).toBe("waiting");
  });

  it("leaves a fresh working watch alone", () => {
    expect(expireStale(w, 59_999, 60_000)[0].phase).toBe("working");
  });

  it("never touches a replied watch", () => {
    const replied = [{ ...w[0], phase: "replied" as const }];
    expect(expireStale(replied, 10 ** 9, 60_000)[0].phase).toBe("replied");
  });
});

describe("groupByMessage / hasActiveWork", () => {
  it("groups watches by triggering message id", () => {
    const ws: DispatchWatch[] = [
      { key: "a", agentName: "x", messageId: "m1", phase: "working", since: 0 },
      { key: "b", agentName: "y", messageId: "m1", phase: "working", since: 0 },
      { key: "c", agentName: "z", messageId: "m2", phase: "replied", since: 0 },
    ];
    const g = groupByMessage(ws);
    expect(g.m1).toHaveLength(2);
    expect(g.m2).toHaveLength(1);
  });

  it("hasActiveWork reflects any working watch", () => {
    expect(hasActiveWork([])).toBe(false);
    expect(
      hasActiveWork([
        {
          key: "a",
          agentName: "x",
          messageId: "m",
          phase: "replied",
          since: 0,
        },
      ]),
    ).toBe(false);
    expect(
      hasActiveWork([
        {
          key: "a",
          agentName: "x",
          messageId: "m",
          phase: "working",
          since: 0,
        },
      ]),
    ).toBe(true);
  });
});

// ISI-7906 (WS-B of ISI-7893): resolve the interrupt target work-item id from
// the active working watch joined to the overview presence feed by agent name.
describe("resolveActiveRunWorkItem", () => {
  const working: DispatchWatch[] = [
    { key: "a", agentName: "Amelia", messageId: "m1", phase: "working", since: 0 },
  ];

  it("returns the work-item id of an active watch's agent from the presence map", () => {
    expect(
      resolveActiveRunWorkItem(working, {
        Amelia: { workItem: "wi-42" },
      }),
    ).toBe("wi-42");
  });

  it("matches the presence map case-insensitively (lowercased fallback)", () => {
    expect(
      resolveActiveRunWorkItem(working, {
        amelia: { workItem: "wi-7" },
      }),
    ).toBe("wi-7");
  });

  it("returns undefined when the live run exposes no work-item id yet", () => {
    expect(
      resolveActiveRunWorkItem(working, { Amelia: {} }),
    ).toBeUndefined();
  });

  it("ignores non-working watches (only a live run is interruptible)", () => {
    const replied: DispatchWatch[] = [
      { key: "a", agentName: "Amelia", messageId: "m1", phase: "replied", since: 0 },
    ];
    expect(
      resolveActiveRunWorkItem(replied, {
        Amelia: { workItem: "wi-42" },
      }),
    ).toBeUndefined();
  });

  it("returns undefined when there is no presence feed at all", () => {
    expect(resolveActiveRunWorkItem(working, null)).toBeUndefined();
    expect(resolveActiveRunWorkItem(working, undefined)).toBeUndefined();
  });

  it("returns the first resolvable work-item across several active watches", () => {
    const many: DispatchWatch[] = [
      { key: "a", agentName: "NoRun", messageId: "m1", phase: "working", since: 0 },
      { key: "b", agentName: "Winston", messageId: "m1", phase: "working", since: 0 },
    ];
    expect(
      resolveActiveRunWorkItem(many, {
        Winston: { workItem: "wi-9" },
      }),
    ).toBe("wi-9");
  });
});

// ISI-7483 (queued ≠ failed): a timed-out watch whose agent provably still
// holds a live (queued or running) run must NOT expire. ISI-7772: when the
// probe cannot prove liveness, the seat drops to the honest waiting state —
// a timeout is not proof of failure.
describe("expireStale stillLive (ISI-7483)", () => {
  const watch = (agentName: string): DispatchWatch => ({
    key: `m1:${agentName.toLowerCase()}`,
    agentName,
    messageId: "m1",
    phase: "working",
    since: 0,
  });
  const now = 10 * 60_000;
  const timeout = 5 * 60_000;

  it("keeps a timed-out watch when the agent provably still has a live run", () => {
    const out = expireStale([watch("ada")], now, timeout, {
      stillLive: (w) => w.agentName === "ada",
    });
    expect(out[0].phase).toBe("working");
  });

  it("drops a timed-out watch to waiting when the probe says the run is not live", () => {
    const out = expireStale([watch("ada")], now, timeout, {
      stillLive: () => false,
    });
    expect(out[0].phase).toBe("waiting");
  });

  it("drops to the honest waiting seat when no probe is supplied", () => {
    const out = expireStale([watch("ada")], now, timeout);
    expect(out[0].phase).toBe("waiting");
  });

  it("never touches non-working or unexpired watches regardless of the probe", () => {
    const replied: DispatchWatch = { ...watch("ada"), phase: "replied" };
    const fresh = { ...watch("brin"), since: now - 1_000 }; // unexpired
    const out = expireStale([replied, fresh], now, timeout, {
      stillLive: () => false,
    });
    expect(out[0].phase).toBe("replied");
    expect(out[1].phase).toBe("working");
  });
});

// ---------------------------------------------------------------------------
// applyLivePresence — ISI-7772: stamp the real per-agent live-run state.
// ---------------------------------------------------------------------------

describe("applyLivePresence (ISI-7772)", () => {
  const watch = (
    agentName: string,
    live?: "queued" | "running",
  ): DispatchWatch => ({
    key: `m1:${agentName.toLowerCase()}`,
    agentName,
    messageId: "m1",
    phase: "working",
    since: 0,
    live,
  });

  it("stamps queued and running states from the presence map (case-insensitive)", () => {
    const out = applyLivePresence([watch("ada"), watch("Brin")], {
      ada: { state: "queued" },
      brin: { state: "running" },
    });
    expect(out[0].live).toBe("queued");
    expect(out[1].live).toBe("running");
  });

  it("clears the stamp when the agent has no live run", () => {
    const out = applyLivePresence([watch("ada", "running")], {
      other: { state: "running" },
    });
    expect(out[0].live).toBeUndefined();
  });

  it("leaves non-working watches untouched", () => {
    const replied = { ...watch("ada"), phase: "replied" as const };
    const out = applyLivePresence([replied], { ada: { state: "queued" } });
    expect(out[0]).toBe(replied);
  });

  it("returns a copy when there is no presence feed", () => {
    const ws = [watch("ada")];
    expect(applyLivePresence(ws, null)).not.toBe(ws);
    expect(applyLivePresence(ws, null)).toEqual(ws);
  });
});
