import { describe, it, expect } from "vitest";
import {
  buildProjectPutBody,
  classifyProjectSettings,
  credentialStatusLabel,
  SCM_PAT_SECRET_KEY,
  type ProjectDetail,
} from "@/lib/projectSettings";

// The pure derivations behind the ISI-4000 S2 Settings tab: the honest tri-state
// credential badge (AC2), the HTTP-status classification (AC7), and the full-spec
// compose-PUT body builder (AC3/AC4 — a repo save must never wipe goals/egress and
// must round-trip an existing / newly-attached credential ref).

describe("credentialStatusLabel — AC2 tri-state", () => {
  it("no credential ⇒ Not connected (never healthy)", () => {
    expect(credentialStatusLabel(false, "untested")).toEqual({ label: "Not connected", tone: "idle" });
    // connected:false wins regardless of a stale lastTest.
    expect(credentialStatusLabel(false, "passed").label).toBe("Not connected");
  });
  it("connected + passed ⇒ test passed (ok)", () => {
    expect(credentialStatusLabel(true, "passed")).toEqual({ label: "Connected — test passed", tone: "ok" });
  });
  it("connected + failed ⇒ test failed (bad)", () => {
    expect(credentialStatusLabel(true, "failed")).toEqual({ label: "Connected — test failed", tone: "bad" });
  });
  it("connected + untested (or unknown) ⇒ untested, never healthy", () => {
    expect(credentialStatusLabel(true, "untested")).toEqual({ label: "Connected — untested", tone: "warn" });
    // An unexpected value collapses to untested — honesty over a guessed-green.
    expect(credentialStatusLabel(true, "weird").label).toBe("Connected — untested");
  });
});

describe("classifyProjectSettings — AC7 honest states", () => {
  it("401 ⇒ unauthenticated", () => expect(classifyProjectSettings(401)).toEqual({ kind: "unauthenticated" }));
  it("403/404 collapse ⇒ not-found (existence-hiding)", () => {
    expect(classifyProjectSettings(403)).toEqual({ kind: "not-found" });
    expect(classifyProjectSettings(404)).toEqual({ kind: "not-found" });
  });
  it("501 ⇒ not-wired", () => expect(classifyProjectSettings(501)).toEqual({ kind: "not-wired" }));
  it("5xx ⇒ error with status", () => expect(classifyProjectSettings(503)).toEqual({ kind: "error", status: 503 }));
});

function detail(over: Partial<ProjectDetail> = {}): ProjectDetail {
  return {
    name: "proj-a",
    repo: { url: "https://github.com/org/old", ref: "main" },
    goals: ["ship the thing", "keep it green"],
    egressPolicyRef: { name: "default-egress", namespace: "squad-a" },
    ...over,
  };
}

describe("buildProjectPutBody — full-spec round-trip (AC3/AC4)", () => {
  it("overlays repo url/ref but PRESERVES goals + egressPolicyRef (no silent wipe)", () => {
    const body = buildProjectPutBody(detail(), { repoUrl: "https://github.com/org/new", repoRef: "dev" });
    expect(body).toMatchObject({
      name: "proj-a",
      repo: { url: "https://github.com/org/new", ref: "dev" },
      goals: ["ship the thing", "keep it green"],
      egressPolicyRef: { name: "default-egress", namespace: "squad-a" },
    });
    // No auth was set and the detail had none ⇒ repo.auth absent.
    expect((body.repo as Record<string, unknown>).auth).toBeUndefined();
  });

  it("an empty ref is omitted (apiserver reads it as 'default branch')", () => {
    const body = buildProjectPutBody(detail({ repo: { url: "x" } }), { repoUrl: "https://github.com/org/new", repoRef: "  " });
    expect((body.repo as Record<string, unknown>).ref).toBeUndefined();
  });

  it("carries an existing credential ref through a repo-only edit (never drops it)", () => {
    const d = detail({ repo: { url: "u", ref: "main", auth: { credentialSecretRef: { name: "scm-pat", key: SCM_PAT_SECRET_KEY } } } });
    const body = buildProjectPutBody(d, { repoUrl: "https://github.com/org/new" });
    expect((body.repo as Record<string, unknown>).auth).toEqual({
      credentialSecretRef: { name: "scm-pat", key: SCM_PAT_SECRET_KEY },
    });
  });

  it("a newly attached ref overrides the detail's auth and pins the key", () => {
    const body = buildProjectPutBody(detail(), {
      repoUrl: "https://github.com/org/new",
      credentialSecretRef: { name: "fresh-pat", key: SCM_PAT_SECRET_KEY },
    });
    expect((body.repo as Record<string, unknown>).auth).toEqual({
      credentialSecretRef: { name: "fresh-pat", key: SCM_PAT_SECRET_KEY },
    });
  });

  it("no goals ⇒ goals key omitted (never an empty array that clears them differently)", () => {
    const body = buildProjectPutBody(detail({ goals: [] }), { repoUrl: "u" });
    expect(body.goals).toBeUndefined();
  });
});
