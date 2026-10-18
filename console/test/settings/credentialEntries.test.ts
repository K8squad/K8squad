// test/settings/credentialEntries.test.ts — the runtime credential-entry mirror (ISI-6202,
// child of ISI-6158 C2: the console LLM-settings vibe entry).
//
// Locks the console mirror of pkg/credinject's injection contract: what a pasted credential for
// each runtime INJECTS into the sandbox. The vibe row is the P4.1 shape: the per-user Mistral key
// maps onto MISTRAL_API_KEY, and the BYO lane adds MISTRAL_BASE_URL (mirroring the claude-code /
// codex rows). Also pins the display derivations the vibe credential rides: the token-type label
// and the static-expiry honesty for a never-expiring API key.

import { describe, it, expect } from "vitest";
import {
  CREATE_RUNTIME_OPTIONS,
  RUNTIME_CREDENTIAL_ENTRIES,
  credentialCreateBody,
  expiryLabel,
  runtimeCredentialEntry,
  tokenTypeLabel,
  type AgentCredentialRow,
} from "@/lib/credentials";

function row(credentialClass: string): AgentCredentialRow {
  return {
    agent: "a",
    namespace: "ns",
    runtime: "vibe",
    credentialRef: "ns/cred",
    credentialClass,
    expiresKnown: false,
    health: "connected",
  };
}

describe("RUNTIME_CREDENTIAL_ENTRIES — the injection-contract mirror (ISI-6158 C2)", () => {
  it("vibe: mistral_api_key injects MISTRAL_API_KEY; the BYO lane adds MISTRAL_BASE_URL", () => {
    const vibe = runtimeCredentialEntry("vibe");
    expect(vibe).toBeDefined();
    expect(vibe).toMatchObject({
      runtime: "vibe",
      credentialType: "mistral_api_key",
      injects: "MISTRAL_API_KEY",
      byoInjects: "MISTRAL_BASE_URL",
    });
  });

  it("keeps the credinject rows verbatim — claude-code/openclaw/hermes → ANTHROPIC_API_KEY, codex/opencode → OPENAI_API_KEY", () => {
    // Env-var names are copied from pkg/credinject's reviewed table, never invented here. If the
    // table changes upstream, this lock forces a deliberate mirror update.
    expect(runtimeCredentialEntry("claude-code")?.injects).toBe("ANTHROPIC_API_KEY");
    expect(runtimeCredentialEntry("openclaw")?.injects).toBe("ANTHROPIC_API_KEY");
    expect(runtimeCredentialEntry("hermes")?.injects).toBe("ANTHROPIC_API_KEY");
    expect(runtimeCredentialEntry("codex")?.injects).toBe("OPENAI_API_KEY");
    expect(runtimeCredentialEntry("opencode")?.injects).toBe("OPENAI_API_KEY");
  });

  it("every runtime the paste form offers resolves to an entry (vibe included)", () => {
    expect(CREATE_RUNTIME_OPTIONS.length).toBeGreaterThan(0);
    for (const opt of CREATE_RUNTIME_OPTIONS) {
      expect(runtimeCredentialEntry(opt.value)).toBeDefined();
    }
    expect(CREATE_RUNTIME_OPTIONS.some((o) => o.value === "vibe")).toBe(true);
  });

  it("returns undefined for a runtime outside the mirror (honest absence, never a guess)", () => {
    expect(runtimeCredentialEntry("does-not-exist")).toBeUndefined();
    expect(RUNTIME_CREDENTIAL_ENTRIES).toHaveLength(6);
  });
});

describe("vibe credential display derivations", () => {
  it("tokenTypeLabel renders the mistral_api_key class as “API key · Mistral”", () => {
    expect(tokenTypeLabel(row("mistral_api_key"))).toBe("API key · Mistral");
  });

  it("a mistral_api_key credential is static — no fabricated expiry horizon (honesty rules)", () => {
    expect(expiryLabel(row("mistral_api_key"))).toBe("— (static)");
  });

  it("a vibe paste rides the service-account lane: name trimmed, value verbatim, class pinned", () => {
    expect(
      credentialCreateBody({ name: "  vibe-team  ", runtime: "vibe", value: "mist-key" }),
    ).toEqual({
      name: "vibe-team",
      runtime: "vibe",
      class: "service-account",
      value: "mist-key",
    });
  });
});
