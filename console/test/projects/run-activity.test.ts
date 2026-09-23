// test/projects/run-activity.test.ts — ISI-4798: the read-model projections behind
// the ISI-4792 run-detail redesign. Covers the two behaviours the acceptance
// criteria call out — the entry-kind classifier (§3 colour system) and the
// This-run/All scope filter (§2, fixes interleaved runs) — plus the tool parsing
// and lifecycle projection they lean on.

import { describe, it, expect } from "vitest";

import {
  belongsToRun,
  buildExecutionItems,
  buildLifecycle,
  classifyEntry,
  enrichLlm,
  filterByScope,
  foldActivity,
  formatCompactTime,
  parseRunScope,
  parseToolCall,
  partitionByView,
  runScopeLabel,
  type ClassifiedEntry,
} from "@/lib/run-activity";
import type { LLMInteractionWire, RunDetailResponseWire, ThinkingEntryWire } from "@/lib/runs";

function entry(overrides: Partial<ThinkingEntryWire>): ThinkingEntryWire {
  return {
    id: overrides.id ?? "e1",
    // default neutral type → falls through to the generic "thinking" bucket; tests
    // that exercise the execution types set `type` explicitly.
    type: overrides.type ?? "",
    content: overrides.content ?? "hello",
    agent: overrides.agent,
    timestamp: overrides.timestamp ?? "2026-09-22T08:57:00Z",
  };
}

describe("classifyEntry — the 5-kind colour system", () => {
  it("maps a human turn (user:admin) to You with the bare author", () => {
    const c = classifyEntry(entry({ agent: "user:admin", content: "Can you trigger the design?" }));
    expect(c.kind).toBe("you");
    expect(c.author).toBe("admin");
    expect(c.text).toBe("Can you trigger the design?");
  });

  it("maps a neutral agent turn to thinking", () => {
    const c = classifyEntry(entry({ agent: "sam", type: "", content: "reasoning out loud" }));
    expect(c.kind).toBe("thinking");
    expect(c.author).toBe("sam");
  });

  it("maps the P2-A execution types: llm_interaction → llm, tool_use/observation → tool", () => {
    const llm = classifyEntry(entry({ type: "llm_interaction", agent: "claude", content: "hi" }));
    expect(llm.kind).toBe("llm");
    expect(llm.author).toBe("claude");

    const call = classifyEntry(entry({ type: "tool_use", agent: "claude", content: "ls -la" }));
    expect(call.kind).toBe("tool");
    expect(call.toolPhase).toBe("call");

    const result = classifyEntry(entry({ type: "observation", agent: "claude", content: "total 8" }));
    expect(result.kind).toBe("tool");
    expect(result.toolPhase).toBe("result");
  });

  it("keeps a human/operator turn ahead of a mis-typed execution type", () => {
    // Defensive: a real execution entry always carries the model as agent, but a
    // user:/operator agent must never be swallowed into the execution stream.
    expect(classifyEntry(entry({ agent: "user:admin", type: "observation" })).kind).toBe("you");
    expect(classifyEntry(entry({ agent: "ksquad-operator", type: "tool_use" })).kind).toBe("system");
  });

  it("maps type=comment to comment (green)", () => {
    const c = classifyEntry(entry({ agent: "sam", type: "comment", content: "Plan is in 01-plan.md" }));
    expect(c.kind).toBe("comment");
  });

  it("maps the operator to system regardless of type", () => {
    const c = classifyEntry(entry({ agent: "ksquad-operator", type: "comment", content: "Run succeeded" }));
    expect(c.kind).toBe("system");
  });

  it("classifies a tool result off the [tool:…] payload, ahead of the agent", () => {
    const ok = classifyEntry(entry({ agent: "sam", content: "[tool:bash/result(ok)] ls -la" }));
    expect(ok.kind).toBe("tool");
    expect(ok.tool).toEqual({ name: "bash", ok: true });
    expect(ok.text).toBe("ls -la");

    const err = classifyEntry(entry({ agent: "sam", content: "[tool:glob/result(err)] no match" }));
    expect(err.kind).toBe("tool");
    expect(err.tool).toEqual({ name: "glob", ok: false });
  });

  it("honours a type=tool_use entry with no marker", () => {
    const c = classifyEntry(entry({ type: "tool_use", content: "webfetch" }));
    expect(c.kind).toBe("tool");
  });

  it("strips the [run …] scope prefix from the displayed text and records it", () => {
    const c = classifyEntry(entry({ agent: "sam", content: "[run 51c621b4] thinking out loud" }));
    expect(c.runScope).toBe("51c621b4");
    expect(c.text).toBe("thinking out loud");
    expect(c.kind).toBe("thinking");
  });
});

describe("parseRunScope / parseToolCall", () => {
  it("returns null scope when no prefix present", () => {
    expect(parseRunScope("just text")).toEqual({ runScope: null, rest: "just text" });
  });
  it("treats success/succeeded as ok", () => {
    expect(parseToolCall("[tool:kubectl/result(success)] applied")?.tool.ok).toBe(true);
    expect(parseToolCall("[tool:kubectl/result(failure)] denied")?.tool.ok).toBe(false);
  });
  it("returns null for non-tool content", () => {
    expect(parseToolCall("plain comment")).toBeNull();
  });
});

describe("runScopeLabel", () => {
  it("prefers the -rNN run suffix", () => {
    expect(runScopeLabel("intake-ef5b2075-r15")).toBe("r15");
  });
  it("falls back to a short id", () => {
    expect(runScopeLabel("51c621b4d9f0aa11")).toBe("51c621b4");
  });
});

function classified(over: Partial<ClassifiedEntry>): ClassifiedEntry {
  return {
    id: over.id ?? "c1",
    kind: over.kind ?? "comment",
    text: over.text ?? "",
    author: over.author,
    runScope: over.runScope ?? null,
    tool: over.tool,
    toolPhase: over.toolPhase,
    role: over.role,
    tokens: over.tokens,
    ts: over.ts ?? 0,
  };
}

describe("This-run / All scope filter", () => {
  const runId = "intake-ef5b2075-r15";

  it("keeps unlabelled entries as this run's own", () => {
    expect(belongsToRun(classified({ runScope: null }), runId)).toBe(true);
  });

  it("keeps entries whose token matches the run label", () => {
    expect(belongsToRun(classified({ runScope: "r15" }), runId)).toBe(true);
  });

  it("drops entries tagged with a different run", () => {
    expect(belongsToRun(classified({ runScope: "51c621b4" }), runId)).toBe(false);
    expect(belongsToRun(classified({ runScope: "af093ad5" }), runId)).toBe(false);
  });

  it("filterByScope=this hides other runs but All keeps everything", () => {
    const entries = [
      classified({ id: "a", runScope: null }),
      classified({ id: "b", runScope: "r15" }),
      classified({ id: "c", runScope: "51c621b4" }),
    ];
    expect(filterByScope(entries, "this", runId).map((e) => e.id)).toEqual(["a", "b"]);
    expect(filterByScope(entries, "all", runId).map((e) => e.id)).toEqual(["a", "b", "c"]);
  });
});

describe("foldActivity — consecutive tool calls collapse into one row", () => {
  it("buckets runs of tool entries and leaves other entries as their own rows", () => {
    const items = foldActivity([
      classified({ id: "you", kind: "you" }),
      classified({ id: "t1", kind: "tool", tool: { name: "bash", ok: true }, ts: 1 }),
      classified({ id: "t2", kind: "tool", tool: { name: "glob", ok: false }, ts: 2 }),
      classified({ id: "cmt", kind: "comment" }),
    ]);
    expect(items).toHaveLength(3);
    expect(items[0]).toMatchObject({ type: "entry" });
    expect(items[1]).toMatchObject({ type: "tools", ts: 2 });
    if (items[1].type === "tools") {
      expect(items[1].tools).toEqual([
        { name: "bash", ok: true },
        { name: "glob", ok: false },
      ]);
    }
    expect(items[2]).toMatchObject({ type: "entry" });
  });
});

describe("enrichLlm — joins the digest role/tokens onto llm entries (ISI-4813)", () => {
  it("stamps role/tokens/model by id and leaves non-llm entries untouched", () => {
    const entries = [
      classified({ id: "i1", kind: "llm", text: "prompt text" }),
      classified({ id: "c1", kind: "comment" }),
    ];
    const digests: LLMInteractionWire[] = [
      { id: "i1", model: "claude-sonnet-4", role: "prompt", content: "prompt text", timestamp: "", tokensUsed: 1200 },
    ];
    const [llm, cmt] = enrichLlm(entries, digests);
    expect(llm.role).toBe("prompt");
    expect(llm.tokens).toBe(1200);
    expect(llm.author).toBe("claude-sonnet-4");
    expect(cmt.role).toBeUndefined();
  });

  it("is a no-op when there is no digest to join", () => {
    const entries = [classified({ id: "i1", kind: "llm" })];
    expect(enrichLlm(entries, null)).toBe(entries);
  });
});

describe("partitionByView — execution vs conversation (ISI-4813)", () => {
  it("routes llm/tool/thinking to execution and you/comment/system to conversation", () => {
    const { execution, conversation } = partitionByView([
      classified({ id: "a", kind: "llm" }),
      classified({ id: "b", kind: "tool" }),
      classified({ id: "c", kind: "thinking" }),
      classified({ id: "d", kind: "you" }),
      classified({ id: "e", kind: "comment" }),
      classified({ id: "f", kind: "system" }),
    ]);
    expect(execution.map((e) => e.id)).toEqual(["a", "b", "c"]);
    expect(conversation.map((e) => e.id)).toEqual(["d", "e", "f"]);
  });
});

describe("buildExecutionItems — pairs prompt/response and call/output (ISI-4813)", () => {
  it("folds a prompt followed by its response into one exchange card", () => {
    const items = buildExecutionItems([
      classified({ id: "p", kind: "llm", role: "prompt", text: "ask", author: "claude", ts: 1 }),
      classified({ id: "r", kind: "llm", role: "response", text: "reply", tokens: 900, author: "claude", ts: 2 }),
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ type: "exchange", prompt: "ask", response: "reply", tokens: 900 });
  });

  it("folds a tool call and its observation output into one tool card", () => {
    const items = buildExecutionItems([
      classified({ id: "t", kind: "tool", toolPhase: "call", tool: { name: "bash", ok: true }, text: "ls", ts: 1 }),
      classified({ id: "o", kind: "tool", toolPhase: "result", text: "total 8", ts: 2 }),
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ type: "tool", name: "bash", ok: true, args: "ls", output: "total 8", ts: 2 });
  });

  it("renders an unpaired response on its own without fabricating a prompt", () => {
    const items = buildExecutionItems([
      classified({ id: "r", kind: "llm", role: "response", text: "reply", ts: 1 }),
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ type: "exchange", response: "reply" });
    if (items[0].type === "exchange") expect(items[0].prompt).toBeUndefined();
  });

  it("passes thinking entries straight through", () => {
    const items = buildExecutionItems([classified({ id: "th", kind: "thinking", text: "hmm" })]);
    expect(items[0]).toMatchObject({ type: "thinking" });
  });
});

describe("buildLifecycle — the rail replaces state-transition rows", () => {
  function detail(phase: string): RunDetailResponseWire {
    return {
      run: {
        metadata: { creationTimestamp: "2026-09-22T08:56:13Z" },
        spec: {},
        status: { phase, claimedAt: "2026-09-22T08:56:15Z" },
      },
      steps: [
        { id: "s1", name: "impl", status: "done", startedAt: "2026-09-22T08:56:15Z", completedAt: "2026-09-22T09:04:48Z" },
      ],
    };
  }

  it("marks every node reached on a terminal run and labels the last node", () => {
    const lc = buildLifecycle(detail("Succeeded"));
    expect(lc.nodes.map((n) => n.label)).toEqual(["Dispatching", "Running", "Collecting", "Succeeded"]);
    expect(lc.nodes.every((n) => n.reached)).toBe(true);
    expect(lc.currentIndex).toBe(3);
    expect(lc.totalMs).toBeGreaterThan(0);
  });

  it("only reaches up to the current phase mid-run", () => {
    const lc = buildLifecycle(detail("Running"));
    expect(lc.currentIndex).toBe(1);
    expect(lc.nodes[2].reached).toBe(false);
    expect(lc.nodes[3].reached).toBe(false);
  });
});

describe("formatCompactTime", () => {
  it("renders h:mm and a dash for the zero clock", () => {
    expect(formatCompactTime(0)).toBe("—");
    expect(formatCompactTime(Date.parse("2026-09-22T08:57:00"))).toMatch(/^\d{1,2}:\d{2}$/);
  });
});
