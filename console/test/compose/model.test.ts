// test/compose/model.test.ts — the Compose form model (story 8.5, ISI-2901).
//
// Locks the wire contract to the apiserver's *Request structs (composecrd.go) and the field-level
// validation to its plan*() checks. If toWire drifts from what the CRD-apply surface decodes, or
// validate stops mirroring the server, these fail.

import { describe, expect, it } from "vitest";
import {
  COMPOSE_KINDS,
  emptyForm,
  isComposeKind,
  isValid,
  parseObjectRef,
  parseSecretRef,
  toWire,
  validate,
  type AgentForm,
  type ComposeForm,
  type ProjectForm,
  type SkillForm,
} from "@/lib/compose";

function proj(f: Partial<ProjectForm>): ComposeForm {
  return { kind: "projects", form: { ...emptyForm("projects").form, ...f } as ProjectForm };
}
function agent(f: Partial<AgentForm>): ComposeForm {
  return { kind: "agents", form: { ...(emptyForm("agents").form as AgentForm), ...f } };
}
function skill(f: Partial<SkillForm>): ComposeForm {
  return { kind: "skills", form: { ...(emptyForm("skills").form as SkillForm), ...f } };
}

describe("kind allow-list", () => {
  it("accepts exactly the five compose kinds", () => {
    expect([...COMPOSE_KINDS]).toEqual(["teams", "projects", "agents", "roles", "skills"]);
    expect(isComposeKind("projects")).toBe(true);
    expect(isComposeKind("runs")).toBe(false); // must not proxy an arbitrary upstream path
    expect(isComposeKind("../secrets")).toBe(false);
  });
});

describe("ref parsing", () => {
  it("splits object refs on namespace/name and secret refs on name/key", () => {
    expect(parseObjectRef("prod/db")).toEqual({ name: "db", namespace: "prod" });
    expect(parseObjectRef("db")).toEqual({ name: "db" });
    expect(parseSecretRef("cred/token")).toEqual({ name: "cred", key: "token" });
    expect(parseSecretRef("cred")).toEqual({ name: "cred" });
  });
});

describe("toWire nesting + optional omission", () => {
  it("nests project repo and omits empty optionals", () => {
    const w = toWire(proj({ name: "shop", repoUrl: "https://x/y", goals: "a\n\nb" }));
    expect(w).toEqual({ name: "shop", repo: { url: "https://x/y" }, goals: ["a", "b"] });
    expect("egressPolicyRef" in w).toBe(false); // blank ref omitted, never {name:""}
  });

  it("builds agent refs and omits optional model endpoint", () => {
    const w = toWire(
      agent({
        project: "p",
        name: "a1",
        runtimeRef: "rt",
        roleRef: "r",
        model: "m",
        credentialSecretRef: "cred/token",
        skillRefs: "s1\nns/s2",
      }),
    );
    expect(w).toMatchObject({
      project: "p",
      name: "a1",
      runtimeRef: { name: "rt" },
      roleRef: { name: "r" },
      model: "m",
      credentialSecretRef: { name: "cred", key: "token" },
      skillRefs: [{ name: "s1" }, { name: "s2", namespace: "ns" }],
    });
    expect("modelEndpointRef" in w).toBe(false);
    // No credentialClass / fallbackModel set on the default form ⇒ neither rides the wire.
    expect("credentialClass" in w).toBe(false);
    expect("fallbackModel" in w).toBe(false);
  });

  it("round-trips credentialClass + fallbackModel through toWire (ISI-3681 E3-S3 AC5)", () => {
    const w = toWire(
      agent({
        project: "p",
        name: "a1",
        runtimeRef: "rt",
        roleRef: "r",
        model: "claude-opus-4-8",
        credentialSecretRef: "cred/token",
        credentialClass: "human-seat",
        fallbackModel: "claude-haiku-4-5",
        fallbackModelEndpointRef: "fb-endpoint/url",
      }),
    );
    expect(w).toMatchObject({
      credentialClass: "human-seat",
      fallbackModel: { model: "claude-haiku-4-5", modelEndpointRef: { name: "fb-endpoint", key: "url" } },
    });
  });

  it("omits the fallback endpoint ref when only a fallback model is set", () => {
    const w = toWire(
      agent({
        project: "p",
        name: "a1",
        runtimeRef: "rt",
        roleRef: "r",
        model: "claude-opus-4-8",
        credentialSecretRef: "cred",
        fallbackModel: "ollama/llama3.1:8b",
      }),
    );
    expect(w.fallbackModel).toEqual({ model: "ollama/llama3.1:8b" });
    // A fallback endpoint with no fallback model is not a fallback at all — nothing rides.
    const noFb = toWire(
      agent({
        project: "p",
        name: "a1",
        runtimeRef: "rt",
        roleRef: "r",
        model: "claude-opus-4-8",
        credentialSecretRef: "cred",
        fallbackModelEndpointRef: "orphan/url",
      }),
    );
    expect("fallbackModel" in noFb).toBe(false);
  });

  it("emits skill git source and drops the inline branch (and vice versa)", () => {
    const git = toWire(
      skill({ name: "s", sourceType: "git", gitRepoRef: "repo", gitRef: "main", gitPath: "d" }),
    );
    expect(git.source).toEqual({ type: "git", git: { repoRef: "repo", ref: "main", path: "d" } });
    const inline = toWire(skill({ name: "s", sourceType: "inline", inline: "code" }));
    expect(inline.source).toEqual({ type: "inline", inline: "code" });
  });
});

describe("validate mirrors the server field checks", () => {
  it("rejects an invalid DNS-1123 name", () => {
    expect(validate(proj({ name: "Bad_Name", repoUrl: "https://x" })).name).toMatch(/DNS-1123/);
  });

  it("requires repo.url for a project", () => {
    expect(validate(proj({ name: "ok" }))["repo.url"]).toBe("is required");
    expect(isValid(proj({ name: "ok", repoUrl: "https://x" }))).toBe(true);
  });

  it("requires all mandatory agent fields", () => {
    const errs = validate(agent({ name: "a" }));
    for (const f of ["project", "runtimeRef.name", "roleRef.name", "credentialSecretRef.name", "model"])
      expect(errs[f]).toBe("is required");
  });

  it("requires an endpoint Secret ref only when BYO is enabled (Story B / AC3, AC5)", () => {
    const base = {
      project: "p",
      name: "a1",
      runtimeRef: "rt",
      roleRef: "r",
      model: "claude-opus-4-8",
      credentialSecretRef: "cred/token",
    };
    // BYO off (default): a blank endpoint ref is fine.
    expect(isValid(agent({ ...base }))).toBe(true);
    // BYO on + empty ref: blocks with a field error.
    expect(validate(agent({ ...base, byoEnabled: true }))["modelEndpointRef.name"]).toBe("is required");
    // BYO on + a ref: valid, and the ref still rides through toWire unchanged.
    const withRef = agent({ ...base, byoEnabled: true, modelEndpointRef: "my-endpoint/url" });
    expect(isValid(withRef)).toBe(true);
    expect(toWire(withRef)).toMatchObject({ modelEndpointRef: { name: "my-endpoint", key: "url" } });
  });

  it("enforces skill source inline/git exclusivity requirements", () => {
    expect(validate(skill({ project: "p", name: "s", sourceType: "inline" }))["source.inline"]).toBe(
      "is required",
    );
    const gitErrs = validate(skill({ project: "p", name: "s", sourceType: "git" }));
    expect(gitErrs["source.git.repoRef"]).toBe("is required");
    expect(gitErrs["source.git.ref"]).toBe("is required");
  });

  it("constrains the role runtimeClassHint enum", () => {
    expect(
      validate({
        kind: "roles",
        form: { ...emptyForm("roles").form, project: "p", name: "r", promptRef: "pr", runtimeClassHint: "xen" } as never,
      })["runtimeClassHint"],
    ).toMatch(/gvisor, kata, runc/);
  });
});
