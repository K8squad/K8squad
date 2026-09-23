// test/tickets/api.test.ts — the BFF read contract for the Tickets screen
// (ISI-4132): the M1.5 board read model (ISI-4131) answers a BARE JSON array,
// which the 8.14d-era { items } unwrap silently dropped ("No tickets in this
// Project" with a full board upstream).
//
// Also pins the state-transition WRITE body (ISI-4225): the console client must
// serialize the apiserver's field names {toState, fromState} — byte-for-byte the
// shared fixture, which the Go handler contract test
// (internal/apiserver/workitemstate_contract_test.go) replays through the real
// handler so neither tree can drift from the other silently.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

import {
  agentOptionLabel,
  fetchViewerRole,
  listSquadAgents,
  listWorkItems,
  patchWorkItemState,
} from "@/lib/tickets/api";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

describe("listWorkItems payload shapes", () => {
  beforeEach(() => fetchMock.mockReset());

  it("accepts the M1.5 bare array", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse([{ id: "wi-1", title: "ship", state: "todo" }]),
    );
    const items = await listWorkItems("bmad-squad/bmad-demo-project");
    expect(items).toHaveLength(1);
    expect(items[0].id).toBe("wi-1");
  });

  it("keeps the legacy { items } envelope working", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ items: [{ id: "wi-2", title: "legacy", state: "done" }] }),
    );
    const items = await listWorkItems("p");
    expect(items).toHaveLength(1);
    expect(items[0].id).toBe("wi-2");
  });

  it("renders an empty board for an empty array (never null upstream)", async () => {
    fetchMock.mockResolvedValue(jsonResponse([]));
    expect(await listWorkItems("p")).toEqual([]);
  });
});

// The single source of truth for the cross-tree request-body contract: this
// exact file is POSTed to the apiserver handler by the Go contract test.
// (cwd-relative — under jsdom, import.meta.url is not a file: URL.)
const contractBody = readFileSync(
  resolve(process.cwd(), "test/tickets/fixtures/state-transition-request.json"),
  "utf8",
).trim();

describe("patchWorkItemState request contract (ISI-4225)", () => {
  beforeEach(() => fetchMock.mockReset());

  it("serializes exactly the fixture body the apiserver handler decodes", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        workItemId: "wi-1",
        fromState: "todo",
        toState: "in_progress",
      }),
    );
    await patchWorkItemState("wi-1", {
      toState: "in_progress",
      fromState: "todo",
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [
      string,
      RequestInit & { headers: Record<string, string> },
    ];
    expect(url).toBe("/api/work-items/wi-1/state");
    expect(init.method).toBe("PATCH");
    expect(init.headers["content-type"]).toBe("application/json");
    // EXACT string equality — key names AND order — against the shared fixture.
    // The 8.14a-era client spoke {to, expectedFrom} here and every browser lane
    // move 400'd ("toState required") because the BFF forwards verbatim.
    expect(init.body).toBe(contractBody);
  });

  it("resolves the target state on 200", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        workItemId: "wi-1",
        fromState: "todo",
        toState: "in_progress",
      }),
    );
    const out = await patchWorkItemState("wi-1", {
      toState: "in_progress",
      fromState: "todo",
    });
    expect(out).toEqual({ state: "in_progress" });
  });

  it("rethrows non-2xx as ApiError so the screen re-syncs (never keeps client state)", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ error: "toState required" }, 400));
    await expect(
      patchWorkItemState("wi-1", {
        toState: "in_progress",
        fromState: "todo",
      }),
    ).rejects.toMatchObject({ status: 400 });
  });
});

describe("fetchViewerRole reads /auth/me's globalRole (ISI-4496 RBAC gate)", () => {
  beforeEach(() => fetchMock.mockReset());

  it("returns the caller's globalRole so a signed-in caller clears the create gate", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ globalRole: "user", username: "ada" }));
    // Regression: reading the (non-existent) `role` field pinned every caller to "viewer",
    // hiding "+ New issue" for admins too. "user" !== "viewer" ⇒ create is offered.
    expect(await fetchViewerRole()).toBe("user");
  });

  it("passes admin through unchanged", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ globalRole: "admin", username: "root" }));
    expect(await fetchViewerRole()).toBe("admin");
  });

  it("fails closed to viewer when globalRole is absent from the payload", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ username: "ada" }));
    expect(await fetchViewerRole()).toBe("viewer");
  });

  it("fails closed to viewer on a non-200 (no session / expired)", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ error: "no valid session" }, 401));
    expect(await fetchViewerRole()).toBe("viewer");
  });

  it("fails closed to viewer when the body is unreadable (catch branch: network/JSON error)", async () => {
    // A 200 with a non-JSON body makes res.json() throw — the same catch that
    // swallows a rejected fetch / network drop, proving the fail-closed default.
    fetchMock.mockResolvedValue(
      new Response("<html>gateway</html>", {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    expect(await fetchViewerRole()).toBe("viewer");
  });
});

describe("listSquadAgents dedupe + role projection (ISI-4807 §1/§2)", () => {
  beforeEach(() => fetchMock.mockReset());

  it("collapses the fleet list to one option per agent name (2× → 1×)", async () => {
    // A fleet/admin caller's GET /api/squad/agents spans every squad namespace,
    // so an agent living in two namespaces returns twice — the reported "2× the
    // agents". Dispatch matches on name (name-unique per team), so dedupe on name.
    fetchMock.mockResolvedValue(
      jsonResponse({
        agents: [
          { id: "ns1-rev", name: "agent:reviewer", role: "Code Reviewer", namespace: "k8squad-system" },
          { id: "ns2-rev", name: "agent:reviewer", role: "Code Reviewer", namespace: "k8squad-preview" },
          { id: "ns1-bld", name: "agent:builder", role: "Implementer", namespace: "k8squad-system" },
          { id: "ns2-bld", name: "agent:builder", role: "Implementer", namespace: "k8squad-preview" },
        ],
      }),
    );
    const agents = await listSquadAgents();
    expect(agents.map((a) => a.name)).toEqual(["agent:reviewer", "agent:builder"]);
    // First occurrence wins — the id is stable, not doubled.
    expect(agents[0].id).toBe("ns1-rev");
  });

  it("carries the referenced role through so the picker can label role — name", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ agents: [{ id: "a1", name: "agent:pm", role: "Product Manager" }] }),
    );
    const [pm] = await listSquadAgents();
    expect(pm.role).toBe("Product Manager");
  });

  it("leaves role undefined when the agent declares none", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ agents: [{ id: "a1", name: "agent:pm" }] }));
    const [pm] = await listSquadAgents();
    expect(pm.role).toBeUndefined();
  });
});

describe("agentOptionLabel (ISI-4807 §2)", () => {
  it("renders role — name when a role is present", () => {
    expect(agentOptionLabel({ id: "a1", name: "agent:pm", role: "Product Manager" })).toBe(
      "Product Manager — agent:pm",
    );
  });

  it("falls back to the bare name when no role is set", () => {
    expect(agentOptionLabel({ id: "a1", name: "agent:pm" })).toBe("agent:pm");
  });
});
