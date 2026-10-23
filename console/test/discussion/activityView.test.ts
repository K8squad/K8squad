import { describe, it, expect } from "vitest";
import { cleanActivityBody } from "@/lib/discussion/activityView";

describe("cleanActivityBody (ISI-5696)", () => {
  it("strips a leading [run <id>] scope marker", () => {
    expect(cleanActivityBody("[run d7a40680] reading the thread")).toBe(
      "reading the thread",
    );
  });

  it("strips a [run][tool:…/result] marker chain, keeping the output text", () => {
    expect(
      cleanActivityBody("[run r1][tool:bash/result(ok)] listed 3 files"),
    ).toBe("listed 3 files");
  });

  it("strips [server:…] transport tags", () => {
    expect(
      cleanActivityBody(
        "[run r1] [tool:fetch/result(ok)] [server:mcp.github] done",
      ),
    ).toBe("done");
  });

  it("strips [untrusted] intermediate-reasoning markers", () => {
    expect(cleanActivityBody("[untrusted] Objective: ship the fix")).toBe(
      "Objective: ship the fix",
    );
  });

  it("strips [status …] lifecycle lines", () => {
    expect(cleanActivityBody("[run r1][status] completed")).toBe("completed");
  });

  it("falls back to the run-scope-stripped remainder when only a bare marker remains", () => {
    // A bare status marker with no trailing text must never render blank.
    expect(cleanActivityBody("[run r1][status]")).toBe("[status]");
  });

  it("leaves plain prose (and later brackets) untouched", () => {
    expect(cleanActivityBody("posted the plan [see above]")).toBe(
      "posted the plan [see above]",
    );
  });

  it("is null-safe on empty input", () => {
    expect(cleanActivityBody("")).toBe("");
  });
});
