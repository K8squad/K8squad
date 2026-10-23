// test/compose/modelConfig.test.ts — the org-default ModelConfig form round-trip
// (ISI-4890 Flow A) with the ISI-5302 adapter rehydration fix.
//
// The ModelConfig wire is adapter-agnostic: it persists only the model triple
// ({model, fallbackModel?, modelEndpointRef?}), never the UI credential branch.
// Before ISI-5302, modelConfigFromWire hard-coded adapter:"claude", so a saved
// opencode default rehydrated onto the claude branch and looked lost on refresh.
// The fix infers the adapter from the primary modelEndpointRef against the saved
// endpoints' providers (GET /api/modelendpoints). These tests pin that inference and
// the triple round-trip (toWire → fromWire).

import { describe, it, expect } from "vitest";
import {
  emptyModelConfigForm,
  inferModelConfigAdapter,
  modelConfigFromWire,
  modelConfigToWire,
  RUNTIME_ADAPTERS,
  RUNTIME_ADAPTER_LABELS,
  type ModelConfigForm,
  type ModelEndpointRow,
  type RuntimeAdapter,
} from "@/lib/compose";

describe("RUNTIME_ADAPTERS — ISI-6380 five-tab contract", () => {
  it("exposes five tabs in order, each with a label", () => {
    expect([...RUNTIME_ADAPTERS]).toEqual([
      "claude",
      "codex",
      "opencode",
      "vibe",
      "goose",
    ]);
    const unlabeled = (RUNTIME_ADAPTERS as readonly string[]).filter(
      (a) => !RUNTIME_ADAPTER_LABELS[a as RuntimeAdapter],
    );
    expect(unlabeled).toEqual([]);
  });
});

const OLLAMA: ModelEndpointRow = {
  name: "lan-ollama",
  provider: "ollama",
  url: "http://10.0.0.5:11434",
  hasToken: false,
};
const DEEPSEEK: ModelEndpointRow = {
  name: "my-deepseek",
  provider: "deepseek",
  url: "",
  hasToken: true,
};
// A non-opencode (e.g. claude BYO) endpoint: its provider is NOT in LLM_PROVIDER_OPTIONS.
const ANTHROPIC: ModelEndpointRow = {
  name: "anthropic-proxy",
  provider: "anthropic",
  url: "https://x",
  hasToken: true,
};

describe("inferModelConfigAdapter — ISI-5302 adapter recovery", () => {
  it("returns claude for an empty ref", () => {
    expect(inferModelConfigAdapter("", [OLLAMA])).toBe("claude");
  });

  it("returns opencode when the ref names a saved opencode-backend endpoint", () => {
    expect(inferModelConfigAdapter("lan-ollama", [OLLAMA, DEEPSEEK])).toBe(
      "opencode",
    );
    expect(inferModelConfigAdapter("my-deepseek", [OLLAMA, DEEPSEEK])).toBe(
      "opencode",
    );
  });

  it("resolves the ref by name even when a /key suffix is present", () => {
    expect(inferModelConfigAdapter("my-deepseek/token", [DEEPSEEK])).toBe(
      "opencode",
    );
  });

  it("returns claude for a non-opencode provider (claude BYO endpoint)", () => {
    expect(inferModelConfigAdapter("anthropic-proxy", [ANTHROPIC])).toBe(
      "claude",
    );
  });

  it("fails safe to claude when the ref can't be resolved (endpoints missing)", () => {
    expect(inferModelConfigAdapter("lan-ollama", [])).toBe("claude");
    expect(inferModelConfigAdapter("ghost", [OLLAMA])).toBe("claude");
  });

  it("rehydrates a mistral-bound default onto vibe, not opencode (ISI-6380)", () => {
    const MISTRAL: ModelEndpointRow = {
      name: "my-mistral",
      provider: "mistral",
      url: "http://ollama.lan:11434/v1",
      hasToken: true,
    };
    expect(inferModelConfigAdapter("my-mistral", [MISTRAL, OLLAMA])).toBe(
      "vibe",
    );
    expect(inferModelConfigAdapter("my-mistral/token", [MISTRAL])).toBe("vibe");
    // other BYO backends must stay on opencode
    expect(inferModelConfigAdapter("lan-ollama", [MISTRAL, OLLAMA])).toBe(
      "opencode",
    );
  });
});

describe("modelConfig toWire ∘ fromWire round-trip", () => {
  it("rehydrates an opencode default onto the opencode branch (the ISI-5302 bug)", () => {
    const saved: ModelConfigForm = {
      ...emptyModelConfigForm(),
      adapter: "opencode",
      model: "llama3.1:8b",
      modelEndpointRef: "lan-ollama",
      byoEnabled: true,
    };
    const wire = modelConfigToWire(saved);
    // adapter never rides the wire (persisted spec is adapter-agnostic).
    expect(wire).not.toHaveProperty("adapter");
    expect(wire).toMatchObject({
      model: "llama3.1:8b",
      modelEndpointRef: { name: "lan-ollama" },
    });

    const rehydrated = modelConfigFromWire(wire, [OLLAMA]);
    expect(rehydrated.adapter).toBe("opencode");
    expect(rehydrated.model).toBe("llama3.1:8b");
    expect(rehydrated.modelEndpointRef).toBe("lan-ollama");
    expect(rehydrated.byoEnabled).toBe(true);
  });

  it("round-trips a claude default (no endpoint ref) without regression", () => {
    const saved: ModelConfigForm = {
      ...emptyModelConfigForm(),
      adapter: "claude",
      model: "claude-opus-4-8",
      fallbackModel: "claude-haiku-4-5-20251001",
    };
    const rehydrated = modelConfigFromWire(modelConfigToWire(saved), [OLLAMA]);
    expect(rehydrated.adapter).toBe("claude");
    expect(rehydrated.model).toBe("claude-opus-4-8");
    expect(rehydrated.fallbackModel).toBe("claude-haiku-4-5-20251001");
    expect(rehydrated.byoEnabled).toBe(false);
  });

  it("without the endpoints list, an opencode default degrades to claude (fail-safe, not a crash)", () => {
    const wire = modelConfigToWire({
      ...emptyModelConfigForm(),
      adapter: "opencode",
      model: "llama3.1:8b",
      modelEndpointRef: "lan-ollama",
      byoEnabled: true,
    });
    const rehydrated = modelConfigFromWire(wire); // no endpoints arg
    expect(rehydrated.adapter).toBe("claude");
    // The model triple is still recovered — strictly better than losing it.
    expect(rehydrated.model).toBe("llama3.1:8b");
    expect(rehydrated.modelEndpointRef).toBe("lan-ollama");
  });
});
