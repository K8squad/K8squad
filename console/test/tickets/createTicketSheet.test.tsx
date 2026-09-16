// test/tickets/createTicketSheet.test.tsx — the create-ticket slide-over
// behaviour (ISI-4399 S2): submit is title-gated, the POST carries exactly the
// buildCreateBody payload, a 201 hands the server item back + closes, and a
// server refusal (403) surfaces honestly instead of a fake success.

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
    fetchMock.mockResolvedValue(jsonResponse(created, 201));
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

    const [url, opts] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/projects/");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body as string)).toEqual({
      title: "ship it",
      body: "do the thing",
      parentId: "ISI-1",
    });
  });

  it("POSTs the full-fidelity body when Priority / Work-mode / Labels are set", async () => {
    const created: WorkItem = {
      id: "ISI-9002",
      projectId: PROJECT,
      parentId: null,
      title: "rich",
      state: "backlog",
      blockedReason: null,
      updatedAt: "2026-09-15T00:00:00Z",
    };
    fetchMock.mockResolvedValue(jsonResponse(created, 201));
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

    const [, opts] = fetchMock.mock.calls[0];
    expect(JSON.parse(opts.body as string)).toEqual({
      title: "rich",
      priority: "high",
      workMode: "planning",
      labels: ["backend", "urgent-fix"],
    });
  });

  it("surfaces a server refusal instead of faking a create", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ error: "forbidden" }, 403));
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
