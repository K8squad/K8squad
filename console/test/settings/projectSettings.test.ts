// test/settings/projectSettings.test.ts — pure derivations for the Settings tab (ISI-4000 / S2).
// AC2 (credential status tri-state), AC7 (status classification), and the no-wipe write merge —
// the ISI-3985 hazard this tab is careful to avoid — plus the SCM secret-name + field-error helpers.

import { describe, it, expect } from "vitest";
import {
  classifyStatus,
  credentialStatusLabel,
  mergeProjectWrite,
  parseFieldErrors,
  scmSecretName,
  SCM_PAT_SECRET_KEY,
  type ProjectAuthoring,
} from "@/lib/projectSettings";

describe("credentialStatusLabel (AC2)", () => {
  it("is 'Not connected' when there is no credential, regardless of lastTest", () => {
    expect(credentialStatusLabel(false, "untested")).toEqual({ label: "Not connected", tone: "idle" });
    // The presence of a stale 'passed' never overrides 'not connected'.
    expect(credentialStatusLabel(false, "passed").label).toBe("Not connected");
  });

  it("qualifies a connected credential by its last-test result (never 'healthy' on presence alone)", () => {
    expect(credentialStatusLabel(true, "passed")).toEqual({ label: "Connected — test passed", tone: "ok" });
    expect(credentialStatusLabel(true, "failed")).toEqual({ label: "Connected — test failed", tone: "bad" });
    expect(credentialStatusLabel(true, "untested")).toEqual({ label: "Connected — untested", tone: "warn" });
  });
});

describe("classifyStatus (AC7)", () => {
  it("collapses 401/403/404 and maps 501 → not-wired, else error", () => {
    expect(classifyStatus(401).kind).toBe("unauthenticated");
    expect(classifyStatus(403).kind).toBe("not-found");
    expect(classifyStatus(404).kind).toBe("not-found");
    expect(classifyStatus(501).kind).toBe("not-wired");
    expect(classifyStatus(500)).toEqual({ kind: "error", status: 500 });
  });
});

const base: ProjectAuthoring = {
  name: "web",
  repo: {
    url: "https://github.com/acme/web",
    ref: "main",
    auth: { credentialSecretRef: { name: "web-scm-abc", key: "apiKey" } },
  },
  goals: ["ship it", "keep it green"],
  egressPolicyRef: { name: "default-egress" },
};

describe("mergeProjectWrite — no-wipe guarantee (ISI-3985)", () => {
  it("editing the repo URL PRESERVES goals, egress, and the connected credential", () => {
    const body = mergeProjectWrite(base, { url: "https://github.com/acme/web2", ref: "release" });
    expect(body.repo.url).toBe("https://github.com/acme/web2");
    expect(body.repo.ref).toBe("release");
    expect(body.repo.auth).toEqual({ credentialSecretRef: { name: "web-scm-abc", key: "apiKey" } });
    expect(body.goals).toEqual(["ship it", "keep it green"]);
    expect(body.egressPolicyRef).toEqual({ name: "default-egress" });
  });

  it("setting the PAT PRESERVES the repo url/ref and goals, repointing only the credential", () => {
    const body = mergeProjectWrite(base, {
      credentialSecretRef: { name: "web-scm-xyz", key: SCM_PAT_SECRET_KEY },
    });
    expect(body.repo.url).toBe("https://github.com/acme/web");
    expect(body.repo.ref).toBe("main");
    expect(body.repo.auth).toEqual({ credentialSecretRef: { name: "web-scm-xyz", key: "apiKey" } });
    expect(body.goals).toEqual(["ship it", "keep it green"]);
  });

  it("an empty ref means 'default branch' and is OMITTED from the wire", () => {
    const body = mergeProjectWrite(base, { url: base.repo.url, ref: "" });
    expect(body.repo.ref).toBeUndefined();
  });

  it("drops absent optional fields (no goals / no egress / no auth) rather than emitting empties", () => {
    const bare: ProjectAuthoring = { name: "x", repo: { url: "https://github.com/a/b" } };
    const body = mergeProjectWrite(bare, { url: "https://github.com/a/b" });
    expect(body.goals).toBeUndefined();
    expect(body.egressPolicyRef).toBeUndefined();
    expect(body.repo.auth).toBeUndefined();
  });
});

describe("scmSecretName + parseFieldErrors", () => {
  it("builds a unique, lowercase DNS-1123-shaped name from project + suffix", () => {
    expect(scmSecretName("Web", "k9z")).toBe("web-scm-k9z");
    // Two different suffixes ⇒ two different names (create-only write ⇒ replace = new name).
    expect(scmSecretName("web", "a")).not.toBe(scmSecretName("web", "b"));
  });

  it("parses the apiserver's {fields:[{field,message}]} 422 body into a keyed map", () => {
    const map = parseFieldErrors({ error: "validation failed", fields: [{ field: "repo.url", message: "is required" }] });
    expect(map["repo.url"]).toBe("is required");
    expect(parseFieldErrors(null)).toEqual({});
    expect(parseFieldErrors({ nope: true })).toEqual({});
  });
});
