import { afterEach, describe, expect, it, vi } from "vitest";

import {
  agentModelPut,
  agentSkillsPut,
  partitionAgentSkills,
  putCompose,
  roleModelPut,
  roleSkillsPut,
  type AgentDetailWire,
  type RoleDetailWire,
} from "@/lib/agents/rosterEdit";

// ISI-5362 / S5 — the inline-edit wire builders are PURE and pin the field-scoped merge
// contract (ISI-5359): each commit sends ONLY its kind's admission-required identity plus the
// edited field(s), JSON-null as the clear sentinel, and never a field the editor did not touch
// (the failure mode the S1+S2 gate existed to prevent). These mirror the apiserver's
// composecrd_merge_test.go semantics from the client side.

const roleDetail: RoleDetailWire = {
  name: "engineer",
  promptRef: { name: "engineer-prompt" },
  defaultSkills: [{ name: "code-review" }, { name: "tdd" }],
  runtimeClassHint: "kata",
  model: "claude-opus-4-8",
  activePhases: ["implementation"],
  coordinator: true,
  coordinatorMode: "propose",
  resourceVersion: "rv-123",
  usedBy: { agents: ["agent-7", "agent-3"] },
};

const agentDetail: AgentDetailWire = {
  name: "agent-7",
  runtimeRef: { name: "claude-code" },
  roleRef: { name: "engineer" },
  skillRefs: [{ name: "kubectl" }],
  model: "",
  credentialSecretRef: { name: "agent-7-cred" },
};

describe("roleModelPut", () => {
  it("sends identity + the model field only — unsent spec fields are the merge's job", () => {
    const body = roleModelPut(roleDetail, { model: "claude-sonnet-5", fallbackModel: undefined });
    expect(body).toEqual({
      name: "engineer",
      promptRef: { name: "engineer-prompt" },
      resourceVersion: "rv-123",
      model: "claude-sonnet-5",
    });
    // The classic silent-drop fields are NOT on the wire.
    expect(body).not.toHaveProperty("defaultSkills");
    expect(body).not.toHaveProperty("activePhases");
    expect(body).not.toHaveProperty("coordinator");
  });

  it("clears the role-tier pin with JSON null (inherit the org default)", () => {
    const body = roleModelPut(roleDetail, { model: "", fallbackModel: undefined });
    expect(body.model).toBeNull();
  });

  it("round-trips the resourceVersion token opaquely when present", () => {
    const body = roleModelPut({ ...roleDetail, resourceVersion: "" }, { model: "m", fallbackModel: undefined });
    expect(body).not.toHaveProperty("resourceVersion");
  });

  it("sends the fallback only when the user touched it", () => {
    expect(roleModelPut(roleDetail, { model: "m", fallbackModel: undefined })).not.toHaveProperty("fallbackModel");
    expect(roleModelPut(roleDetail, { model: "m", fallbackModel: "qwen3-8b" }).fallbackModel).toEqual({
      model: "qwen3-8b",
    });
    expect(roleModelPut(roleDetail, { model: "m", fallbackModel: "" }).fallbackModel).toBeNull();
  });
});

describe("roleSkillsPut", () => {
  it("replaces the defaultSkills array wholesale (a sent array overwrites)", () => {
    const body = roleSkillsPut(roleDetail, ["code-review", "deploy"]);
    expect(body.defaultSkills).toEqual([{ name: "code-review" }, { name: "deploy" }]);
  });

  it("de-duplicates and trims, and clears with JSON null when empty", () => {
    expect(roleSkillsPut(roleDetail, ["git", " git ", ""]).defaultSkills).toEqual([{ name: "git" }]);
    expect(roleSkillsPut(roleDetail, []).defaultSkills).toBeNull();
  });
});

describe("agentModelPut", () => {
  it("sends the required round-trip identity + the override", () => {
    const body = agentModelPut(agentDetail, "claude-opus-4-8");
    expect(body).toEqual({
      name: "agent-7",
      runtimeRef: { name: "claude-code" },
      roleRef: { name: "engineer" },
      credentialSecretRef: { name: "agent-7-cred" },
      model: "claude-opus-4-8",
    });
    // A skills-only-adjacent edit must not ride skillRefs along.
    expect(body).not.toHaveProperty("skillRefs");
  });

  it("resets to inherited with JSON null (blank means inherit, ISI-4892)", () => {
    expect(agentModelPut(agentDetail, "").model).toBeNull();
  });
});

describe("agentSkillsPut", () => {
  it("replaces skillRefs wholesale and de-duplicates", () => {
    const body = agentSkillsPut(agentDetail, ["kubectl", "kubectl", "helm"]);
    expect(body.skillRefs).toEqual([{ name: "kubectl" }, { name: "helm" }]);
    expect(body).not.toHaveProperty("model"); // blank primary stays unsent ⇒ merge keeps inherit
  });

  it("clears all agent-added skills with JSON null (role-tier defaults untouched)", () => {
    expect(agentSkillsPut(agentDetail, []).skillRefs).toBeNull();
  });
});

describe("partitionAgentSkills", () => {
  it("splits the union by provenance: role defaults vs agent-added", () => {
    const { fromRole, addedToAgent } = partitionAgentSkills(
      { ...agentDetail, skillRefs: [{ name: "kubectl" }, { name: "tdd" }] },
      ["code-review", "tdd"],
    );
    expect(fromRole).toEqual(["code-review", "tdd"]);
    expect(addedToAgent).toEqual(["kubectl"]);
  });

  it("de-duplicates a skill granted by both tiers (role wins the provenance label)", () => {
    const { fromRole, addedToAgent } = partitionAgentSkills(
      { ...agentDetail, skillRefs: [{ name: "tdd" }] },
      ["tdd"],
    );
    expect(fromRole).toEqual(["tdd"]);
    expect(addedToAgent).toEqual([]);
  });
});

// ISI-5419 — the inline-edit WRITE must forward the node-derived `?team=` act-as-team
// selector (mirroring the read), so a fleet admin's save lands in the squad whose node
// they edited. The apiserver honors it admin-only; the client's job is only to carry it.
describe("putCompose ?team= forwarding", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("appends the team selector to the compose URL when a team is given", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    await putCompose("roles", "engineer", { name: "engineer" }, "globex");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const url = fetchMock.mock.calls[0][0] as string;
    expect(url).toBe("/api/compose/roles/engineer?team=globex");
  });

  it("percent-encodes the team selector", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    await putCompose("agents", "agent-7", { name: "agent-7" }, "team a/b");
    const url = fetchMock.mock.calls[0][0] as string;
    expect(url).toBe("/api/compose/agents/agent-7?team=team%20a%2Fb");
  });

  it("omits the query entirely when no team is given (tenant self-scope)", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    await putCompose("agents", "agent-7", { name: "agent-7" });
    const url = fetchMock.mock.calls[0][0] as string;
    expect(url).toBe("/api/compose/agents/agent-7");
  });
});
