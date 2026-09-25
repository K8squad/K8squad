import { describe, it, expect } from "vitest";
import {
  presenceFromStatus,
  presenceSublabel,
} from "@/lib/discussion/presence";

// ISI-4929 (plan §4.5): the roster shows online / working / offline, folded
// from the four-value agent status bucket (idle/running/blocked/paused, §8).
// v1 degrade: no last-seen timestamp exists, so unknown → offline, and the
// sub-label shows only what the read model actually knows.

describe("presenceFromStatus", () => {
  it("running → working", () => {
    expect(presenceFromStatus("running")).toBe("working");
  });
  it("idle → online", () => {
    expect(presenceFromStatus("idle")).toBe("online");
  });
  it("paused / blocked / unknown / missing → offline (v1 degrade)", () => {
    expect(presenceFromStatus("paused")).toBe("offline");
    expect(presenceFromStatus("blocked")).toBe("offline");
    expect(presenceFromStatus("who-knows")).toBe("offline");
    expect(presenceFromStatus(undefined)).toBe("offline");
    expect(presenceFromStatus(null)).toBe("offline");
  });
});

describe("presenceSublabel", () => {
  it("shows the known bucket verbatim", () => {
    expect(presenceSublabel("running")).toBe("running");
  });
  it("shows a dash when the status is absent (never a fabricated time)", () => {
    expect(presenceSublabel(undefined)).toBe("—");
    expect(presenceSublabel(null)).toBe("—");
    expect(presenceSublabel("")).toBe("—");
  });
});
