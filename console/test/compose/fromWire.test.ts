import { describe, it, expect } from "vitest";
import {
  fromWire,
  toWire,
  emptyForm,
  type ComposeForm,
} from "@/lib/compose";

// fromWire is the exact inverse of toWire (ADR-0016 / ISI-4002): the Compose EDIT
// form hydrates from GET /api/squad/{kind}/{name}, whose body is the compose WRITE
// wire minus the RBAC-only `project` scope. A round-trip form → toWire → fromWire
// must recover every form-owned field (project excepted — it is never persisted on
// the object, so it comes back "" and the editor re-selects it, as on create).

/** Compare two forms ignoring the RBAC-only `project` field (never round-tripped). */
function expectFormsEqualIgnoringProject(got: ComposeForm, want: ComposeForm) {
  const strip = (cf: ComposeForm) => {
    const f = { ...(cf.form as Record<string, unknown>) };
    delete f.project;
    return { kind: cf.kind, form: f };
  };
  expect(strip(got)).toEqual(strip(want));
}

describe("fromWire ∘ toWire round-trip", () => {
  it("agents — full spec (refs, skills, BYO endpoint, credential class, fallback)", () => {
    const form: ComposeForm = {
      kind: "agents",
      form: {
        project: "widget",
        name: "cade",
        runtimeRef: "claude-code",
        roleRef: "shared/boss",
        skillRefs: "web-search\nshared/pg",
        model: "claude-opus-5",
        modelEndpointRef: "byo-endpoint/url",
        credentialSecretRef: "model-creds/token",
        credentialClass: "human-seat",
        fallbackModel: "claude-sonnet-5",
        fallbackModelEndpointRef: "fb-endpoint",
        byoEnabled: true,
      },
    };
    const back = fromWire("agents", toWire(form));
    expectFormsEqualIgnoringProject(back, form);
    // byoEnabled is derived from a present modelEndpointRef, not serialized.
    if (back.kind === "agents") expect(back.form.byoEnabled).toBe(true);
  });

  it("agents — no BYO endpoint ⇒ byoEnabled derives false", () => {
    const form: ComposeForm = {
      kind: "agents",
      form: {
        project: "widget",
        name: "solo",
        runtimeRef: "claude-code",
        roleRef: "boss",
        skillRefs: "",
        model: "claude-opus-5",
        modelEndpointRef: "",
        credentialSecretRef: "model-creds",
        credentialClass: "",
        fallbackModel: "",
        fallbackModelEndpointRef: "",
        byoEnabled: false,
      },
    };
    const back = fromWire("agents", toWire(form));
    expectFormsEqualIgnoringProject(back, form);
    if (back.kind === "agents") expect(back.form.byoEnabled).toBe(false);
  });

  it("roles — prompt ref, default skills, runtime class hint", () => {
    const form: ComposeForm = {
      kind: "roles",
      form: {
        project: "widget",
        name: "boss",
        promptRef: "boss-prompt",
        defaultSkills: "web-search\nshared/pg",
        runtimeClassHint: "gvisor",
      },
    };
    expectFormsEqualIgnoringProject(fromWire("roles", toWire(form)), form);
  });

  it("projects — repo url/ref, goals, egress policy ref", () => {
    const form: ComposeForm = {
      kind: "projects",
      form: {
        name: "widget",
        repoUrl: "https://github.com/acme/widget",
        repoRef: "main",
        goals: "ship v1\nraise coverage",
        egressPolicyRef: "default-egress",
      },
    };
    expectFormsEqualIgnoringProject(fromWire("projects", toWire(form)), form);
  });

  it("teams — name + namespace strategy (TeamDetail shape)", () => {
    const back = fromWire("teams", { name: "alpha", namespaceStrategy: "perTeam" });
    expect(back).toEqual({ kind: "teams", form: { name: "alpha", namespaceStrategy: "perTeam" } });
  });

  it("skills — inline body (SkillView flat shape, ISI-3961 + inline)", () => {
    const back = fromWire("skills", {
      name: "greeter",
      sourceType: "inline",
      inline: "# greeter body\nhello",
      permissions: ["fs:read"],
    });
    expect(back).toEqual({
      kind: "skills",
      form: {
        project: "",
        name: "greeter",
        sourceType: "inline",
        inline: "# greeter body\nhello",
        gitRepoRef: "",
        gitRef: "",
        gitPath: "",
        permissions: "fs:read",
      },
    });
  });

  it("skills — git source (repoRef/ref/path flat shape)", () => {
    const back = fromWire("skills", {
      name: "pg-migrate",
      sourceType: "git",
      repoRef: "github.com/acme/skills",
      ref: "abc123",
      path: "skills/pg",
    });
    if (back.kind !== "skills") throw new Error("kind");
    expect(back.form.sourceType).toBe("git");
    expect(back.form.gitRepoRef).toBe("github.com/acme/skills");
    expect(back.form.gitRef).toBe("abc123");
    expect(back.form.gitPath).toBe("skills/pg");
    expect(back.form.inline).toBe("");
  });

  it("tolerates a null/empty body ⇒ empty form for the kind", () => {
    expect(fromWire("agents", null)).toEqual(emptyForm("agents"));
    expect(fromWire("projects", {})).toEqual(emptyForm("projects"));
  });
});
