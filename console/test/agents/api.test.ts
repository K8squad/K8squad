import { describe, it, expect, vi } from "vitest";
import {
  classifyStatus,
  createAgentsClient,
  AgentsApiError,
  type FetchLike,
} from "@/lib/agents/api";

// Stories 8.10/8.11 ride the SAME deny-by-default BFF choke point as every other read model
// (§13 / 8.7d): deny is existence-hiding — 401/403/404 collapse to a single not-found outcome so
// a Team-B principal never learns a Team-A org / agent / run exists.

describe("classifyStatus — deny collapse", () => {
  it("2xx → ok", () => {
    expect(classifyStatus(200)).toBe("ok");
    expect(classifyStatus(204)).toBe("ok");
  });
  it("401/403/404 all → not-found (no foreign existence leak)", () => {
    expect(classifyStatus(401)).toBe("not-found");
    expect(classifyStatus(403)).toBe("not-found");
    expect(classifyStatus(404)).toBe("not-found");
  });
  it("5xx → error", () => {
    expect(classifyStatus(500)).toBe("error");
    expect(classifyStatus(502)).toBe("error");
  });
});

function stub(status: number, json: unknown): FetchLike {
  return vi.fn(async () => ({
    ok: status >= 200 && status < 300,
    status,
    json: async () => json,
  }));
}

describe("createAgentsClient — read paths + deny hiding", () => {
  it("getTeamOrg returns the org projection on 200", async () => {
    const org = { teamId: "t1", teamName: "Squad One", agents: [] };
    const client = createAgentsClient(stub(200, org));
    await expect(client.getTeamOrg("t1")).resolves.toEqual(org);
  });

  it("cross-Team org read (403) surfaces as not-found, no foreign data", async () => {
    const client = createAgentsClient(stub(403, { teamId: "foreign" }));
    await expect(client.getTeamOrg("teamA")).rejects.toMatchObject({
      outcome: "not-found",
    });
  });

  it("missing agent (404) surfaces as not-found", async () => {
    const client = createAgentsClient(stub(404, {}));
    await expect(client.getAgent("nope")).rejects.toBeInstanceOf(
      AgentsApiError,
    );
  });

  it("listAgentRuns forwards limit/offset as query params", async () => {
    const fetchImpl = stub(200, []);
    const client = createAgentsClient(fetchImpl);
    await client.listAgentRuns("a1", { limit: 25, offset: 50 });
    expect(fetchImpl).toHaveBeenCalledWith(
      "/api/agents/a1/runs?limit=25&offset=50",
      { method: "GET" },
    );
  });

  it("getRunLogs targets the tab route with a GET (read-only)", async () => {
    const fetchImpl = stub(200, []);
    const client = createAgentsClient(fetchImpl);
    await client.getRunLogs("r1", "llm");
    expect(fetchImpl).toHaveBeenCalledWith("/api/runs/r1/logs/llm", {
      method: "GET",
    });
  });

  it("percent-encodes ids into the BFF path", async () => {
    const fetchImpl = stub(200, { teamId: "a/b", teamName: "x", agents: [] });
    const client = createAgentsClient(fetchImpl);
    await client.getTeamOrg("a/b");
    expect(fetchImpl).toHaveBeenCalledWith("/api/teams/a%2Fb/org", {
      method: "GET",
    });
  });

  it("5xx surfaces as error (distinct from not-found)", async () => {
    const client = createAgentsClient(stub(500, {}));
    await expect(client.getTeamOrg("t")).rejects.toMatchObject({
      outcome: "error",
    });
  });
});
