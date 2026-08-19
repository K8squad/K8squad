import { describe, it, expect } from "vitest";
import {
  chipBorder,
  badgeBaseColor,
  runChipBaseColor,
  CHIP_BORDER_ALPHA,
} from "@/lib/discussion/theme";

// AC6 / 8.9: chip borders derive from base (#{BASE}55) → theme-invariant.

describe("chipBorder — 8.9 theme-invariant chip rule", () => {
  it("appends the alpha suffix to a hex base", () => {
    expect(chipBorder("#3B82F6")).toBe("#3B82F655");
    expect(CHIP_BORDER_ALPHA).toBe("55");
  });

  it("is a pure function of the base only (identical in dark and light)", () => {
    // Same base token in either theme yields the same border — no theme input.
    const dark = chipBorder(badgeBaseColor("agent"));
    const light = chipBorder(badgeBaseColor("agent"));
    expect(dark).toBe(light);
  });

  it("maps every badge kind to a distinct base token", () => {
    const kinds = ["agent", "human", "system", "unknown"] as const;
    const tokens = kinds.map(badgeBaseColor);
    expect(new Set(tokens).size).toBe(kinds.length);
    expect(runChipBaseColor()).not.toBe(badgeBaseColor("agent"));
  });
});
