// test/compose/fromWire.test.ts — fromWire ⟺ toWire round-trip (ADR-0016 / ISI-4007).
//
// fromWire is the inverse of toWire: it maps a per-kind authoring-spec detail read
// (GET /api/squad/{kind}/{name}) back onto the ComposeForm the edit surface renders,
// so opening an object to EDIT pre-fills the real spec instead of a blank form the
// PUT would then blow away (ISI-3985). These tests pin the "one mapper pair, no
// drift" contract: for agents/roles/projects the detail body IS the write wire, so
// toWire(fromWire(wire)) reproduces it (agents/roles gain the write-only `project`
// scope back as "", which the edit-save caller re-supplies). Skills reuse the
// flattened SkillView shape, asserted field-by-field.

import { describe, it, expect } from "vitest";
import { fromWire, toWire, type AgentWire, type RoleWire, type ProjectWire, type SkillWire } from "@/lib/compose";

describe("fromWire ⟺ toWire round-trip (ADR-0016)", () => {
  it("agents: a full authoring spec round-trips losslessly (project re-added as \"\")", () => {
    const wire: AgentWire = {
      name: "cade",
      runtimeRef: { name: "claude-rt" },
      roleRef: { name: "dev", namespace: "shared" },
      skillRefs: [{ name: "sk-a" }, { name: "sk-b", namespace: "shared" }],
      model: "claude-opus",
      modelEndpointRef: { name: "byo-endpoint", key: "url" },
      credentialSecretRef: { name: "cade-cred", key: "token" },
      credentialClass: "human-seat",
      fallbackModel: { model: "gpt-5", modelEndpointRef: { name: "fb-endpoint" } },
    };
    const cf = fromWire("agents", wire);
    expect(cf.kind).toBe("agents");
    // Refs → editable strings; a ref list → newline-joined textarea; byoEnabled derived.
    if (cf.kind === "agents") {
      expect(cf.form.roleRef).toBe("shared/dev");
      expect(cf.form.skillRefs).toBe("sk-a\nshared/sk-b");
      expect(cf.form.modelEndpointRef).toBe("byo-endpoint/url");
      expect(cf.form.credentialSecretRef).toBe("cade-cred/token");
      expect(cf.form.fallbackModel).toBe("gpt-5");
      expect(cf.form.fallbackModelEndpointRef).toBe("fb-endpoint");
      expect(cf.form.byoEnabled).toBe(true);
    }
    // toWire reproduces the wire byte-for-byte, plus the write-only project scope as "".
    expect(toWire(cf)).toEqual({ project: "", ...wire });
  });

  it("agents: no BYO endpoint ⇒ byoEnabled false and no modelEndpointRef emitted", () => {
    const wire: AgentWire = {
      name: "plain",
      runtimeRef: { name: "claude-rt" },
      roleRef: { name: "dev" },
      model: "claude-opus",
      credentialSecretRef: { name: "cred" },
    };
    const cf = fromWire("agents", wire);
    if (cf.kind === "agents") expect(cf.form.byoEnabled).toBe(false);
    expect(toWire(cf)).toEqual({ project: "", ...wire });
  });

  it("roles: prompt + default skills + hint round-trip (project re-added as \"\")", () => {
    const wire: RoleWire = {
      name: "dev",
      promptRef: { name: "dev-prompt" },
      defaultSkills: [{ name: "sk-a" }, { name: "sk-b", namespace: "shared" }],
      runtimeClassHint: "gvisor",
    };
    const cf = fromWire("roles", wire);
    if (cf.kind === "roles") expect(cf.form.defaultSkills).toBe("sk-a\nshared/sk-b");
    expect(toWire(cf)).toEqual({ project: "", ...wire });
  });

  it("projects: repo + goals + egress ref round-trip (no project scope field)", () => {
    const wire: ProjectWire = {
      name: "widget",
      repo: { url: "https://github.com/acme/widget", ref: "main" },
      goals: ["ship v1", "keep CI green"],
      egressPolicyRef: { name: "default-deny" },
    };
    const cf = fromWire("projects", wire);
    if (cf.kind === "projects") {
      expect(cf.form.repoUrl).toBe("https://github.com/acme/widget");
      expect(cf.form.goals).toBe("ship v1\nkeep CI green");
      expect(cf.form.egressPolicyRef).toBe("default-deny");
    }
    // Projects have no write-only scope field, so the wire round-trips exactly.
    expect(toWire(cf)).toEqual(wire);
  });

  it("skills: the flattened SkillView hydrates git fields + reconstructs nested source", () => {
    const wire: SkillWire = {
      name: "pg-migrate",
      sourceType: "git",
      repoRef: "github.com/acme/skills",
      ref: "abc123",
      path: "skills/pg",
      permissions: ["net:egress", "fs:write"],
    };
    const cf = fromWire("skills", wire);
    if (cf.kind === "skills") {
      expect(cf.form.sourceType).toBe("git");
      expect(cf.form.gitRepoRef).toBe("github.com/acme/skills");
      expect(cf.form.gitPath).toBe("skills/pg");
      expect(cf.form.permissions).toBe("net:egress\nfs:write");
    }
    // toWire re-nests the flat SkillView into the write wire's source.git shape.
    expect(toWire(cf)).toEqual({
      project: "",
      name: "pg-migrate",
      source: { type: "git", git: { repoRef: "github.com/acme/skills", ref: "abc123", path: "skills/pg" } },
      permissions: ["net:egress", "fs:write"],
    });
  });

  it("skills: an inline body round-trips through the inline source shape", () => {
    const wire: SkillWire = { name: "hello", sourceType: "inline", inline: "echo hi", permissions: [] };
    const cf = fromWire("skills", wire);
    if (cf.kind === "skills") expect(cf.form.inline).toBe("echo hi");
    expect(toWire(cf)).toEqual({ project: "", name: "hello", source: { type: "inline", inline: "echo hi" } });
  });

  it("is defensive: a sparse/empty wire hydrates to a valid empty-ish form (never throws)", () => {
    const cf = fromWire("agents", {});
    expect(cf.kind).toBe("agents");
    if (cf.kind === "agents") {
      expect(cf.form.name).toBe("");
      expect(cf.form.byoEnabled).toBe(false);
      expect(cf.form.skillRefs).toBe("");
    }
  });
});
