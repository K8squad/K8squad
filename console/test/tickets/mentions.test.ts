// test/tickets/mentions.test.ts — ISI-5159 shared `@`-mention primitives.

import { describe, it, expect } from "vitest";
import {
  mentionFragmentBefore,
  replaceMentionFragment,
  agentMentionSuggestions,
  triggerFragmentBefore,
  replaceTriggerFragment,
  resolveBodyMentions,
} from "@/lib/mentions";
import type { AgentOption } from "@/lib/tickets/api";

const ROSTER: AgentOption[] = [
  { id: "a1", name: "builder", role: "engineer" },
  { id: "a2", name: "reviewer", role: "code_review" },
  { id: "a3", name: "planner" },
];

describe("mentionFragmentBefore", () => {
  it("opens on a bare `@` at the caret", () => {
    expect(mentionFragmentBefore("hi @", 4)).toBe("");
  });
  it("captures the running fragment", () => {
    expect(mentionFragmentBefore("hi @rev", 7)).toBe("rev");
  });
  it("does not trigger inside an email-like token", () => {
    expect(mentionFragmentBefore("mail a@b", 8)).toBeNull();
  });
  it("returns null when the caret is not in a fragment", () => {
    expect(mentionFragmentBefore("just words", 10)).toBeNull();
  });
});

describe("replaceMentionFragment", () => {
  it("swaps the trailing fragment for the canonical token", () => {
    expect(replaceMentionFragment("hi @rev", "reviewer")).toBe("hi @reviewer ");
  });
  it("swaps a bare `@`", () => {
    expect(replaceMentionFragment("hi @", "planner")).toBe("hi @planner ");
  });
});

describe("triggerFragmentBefore (ISI-5167 — dual `@`/`#` trigger)", () => {
  it("detects the `@` trigger and its fragment", () => {
    expect(triggerFragmentBefore("hi @rev", 7)).toEqual({
      trigger: "@",
      fragment: "rev",
    });
  });
  it("detects the `#` trigger and its fragment", () => {
    expect(triggerFragmentBefore("see #fix", 8)).toEqual({
      trigger: "#",
      fragment: "fix",
    });
  });
  it("opens on a bare `#`", () => {
    expect(triggerFragmentBefore("re: #", 5)).toEqual({
      trigger: "#",
      fragment: "",
    });
  });
  it("does not trigger inside a mid-token `#` (e.g. `c#3`)", () => {
    expect(triggerFragmentBefore("issue c#3", 9)).toBeNull();
  });
  it("returns null when the caret is not in a fragment", () => {
    expect(triggerFragmentBefore("just words", 10)).toBeNull();
  });
});

describe("replaceTriggerFragment (ISI-5167)", () => {
  it("agent → `@displayName ` token", () => {
    expect(replaceTriggerFragment("hi @rev", "reviewer", "agent")).toBe(
      "hi @reviewer ",
    );
  });
  it("work_item → `#Title ` token", () => {
    expect(replaceTriggerFragment("see #fl", "Fix flaky test", "work_item")).toBe(
      "see #Fix flaky test ",
    );
  });
  it("normalizes a work_item picked from the `@` list to a `#` prefix (bug fix)", () => {
    expect(replaceTriggerFragment("see @fl", "Fix flaky test", "work_item")).toBe(
      "see #Fix flaky test ",
    );
  });
});

describe("agentMentionSuggestions", () => {
  it("offers the whole roster for an empty fragment", () => {
    expect(agentMentionSuggestions(ROSTER, "")).toHaveLength(3);
  });
  it("filters case-insensitively by substring", () => {
    const out = agentMentionSuggestions(ROSTER, "REV");
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      type: "agent",
      id: "reviewer",
      displayName: "reviewer",
      state: "code_review",
    });
  });
  it("maps every row to a dispatchable agent name (id === name)", () => {
    for (const s of agentMentionSuggestions(ROSTER, "")) {
      expect(s.type).toBe("agent");
      expect(s.id).toBe(s.displayName);
    }
  });
});

describe("resolveBodyMentions (ISI-5281 — dispatch from typed @-body)", () => {
  it("resolves a single typed @name to its canonical roster name", () => {
    expect(resolveBodyMentions("please look @reviewer", ROSTER)).toEqual([
      "reviewer",
    ]);
  });
  it("resolves case-insensitively to the canonical casing", () => {
    expect(resolveBodyMentions("hey @REVIEWER", ROSTER)).toEqual(["reviewer"]);
  });
  it("ignores @tokens that match no roster agent", () => {
    expect(resolveBodyMentions("ping @nobody here", ROSTER)).toEqual([]);
  });
  it("returns [] for a body with no mentions", () => {
    expect(resolveBodyMentions("just a plain comment", ROSTER)).toEqual([]);
  });
  it("never triggers inside an email-like token", () => {
    expect(resolveBodyMentions("mail builder@reviewer.io", ROSTER)).toEqual([]);
  });
  it("de-dupes a repeated mention to a single canonical name", () => {
    expect(resolveBodyMentions("@reviewer and again @reviewer", ROSTER)).toEqual(
      ["reviewer"],
    );
  });
  it("returns all distinct resolvable names in first-seen order (2+ is ambiguous for WS-1)", () => {
    expect(
      resolveBodyMentions("@reviewer then @builder please", ROSTER),
    ).toEqual(["reviewer", "builder"]);
  });
});
