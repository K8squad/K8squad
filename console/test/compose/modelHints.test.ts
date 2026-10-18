// test/compose/modelHints.test.ts — the curated model-hints seam (Story B, ISI-3555).
//
// Locks the seam's contract: the curated list is non-empty and shaped {id,label}; useModelHints()
// returns it; curated detection and the SOFT (never-blocking) shape hint behave per AC1/AC2/AC5/AC7.

import { describe, expect, it } from "vitest";
import {
  CURATED_MODELS,
  isCuratedModel,
  modelProvider,
  modelShapeHint,
  sameProvider,
  useModelHints,
} from "@/lib/modelHints";

describe("modelProvider / sameProvider (ISI-3681 E3-S3 fallback warning)", () => {
  it("extracts a coarse provider token", () => {
    expect(modelProvider("claude-opus-4-8")).toBe("claude");
    expect(modelProvider("ollama/llama3.1:8b")).toBe("ollama");
    expect(modelProvider("gpt-4o")).toBe("openai");
    expect(modelProvider("")).toBe("");
  });

  it("flags same-provider primary+fallback, ignores empties and cross-provider", () => {
    expect(sameProvider("claude-opus-4-8", "claude-haiku-4-5")).toBe(true);
    expect(sameProvider("claude-opus-4-8", "ollama/llama3.1:8b")).toBe(false);
    expect(sameProvider("claude-opus-4-8", "")).toBe(false);
    expect(sameProvider("", "claude-haiku-4-5")).toBe(false);
  });
});

describe("useModelHints() seam", () => {
  it("returns the static curated list, each entry an {id,label}", () => {
    const hints = useModelHints();
    expect(hints).toBe(CURATED_MODELS);
    expect(hints.length).toBeGreaterThan(0);
    for (const h of hints) {
      expect(typeof h.id).toBe("string");
      expect(h.id.length).toBeGreaterThan(0);
      expect(typeof h.label).toBe("string");
      expect(h.label.length).toBeGreaterThan(0);
    }
  });

  it("offers the Claude family (Opus / Sonnet / Haiku) up front", () => {
    const ids = useModelHints().map((h) => h.id);
    expect(ids).toContain("claude-opus-4-8");
    expect(ids.some((id) => id.includes("sonnet"))).toBe(true);
    expect(ids.some((id) => id.includes("haiku"))).toBe(true);
  });

  it("is frozen — a consumer cannot mutate the shared list", () => {
    expect(Object.isFrozen(CURATED_MODELS)).toBe(true);
  });
});

describe("isCuratedModel", () => {
  it("recognises a curated id and rejects a custom one (AC1/AC2)", () => {
    expect(isCuratedModel("claude-opus-4-8")).toBe(true);
    expect(isCuratedModel("  claude-opus-4-8  ")).toBe(true); // trims
    expect(isCuratedModel("ollama/llama3.1:8b")).toBe(false);
    expect(isCuratedModel("")).toBe(false);
    expect(isCuratedModel("   ")).toBe(false);
  });
});

describe("modelShapeHint — soft guidance, never a hard block (AC5/AC7)", () => {
  it("stays silent for conventional ids (curated + non-Anthropic)", () => {
    expect(modelShapeHint("claude-opus-4-8")).toBeUndefined();
    expect(modelShapeHint("ollama/llama3.1:8b")).toBeUndefined();
    expect(modelShapeHint("gpt-4o")).toBeUndefined();
    expect(modelShapeHint("")).toBeUndefined(); // emptiness is a required-error elsewhere, not a hint
  });

  it("nudges (but does not reject) an unusual id", () => {
    expect(modelShapeHint("My Model!")).toMatch(/unusual/i);
    expect(modelShapeHint("has space")).toBeDefined();
  });
});

describe("vibe default (ISI-6158 C2) — curated first, never a constraint", () => {
  it("leads the curated list with the vibe runtime default (mistral-large-latest)", () => {
    const hints = useModelHints();
    expect(hints[0].id).toBe("mistral-large-latest");
    expect(hints[0].label).toMatch(/vibe/i);
    // The Claude family stays curated right behind it (nothing was displaced, only led).
    expect(hints.map((h) => h.id)).toContain("claude-opus-4-8");
  });

  it("recognises the vibe default as curated — and keeps other Mistral ids on the Custom path (ADR-026)", () => {
    expect(isCuratedModel("mistral-large-latest")).toBe(true);
    // Convenience, never constraint: any other mistral id stays reachable via “Custom model…”.
    expect(isCuratedModel("mistral-medium-latest")).toBe(false);
    expect(modelShapeHint("mistral-medium-latest")).toBeUndefined(); // conventional shape — no nudge
  });

  it("resolves the mistral provider token for the same-provider fallback warning", () => {
    expect(modelProvider("mistral-large-latest")).toBe("mistral");
    expect(sameProvider("mistral-large-latest", "mistral-medium-latest")).toBe(true);
    expect(sameProvider("mistral-large-latest", "claude-opus-4-8")).toBe(false);
  });
});
