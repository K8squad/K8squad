import { describe, it, expect, vi, afterEach } from "vitest";
import { NextRequest } from "next/server";
import { GET as toolUsageGET } from "@/app/api/telemetry/tool-usage/route";
import {
  fetchToolUsage,
  totalToolCalls,
  totalSkillLoads,
  formatSeconds,
  type ToolUsagePayload,
} from "@/lib/toolusage";

// Epic D / ISI-3288 (D3): the tool-usage BFF proxy + lib contract. The
// apiserver's status surfaces VERBATIM (501 unconfigured and 503 unreachable
// stay themselves — the panel renders the honest degraded state); ?agent=
// scopes the upstream query; the pure helpers drive the panel's KPI numbers.

const realFetch = globalThis.fetch;

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function makeReq(path: string): NextRequest {
  return new NextRequest(`http://console.local${path}`);
}

afterEach(() => {
  globalThis.fetch = realFetch;
  vi.restoreAllMocks();
});

describe("GET /api/telemetry/tool-usage — the read proxy", () => {
  it("relays status + body verbatim (501 stays 501)", async () => {
    const upstream = vi.fn(async () =>
      jsonResponse(501, { error: "tool-usage read model not configured" }),
    );
    globalThis.fetch = upstream as unknown as typeof fetch;

    const res = await toolUsageGET(makeReq("/api/telemetry/tool-usage"));

    expect(res.status).toBe(501);
    expect(await res.json()).toEqual({
      error: "tool-usage read model not configured",
    });
  });

  it("scopes ?agent= into the upstream query", async () => {
    const upstream = vi.fn(async () => jsonResponse(200, { agents: [], mcp: [] }));
    globalThis.fetch = upstream as unknown as typeof fetch;

    const res = await toolUsageGET(
      makeReq("/api/telemetry/tool-usage?agent=coder-1"),
    );

    expect(res.status).toBe(200);
    const calledURL = String((upstream.mock.calls[0] as unknown[])[0]);
    expect(calledURL).toContain(
      "/api/telemetry/tool-usage?agent=coder-1",
    );
  });

  it("omits the query string entirely without ?agent=", async () => {
    const upstream = vi.fn(async () => jsonResponse(200, { agents: [], mcp: [] }));
    globalThis.fetch = upstream as unknown as typeof fetch;

    await toolUsageGET(makeReq("/api/telemetry/tool-usage"));

    const calledURL = String((upstream.mock.calls[0] as unknown[])[0]);
    expect(calledURL).toContain("/api/telemetry/tool-usage");
    expect(calledURL).not.toContain("?");
  });
});

describe("fetchToolUsage", () => {
  it("returns the payload on 200 and raises with status on non-2xx", async () => {
    const payload: ToolUsagePayload = {
      agents: [
        {
          agent: "coder-1",
          toolCalls: [
            { tool: "bash", skill: "tdd", calls: 12 },
            { tool: "ripgrep", calls: 7 },
          ],
          skillLoads: [{ skill: "tdd", loads: 3 }],
          mcp: [],
        },
      ],
      mcp: [
        { server: "github", tool: "create_issue", calls: 5, avgSeconds: 0.42 },
      ],
      reporting: true,
    };
    globalThis.fetch = vi.fn(async () =>
      jsonResponse(200, payload),
    ) as unknown as typeof fetch;

    const got = await fetchToolUsage("coder-1");
    expect(got).toEqual(payload);

    globalThis.fetch = vi.fn(async () => jsonResponse(503, {})) as unknown as typeof fetch;
    await expect(fetchToolUsage()).rejects.toThrow("503");
  });
});

describe("panel aggregation helpers", () => {
  const agent: ToolUsagePayload["agents"][number] = {
    agent: "a",
    toolCalls: [
      { tool: "bash", skill: "tdd", calls: 12 },
      { tool: "ripgrep", calls: 7 },
    ],
    skillLoads: [
      { skill: "tdd", loads: 3 },
      { skill: "review", loads: 2 },
    ],
    mcp: [],
  };

  it("sums tool calls and skill loads", () => {
    expect(totalToolCalls(agent)).toBe(19);
    expect(totalSkillLoads(agent)).toBe(5);
  });

  it("sums to zero on empty rows", () => {
    expect(totalToolCalls({ ...agent, toolCalls: [] })).toBe(0);
    expect(totalSkillLoads({ ...agent, skillLoads: [] })).toBe(0);
  });

  it("formats sub-second and multi-second durations", () => {
    expect(formatSeconds(0.42)).toBe("420ms");
    expect(formatSeconds(2.34)).toBe("2.3s");
  });
});
