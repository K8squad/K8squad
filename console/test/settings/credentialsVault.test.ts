import { describe, it, expect } from "vitest";
import {
  MASKED_VALUE,
  classifyCredentialTypes,
  classifyVaultCreateStatus,
  isRequiredMissing,
  isSatisfied,
  sortVaultTypes,
  toolCredentialCreateBody,
  vaultCreateMessage,
  vaultStatusBadge,
  type CredentialTypeEntry,
} from "@/lib/credentialsVault";

// Pure derivations behind the WS-9 S4 Credentials Vault UI (ISI-5847, spec
// isi-5817 §3/§5/§6/§1.2): the status badge (gateState + requiredByGate), the
// required-first row ordering, the write-body shape, and the honest status/outcome
// classifications. All DOM-free so they pin the acceptance rules directly.

function entry(over: Partial<CredentialTypeEntry> = {}): CredentialTypeEntry {
  return {
    type: "github",
    displayName: "GitHub",
    consumption: "header",
    headerName: "Authorization",
    dataKey: "token",
    requiredByGate: true,
    probeKind: "http",
    consumedBy: ["github-pr-review"],
    gateState: "unsatisfied",
    ...over,
  };
}

describe("vaultStatusBadge — spec §3", () => {
  it("satisfied ⇒ Connected (ok), regardless of requiredByGate", () => {
    expect(vaultStatusBadge(entry({ gateState: "satisfied" }))).toEqual({ label: "Connected", tone: "ok" });
    expect(vaultStatusBadge(entry({ gateState: "satisfied", requiredByGate: false })).label).toBe("Connected");
  });
  it("unsatisfied & required ⇒ Required (bad)", () => {
    expect(vaultStatusBadge(entry({ gateState: "unsatisfied", requiredByGate: true }))).toEqual({
      label: "Required",
      tone: "bad",
    });
  });
  it("unsatisfied & optional ⇒ Not set (idle)", () => {
    expect(vaultStatusBadge(entry({ gateState: "unsatisfied", requiredByGate: false }))).toEqual({
      label: "Not set",
      tone: "idle",
    });
  });
  it("an unknown gateState is NEVER a guessed green — optional ⇒ Not set, required ⇒ Required", () => {
    expect(vaultStatusBadge(entry({ gateState: "weird", requiredByGate: false })).label).toBe("Not set");
    expect(vaultStatusBadge(entry({ gateState: "weird", requiredByGate: true })).label).toBe("Required");
  });
});

describe("isSatisfied / isRequiredMissing — read from gateState only", () => {
  it("isSatisfied is true ONLY for the explicit satisfied state", () => {
    expect(isSatisfied(entry({ gateState: "satisfied" }))).toBe(true);
    expect(isSatisfied(entry({ gateState: "unsatisfied" }))).toBe(false);
    expect(isSatisfied(entry({ gateState: "" }))).toBe(false);
  });
  it("isRequiredMissing is required AND not satisfied", () => {
    expect(isRequiredMissing(entry({ requiredByGate: true, gateState: "unsatisfied" }))).toBe(true);
    expect(isRequiredMissing(entry({ requiredByGate: true, gateState: "satisfied" }))).toBe(false);
    expect(isRequiredMissing(entry({ requiredByGate: false, gateState: "unsatisfied" }))).toBe(false);
  });
});

describe("sortVaultTypes — required-but-missing first (spec §6), stable otherwise", () => {
  it("floats required-missing rows to the top, preserving backend order within groups", () => {
    const types = [
      entry({ type: "a", requiredByGate: false, gateState: "unsatisfied" }), // not set (optional)
      entry({ type: "b", requiredByGate: true, gateState: "satisfied" }), // connected
      entry({ type: "c", requiredByGate: true, gateState: "unsatisfied" }), // REQUIRED missing
      entry({ type: "d", requiredByGate: true, gateState: "unsatisfied" }), // REQUIRED missing
    ];
    const order = sortVaultTypes(types).map((t) => t.type);
    expect(order).toEqual(["c", "d", "a", "b"]);
  });
  it("does not mutate the input array", () => {
    const types = [
      entry({ type: "a", requiredByGate: false, gateState: "unsatisfied" }),
      entry({ type: "c", requiredByGate: true, gateState: "unsatisfied" }),
    ];
    const before = types.map((t) => t.type);
    sortVaultTypes(types);
    expect(types.map((t) => t.type)).toEqual(before);
  });
});

describe("toolCredentialCreateBody — vault axis (spec §1.2)", () => {
  it("carries {name,value,type,projectId} with the value verbatim (write-only)", () => {
    expect(
      toolCredentialCreateBody({ name: "github", value: "ghp_secret", type: "github", projectId: "proj-a" }),
    ).toEqual({ name: "github", value: "ghp_secret", type: "github", projectId: "proj-a" });
  });
});

describe("classifyVaultCreateStatus — spec §1.2 error map", () => {
  it("201 ⇒ created", () => expect(classifyVaultCreateStatus(201)).toBe("created"));
  it("409 ⇒ conflict (the Rotate path)", () => expect(classifyVaultCreateStatus(409)).toBe("conflict"));
  it("400 & 422 ⇒ invalid (inline field error)", () => {
    expect(classifyVaultCreateStatus(400)).toBe("invalid");
    expect(classifyVaultCreateStatus(422)).toBe("invalid");
  });
  it("401/403 ⇒ denied", () => {
    expect(classifyVaultCreateStatus(401)).toBe("denied");
    expect(classifyVaultCreateStatus(403)).toBe("denied");
  });
  it("501 ⇒ unsupported (human-seat / axis unavailable)", () => expect(classifyVaultCreateStatus(501)).toBe("unsupported"));
  it("502 / 5xx ⇒ error", () => {
    expect(classifyVaultCreateStatus(502)).toBe("error");
    expect(classifyVaultCreateStatus(500)).toBe("error");
  });
});

describe("vaultCreateMessage — honest fallback copy", () => {
  it("conflict points at Rotate; unsupported names Connect Claude", () => {
    expect(vaultCreateMessage("conflict")).toMatch(/Rotate/);
    expect(vaultCreateMessage("unsupported")).toMatch(/Connect Claude/);
  });
});

describe("classifyCredentialTypes — honest read states (401/403/404 collapse, 501 not-wired)", () => {
  it("401 ⇒ unauthenticated", () => expect(classifyCredentialTypes(401)).toEqual({ kind: "unauthenticated" }));
  it("403/404 ⇒ not-found (existence-hiding)", () => {
    expect(classifyCredentialTypes(403)).toEqual({ kind: "not-found" });
    expect(classifyCredentialTypes(404)).toEqual({ kind: "not-found" });
  });
  it("501 ⇒ not-wired", () => expect(classifyCredentialTypes(501)).toEqual({ kind: "not-wired" }));
  it("5xx ⇒ error with the status", () => expect(classifyCredentialTypes(503)).toEqual({ kind: "error", status: 503 }));
});

describe("MASKED_VALUE — the only representation of a stored value (no reveal)", () => {
  it("is a run of bullet dots, never the value", () => {
    expect(MASKED_VALUE).toMatch(/^[•]+$/);
  });
});
