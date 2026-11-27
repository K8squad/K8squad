// test/tickets/feedView.test.ts - ISI-7888: the conversation-first feed's
// para-4 taxonomy shaping (classifier + block assembly + notice parsing).
// Pure layer, no DOM - the component treatments are exercised by the
// ticketDetail suites.

import { describe, it, expect } from "vitest";
import {
  buildFeed,
  isSystemNoticeComment,
  isSystemPrincipal,
  noticeHeadline,
  noticeRunId,
  readStoredExpanded,
  readStoredFeedMode,
} from "@/lib/tickets/feedView";
import type { ActivityItem, ThreadComment } from "@/lib/tickets/thread";

const comment = (over: Partial<ThreadComment> = {}): ThreadComment => ({
  author: "user:admin",
  body: "hello",
  createdAt: "2026-11-26T10:00:00Z",
  ...over,
});

const commentItem = (c: ThreadComment): ActivityItem => ({
  kind: "comment",
  at: c.createdAt,
  who: c.author,
  authorKind: "agent",
  comment: c,
});

const eventItem = (at: string): ActivityItem => ({
  kind: "event",
  at,
  who: "ksquad-operator",
  authorKind: "user",
  event: {
    fromState: "todo",
    toState: "in_progress",
    principal: "ksquad-operator",
    occurredAt: at,
  },
});

describe("isSystemPrincipal (para-4 author rule)", () => {
  it("classifies the dispatcher and service vocabulary as system", () => {
    expect(isSystemPrincipal("system:ci-failure")).toBe(true);
    expect(isSystemPrincipal("system:issue-triage")).toBe(true);
    expect(isSystemPrincipal("operator:relay")).toBe(true);
    expect(isSystemPrincipal("ksquad-operator")).toBe(true);
  });

  it("never classifies humans or agents as system", () => {
    expect(isSystemPrincipal("user:admin")).toBe(false);
    expect(isSystemPrincipal("agent:builder")).toBe(false);
    expect(isSystemPrincipal("run/abcd1234")).toBe(false);
    expect(isSystemPrincipal("admin")).toBe(false);
    expect(isSystemPrincipal("")).toBe(false);
  });
});

describe("isSystemNoticeComment (para-4 taxonomy rule)", () => {
  it("agent speech stays a comment - including run mirrors and summaries", () => {
    expect(
      isSystemNoticeComment(comment({ author: "agent:john", kind: "summary" })),
    ).toBe(false);
    expect(
      isSystemNoticeComment(
        comment({ author: "run/abcd1234", kind: "summary" }),
      ),
    ).toBe(false);
    expect(isSystemNoticeComment(comment({ author: "agent:john" }))).toBe(
      false,
    );
  });

  it("operator-settled recaps are notices (kind=summary, non-agent author)", () => {
    expect(
      isSystemNoticeComment(
        comment({ author: "ksquad-operator", kind: "summary" }),
      ),
    ).toBe(true);
    expect(
      isSystemNoticeComment(comment({ author: "system:ci-failure" })),
    ).toBe(true);
  });

  it("a legacy failure-notice body classifies without the kind stamp", () => {
    expect(
      isSystemNoticeComment(
        comment({
          author: "ksquad-operator",
          body: "Run failure notice (auto-generated - the run failed)",
        }),
      ),
    ).toBe(true);
  });

  it("a plain human comment stays a comment", () => {
    expect(isSystemNoticeComment(comment())).toBe(false);
  });
});

describe("notice parsing", () => {
  const modelRoute =
    "Run failure notice (auto-generated - the run failed before the agent could post a summary)\n\n" +
    "What happened: john's run failed - the model route gave up.\n\n" +
    "Evidence: run_id=b2ad8107-7234-4652-8d4b-7ab2d36290a1";

  it("headline names the humane cause class", () => {
    expect(noticeHeadline(modelRoute)).toBe("Run failed - model route");
    expect(noticeHeadline("Run failure notice - the follow stream broke")).toBe(
      "Run failed - follow stream error",
    );
    expect(noticeHeadline("Run summary (auto-generated - no summary)")).toBe(
      "Run summary - auto-generated",
    );
  });

  it("an unknown producer degrades to its own first line, never a guess", () => {
    expect(noticeHeadline("Something else happened\nsecond line")).toBe(
      "Something else happened",
    );
    expect(noticeHeadline("")).toBe("System notice");
  });

  it("extracts the evidence run id for the View run deep-link", () => {
    expect(noticeRunId(modelRoute)).toBe(
      "b2ad8107-7234-4652-8d4b-7ab2d36290a1",
    );
    expect(noticeRunId("no evidence here")).toBe("");
  });
});

describe("buildFeed (para-5 block assembly)", () => {
  const human = commentItem(comment({ author: "user:admin" }));
  const agent = commentItem(
    comment({ author: "agent:john", kind: "summary", body: "did the work" }),
  );
  const notice = commentItem(
    comment({
      author: "ksquad-operator",
      kind: "summary",
      body: "Run failure notice (auto-generated - model route)",
    }),
  );
  const live = {
    ...commentItem(comment({ author: "run/abcd1234", body: "stepping" })),
    live: true,
    liveTier: "thinking" as const,
  };
  const deliverable = {
    ...commentItem(comment({ author: "agent:john", body: "final answer" })),
    live: true,
    liveTier: "deliverable" as const,
  };

  it("conversation mode: >=2 consecutive events collapse into one group", () => {
    const blocks = buildFeed(
      [eventItem("10:00"), eventItem("10:01"), human, eventItem("10:05")],
      "conversation",
    );
    expect(blocks).toEqual([
      { kind: "activity", items: [eventItem("10:00"), eventItem("10:01")] },
      { kind: "row", item: human },
      { kind: "row", item: eventItem("10:05") },
    ]);
  });

  it("conversation mode: notices become notice blocks, speech stays rows", () => {
    const blocks = buildFeed([human, notice, agent], "conversation");
    expect(blocks).toEqual([
      { kind: "row", item: human },
      { kind: "notice", item: notice },
      { kind: "row", item: agent },
    ]);
  });

  it("conversation mode: live thinking rows drop out; deliverables stay", () => {
    const blocks = buildFeed([live, deliverable, human], "conversation");
    expect(blocks).toEqual([
      { kind: "row", item: deliverable },
      { kind: "row", item: human },
    ]);
  });

  it("all mode: every event is its own row and live rows stay inline", () => {
    const e1 = eventItem("10:00");
    const blocks = buildFeed([e1, live, human], "all");
    expect(blocks).toEqual([
      { kind: "row", item: e1 },
      { kind: "row", item: live },
      { kind: "row", item: human },
    ]);
  });

  it("all mode keeps the notice treatment (never a comment card)", () => {
    expect(buildFeed([notice], "all")).toEqual([
      { kind: "notice", item: notice },
    ]);
  });
});

describe("feed persistence (para-5)", () => {
  it("defaults to conversation and closed affordances", () => {
    expect(readStoredFeedMode()).toBe("conversation");
    expect(readStoredExpanded("run.x")).toBe(false);
  });
});
