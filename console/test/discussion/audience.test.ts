import { describe, it, expect } from "vitest";
import {
  PARTY_AUDIENCE,
  audienceLabel,
  audienceWire,
  directAudience,
  parseAudience,
} from "@/lib/discussion/audience";

// ISI-4929 (plan §4.1/§4.2): audience is exactly two wire shapes — `party`
// (default) and `direct:{agentId}`. The server's normalizeAudience is the
// authority; these helpers build/parse the token client-side and must degrade
// (never throw) on anything else.

describe("directAudience", () => {
  it("builds direct:{agentId}", () => {
    expect(directAudience("agent-7")).toBe("direct:agent-7");
  });
  it("an empty target is a caller defect → party (the server default)", () => {
    expect(directAudience("")).toBe(PARTY_AUDIENCE);
    expect(directAudience("   ")).toBe(PARTY_AUDIENCE);
  });
});

describe("parseAudience", () => {
  it("parses a direct token", () => {
    expect(parseAudience("direct:agent-7")).toEqual({
      kind: "direct",
      agentId: "agent-7",
    });
  });
  it("party / missing / unknown all degrade to party", () => {
    expect(parseAudience("party")).toEqual({ kind: "party" });
    expect(parseAudience(undefined)).toEqual({ kind: "party" });
    expect(parseAudience(null)).toEqual({ kind: "party" });
    expect(parseAudience("direct:")).toEqual({ kind: "party" });
    expect(parseAudience("broadcast")).toEqual({ kind: "party" });
  });
});

describe("audienceWire — the minimal-wire projection", () => {
  it("party and empty are OMITTED (server default keeps the body minimal)", () => {
    expect(audienceWire({ kind: "party" })).toBeUndefined();
    expect(audienceWire(null)).toBeUndefined();
    expect(audienceWire(undefined)).toBeUndefined();
    expect(audienceWire("party")).toBeUndefined();
  });
  it("a direct audience emits its token", () => {
    expect(audienceWire({ kind: "direct", agentId: "a-1" })).toBe(
      "direct:a-1",
    );
  });
  it("round-trips an already-built token (buildPostBody is idempotent)", () => {
    const once = audienceWire({ kind: "direct", agentId: "a-1" });
    expect(audienceWire(once)).toBe("direct:a-1");
  });
  it("a malformed token degrades to omitted, never a throw", () => {
    expect(audienceWire("direct:")).toBeUndefined();
    expect(audienceWire("weird")).toBeUndefined();
  });
});

describe("audienceLabel", () => {
  it("labels both audiences", () => {
    expect(audienceLabel({ kind: "party" })).toBe("Party (room)");
    expect(audienceLabel({ kind: "direct", agentId: "a-9" })).toBe(
      "Direct · a-9",
    );
  });
});
