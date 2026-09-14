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
