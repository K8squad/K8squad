// test/tickets/createTicketSheet.test.tsx — the create-ticket slide-over
// behaviour (ISI-4399 S2 + ISI-4501): submit is title-gated, the POST carries
// exactly the buildCreateBody payload (never an assignee — ISI-4501), a 201 hands
// the server item back + closes, a server refusal (403) surfaces honestly instead
// of a fake success, and picking an agent chains EXACTLY ONE dispatch POST while
// leaving it unassigned chains none.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { CreateTicketSheet } from "@/components/tickets/CreateTicketSheet";
import type { WorkItem } from "@/lib/tickets/types";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const PROJECT = "ns/demo";

const AGENTS = {
  agents: [
    { id: "uid-alice", name: "alice", namespace: "ns", skillCount: 0 },
    { id: "uid-bob", name: "bob", namespace: "ns", skillCount: 0 },
  ],
};

/** Route fetch by request: mount fires GET /api/squad/agents, then create/dispatch. */
function route(handlers: {
  create?: (body: unknown) => Response;
  dispatch?: (body: unknown) => Response;
  agents?: Response;
}) {
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url);
    if (u.includes("/api/squad/agents")) {
      return Promise.resolve(handlers.agents ?? jsonResponse(AGENTS, 200));
    }
    if (u.includes("/dispatch")) {
      const body = init?.body ? JSON.parse(init.body as string) : {};
      return Promise.resolve(
        handlers.dispatch?.(body) ??
          jsonResponse(
            {
              workItemId: "ISI-9001",
              fromState: "backlog",
              toState: "todo",
              requestedAgent: body.agentId,
            },
            200,
          ),
      );
    }
    // create — POST /api/projects/{id}/work-items
    const body = init?.body ? JSON.parse(init.body as string) : {};
    return Promise.resolve(
      handlers.create?.(body) ?? jsonResponse({}, 201),
    );
  });
}

/** All POSTs to a given path fragment (the mount GET is excluded). */
function postsTo(fragment: string): unknown[][] {
  return fetchMock.mock.calls.filter(
    ([url, init]) =>
      String(url).includes(fragment) &&
      (init as RequestInit | undefined)?.method === "POST",
  );
}

function parent(id: string, title: string): WorkItem {
  return {
    id,
    projectId: PROJECT,
    parentId: null,
    title,
    state: "backlog",
    blockedReason: null,
    updatedAt: "2026-09-01T00:00:00Z",
  };
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("CreateTicketSheet", () => {
  it("disables submit until a title is present", () => {
    route({});
    render(
      <CreateTicketSheet
        projectId={PROJECT}
        parents={[]}
        onCreated={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    const submit = screen.getByTestId("create-ticket-submit") as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.change(screen.getByTestId("create-ticket-title"), {
      target: { value: "ship it" },
    });
    expect(submit.disabled).toBe(false);
  });

  it("renders the live agent list plus an Unassigned default", async () => {
    route({});
    render(
      <CreateTicketSheet
        projectId={PROJECT}
        parents={[]}
        onCreated={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    const select = screen.getByTestId("create-ticket-assignee") as HTMLSelectElement;
    // Default option is present synchronously; agents arrive after the mount fetch.
    expect(select.value).toBe("");
    await waitFor(() => expect(select.options.length).toBeGreaterThan(1));
    const opts = Array.from(select.options).map((o) => o.textContent);
    expect(opts).toEqual(["Unassigned", "alice", "bob"]);
    // Option VALUE is the agent name (what dispatch matches on), not the UID.
    expect(select.options[1].value).toBe("alice");
  });

  it("POSTs the buildCreateBody payload and hands back the created item", async () => {
    const created: WorkItem = {
      id: "ISI-9001",
      projectId: PROJECT,
      parentId: "ISI-1",
      title: "ship it",
      state: "backlog",
      blockedReason: null,
      updatedAt: "2026-09-14T00:00:00Z",
    };
    route({ create: () => jsonResponse(created, 201) });
    const onCreated = vi.fn();
    const onClose = vi.fn();

    render(
      <CreateTicketSheet
        projectId={PROJECT}
        parents={[parent("ISI-1", "epic")]}
        onCreated={onCreated}
        onClose={onClose}
      />,
    );

    fireEvent.change(screen.getByTestId("create-ticket-title"), {
      target: { value: "  ship it  " },
    });
    fireEvent.change(screen.getByTestId("create-ticket-description"), {
      target: { value: "do the thing" },
    });
    fireEvent.change(screen.getByTestId("create-ticket-parent"), {
      target: { value: "ISI-1" },
    });
    fireEvent.click(screen.getByTestId("create-ticket-submit"));

    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(created));
    expect(onClose).toHaveBeenCalledTimes(1);

    const creates = postsTo("/work-items");
    expect(creates).toHaveLength(1);
    const [url, opts] = creates[0] as [string, RequestInit];
    expect(String(url)).toContain("/api/projects/");
    expect(JSON.parse(opts.body as string)).toEqual({
      title: "ship it",
      body: "do the thing",
      parentId: "ISI-1",
    });
    // Unassigned ⇒ NO dispatch chained.
    expect(postsTo("/dispatch")).toHaveLength(0);
  });

  it("POSTs the full-fidelity body when Priority / Work-mode / Labels are set (no assignee)", async () => {
    const created: WorkItem = {
      id: "ISI-9002",
      projectId: PROJECT,
      parentId: null,
      title: "rich",
      state: "backlog",
      blockedReason: null,
      updatedAt: "2026-09-15T00:00:00Z",
    };
    route({ create: () => jsonResponse(created, 201) });
    const onCreated = vi.fn();

    render(
      <CreateTicketSheet
        projectId={PROJECT}
        parents={[]}
        onCreated={onCreated}
        onClose={vi.fn()}
      />,
    );

    fireEvent.change(screen.getByTestId("create-ticket-title"), {
      target: { value: "rich" },
    });
    fireEvent.change(screen.getByTestId("create-ticket-priority"), {
      target: { value: "high" },
    });
    fireEvent.change(screen.getByTestId("create-ticket-workmode"), {
      target: { value: "planning" },
    });
    fireEvent.change(screen.getByTestId("create-ticket-labels"), {
      target: { value: "backend, urgent-fix, backend" },
    });
    fireEvent.click(screen.getByTestId("create-ticket-submit"));

    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(created));

    const [, opts] = postsTo("/work-items")[0] as [string, RequestInit];
    const body = JSON.parse(opts.body as string);
    // assignee is NEVER in the create body (ISI-4501).
    expect(body).toEqual({
      title: "rich",
      priority: "high",
      workMode: "planning",
      labels: ["backend", "urgent-fix"],
    });
    expect(body).not.toHaveProperty("assignee");
    expect(body).not.toHaveProperty("agentId");
  });

  it("chains EXACTLY ONE dispatch when an agent is selected, reflecting the assignee", async () => {
    const created: WorkItem = {
      id: "ISI-9003",
      projectId: PROJECT,
      parentId: null,
      title: "assigned",
      state: "backlog",
      blockedReason: null,
      updatedAt: "2026-09-16T00:00:00Z",
    };
    route({ create: () => jsonResponse(created, 201) });
    const onCreated = vi.fn();
    const onClose = vi.fn();

    render(
      <CreateTicketSheet
        projectId={PROJECT}
        parents={[]}
        onCreated={onCreated}
        onClose={onClose}
      />,
    );

    fireEvent.change(screen.getByTestId("create-ticket-title"), {
      target: { value: "assigned" },
    });
    // Wait for agents to load, then pick one.
    await waitFor(() =>
      expect(
        (screen.getByTestId("create-ticket-assignee") as HTMLSelectElement)
          .options.length,
      ).toBeGreaterThan(1),
    );
    fireEvent.change(screen.getByTestId("create-ticket-assignee"), {
      target: { value: "bob" },
    });
    fireEvent.click(screen.getByTestId("create-ticket-submit"));

    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));

    // Exactly one create + exactly one dispatch, dispatch carries {agentId: name}.
    expect(postsTo("/work-items").filter(([u]) => !String(u).includes("/dispatch"))).toHaveLength(1);
    const dispatches = postsTo("/dispatch");
    expect(dispatches).toHaveLength(1);
    const [durl, dopts] = dispatches[0] as [string, RequestInit];
    expect(String(durl)).toContain("/api/work-items/ISI-9003/dispatch");
    expect(JSON.parse(dopts.body as string)).toEqual({ agentId: "bob" });

    // Optimistic row reflects the assignee + advanced state (last hand-back).
    const last = onCreated.mock.calls.at(-1)?.[0] as WorkItem;
    expect(last.assignee).toBe("bob");
    expect(last.state).toBe("todo");
  });

  it("keeps the created item and surfaces a dispatch failure inline", async () => {
    const created: WorkItem = {
      id: "ISI-9004",
      projectId: PROJECT,
      parentId: null,
      title: "half-way",
      state: "backlog",
      blockedReason: null,
      updatedAt: "2026-09-16T00:00:00Z",
    };
    route({
      create: () => jsonResponse(created, 201),
      dispatch: () => jsonResponse({ error: "not in team" }, 403),
    });
    const onCreated = vi.fn();
    const onClose = vi.fn();

    render(
      <CreateTicketSheet
        projectId={PROJECT}
        parents={[]}
        onCreated={onCreated}
        onClose={onClose}
      />,
    );

    fireEvent.change(screen.getByTestId("create-ticket-title"), {
      target: { value: "half-way" },
    });
    await waitFor(() =>
      expect(
        (screen.getByTestId("create-ticket-assignee") as HTMLSelectElement)
          .options.length,
      ).toBeGreaterThan(1),
    );
    fireEvent.change(screen.getByTestId("create-ticket-assignee"), {
      target: { value: "alice" },
    });
    fireEvent.click(screen.getByTestId("create-ticket-submit"));

    // The created item is handed back (not lost) even though dispatch failed.
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(created));
    await waitFor(() =>
      expect(screen.getByTestId("create-ticket-error").textContent).toContain(
        "team",
      ),
    );
    // Sheet stays open so the failure is visible; the button offers a retry.
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByTestId("create-ticket-submit").textContent).toContain(
      "Retry assign",
    );

    // A retry re-uses the created item — no second create, one more dispatch.
    expect(postsTo("/work-items").filter(([u]) => !String(u).includes("/dispatch"))).toHaveLength(1);
    fireEvent.click(screen.getByTestId("create-ticket-submit"));
    await waitFor(() => expect(postsTo("/dispatch")).toHaveLength(2));
    expect(postsTo("/work-items").filter(([u]) => !String(u).includes("/dispatch"))).toHaveLength(1);
  });

  it("surfaces a server refusal instead of faking a create", async () => {
    route({ create: () => jsonResponse({ error: "forbidden" }, 403) });
    const onCreated = vi.fn();
    const onClose = vi.fn();

    render(
      <CreateTicketSheet
        projectId={PROJECT}
        parents={[]}
        onCreated={onCreated}
        onClose={onClose}
      />,
    );
    fireEvent.change(screen.getByTestId("create-ticket-title"), {
      target: { value: "nope" },
    });
    fireEvent.click(screen.getByTestId("create-ticket-submit"));

    await waitFor(() =>
      expect(screen.getByTestId("create-ticket-error")).toBeTruthy(),
    );
    expect(onCreated).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByTestId("create-ticket-error").textContent).toContain(
      "permission",
    );
  });

  it("locks the parent and files a CHILD without touching any control in fixedParent mode", async () => {
    // Gap A regression: an untouched sub-ticket form must POST parentId, never a
    // silent top-level item.
    const created: WorkItem = {
      id: "ISI-9003",
      projectId: PROJECT,
      parentId: "ISI-7",
      title: "child",
      state: "backlog",
      blockedReason: null,
      updatedAt: "2026-09-16T00:00:00Z",
    };
    fetchMock.mockResolvedValue(jsonResponse(created, 201));
    const onCreated = vi.fn();

    render(
      <CreateTicketSheet
        projectId={PROJECT}
        parents={[]}
        fixedParent={parent("ISI-7", "Umbrella epic")}
        onCreated={onCreated}
        onClose={vi.fn()}
      />,
    );

    // Header + locked parent line; the editable dropdown is gone.
    expect(screen.getByText("New sub-ticket")).toBeTruthy();
    expect(
      screen.getByTestId("create-ticket-parent-fixed").textContent,
    ).toContain("Umbrella epic");
    expect(screen.queryByTestId("create-ticket-parent")).toBeNull();

    // Only the title is set — the parent is never touched.
    fireEvent.change(screen.getByTestId("create-ticket-title"), {
      target: { value: "child" },
    });
    fireEvent.click(screen.getByTestId("create-ticket-submit"));

    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(created));
    const [, opts] = fetchMock.mock.calls[0];
    expect(JSON.parse(opts.body as string)).toEqual({
      title: "child",
      parentId: "ISI-7",
    });
  });

  it("closes on scrim click and the × button", () => {
    route({});
    const onClose = vi.fn();
    render(
      <CreateTicketSheet
        projectId={PROJECT}
        parents={[]}
        onCreated={vi.fn()}
        onClose={onClose}
      />,
    );
    fireEvent.click(screen.getByTestId("create-ticket-cancel-x"));
    fireEvent.click(screen.getByTestId("create-ticket-scrim"));
    expect(onClose).toHaveBeenCalledTimes(2);
  });
});
