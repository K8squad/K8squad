import { describe, it, expect } from "vitest";
import {
  SLASH_COMMANDS,
  matchSlashCommands,
  resolveSlashCommand,
  slashFragment,
} from "@/lib/discussion/slash";

// ISI-5674 WS-D — the pure slash-command parse/intercept primitives. `/reset` is
// the only command this phase (plan §WS-D.4).

describe("slashFragment — leading-`/` command context", () => {
  it("opens on a bare slash", () => {
    expect(slashFragment("/")).toBe("");
  });

  it("returns the (lower-cased) word after the slash", () => {
    expect(slashFragment("/r")).toBe("r");
    expect(slashFragment("/reset")).toBe("reset");
    expect(slashFragment("/RESET")).toBe("reset");
  });

  it("is null for ordinary text, a mid-token slash, or a slash with a space", () => {
    expect(slashFragment("")).toBeNull();
    expect(slashFragment("hello")).toBeNull();
    expect(slashFragment("and/or")).toBeNull();
    expect(slashFragment("/reset now")).toBeNull(); // a space ends the context
    expect(slashFragment(" /reset")).toBeNull(); // not leading
  });
});

describe("matchSlashCommands — prefix filter", () => {
  it("a bare slash lists every command", () => {
    expect(matchSlashCommands("")).toEqual(SLASH_COMMANDS);
  });

  it("filters by name prefix, case-insensitively", () => {
    expect(matchSlashCommands("res").map((c) => c.name)).toEqual(["reset"]);
    expect(matchSlashCommands("RES").map((c) => c.name)).toEqual(["reset"]);
  });

  it("returns empty for an unknown prefix", () => {
    expect(matchSlashCommands("xyz")).toEqual([]);
  });

  it("exposes the mock copy for /reset", () => {
    const reset = SLASH_COMMANDS.find((c) => c.name === "reset");
    expect(reset?.title).toBe("Clear this discussion");
    expect(reset?.subtitle).toBe("Starts a fresh transcript · history preserved");
  });
});

describe("resolveSlashCommand — exact intercept on submit", () => {
  it("resolves an exact /reset (tolerating trailing space)", () => {
    expect(resolveSlashCommand("/reset")?.name).toBe("reset");
    expect(resolveSlashCommand("/reset ")?.name).toBe("reset");
    expect(resolveSlashCommand("/RESET")?.name).toBe("reset");
  });

  it("does NOT resolve a partial, an argument form, or ordinary text", () => {
    expect(resolveSlashCommand("/res")).toBeNull();
    expect(resolveSlashCommand("/")).toBeNull();
    expect(resolveSlashCommand("/reset the room")).toBeNull();
    expect(resolveSlashCommand("reset")).toBeNull();
    expect(resolveSlashCommand("")).toBeNull();
  });
});
