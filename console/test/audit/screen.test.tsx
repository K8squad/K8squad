import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { AuditTrailScreen } from "@/components/audit/AuditTrailScreen";
import type { AuditPage } from "@/lib/audit";

afterEach(cleanup);

// The 2.6 screen at the component boundary: rows render who/what/when/result from the read
// model, the honest states (denied / forbidden-actor / unconfigured 501 / error) each render
// their own message, and id-cursor pagination appends older rows via `before`.

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function page(events: AuditPage["events"], nextBefore: number | null = null): AuditPage {
  return { events, nextBefore };
}

describe("<AuditTrailScreen> — 2.6 ACs", () => {
  it("renders who / what / when / result rows from the read model", async () => {
    render(
      <AuditTrailScreen
        load={async () =>
          jsonResponse(200, page([
            {
              id: 42,
              workItemId: "11111111-1111-1111-1111-111111111111",
              runId: null,
              eventType: "state_transition",
              principal: "user:jane",
              fromState: "in_progress",
              toState: "in_review",
              createdAt: "2026-08-21T09:56:28.480Z",
            },
            {
              id: 41,
              eventType: "claim_acquired",
              principal: "agent:coder",
              payload: { detail: "fence 3" },
              createdAt: "2026-08-21T09:40:00Z",
            },
          ]))
        }
      />,
    );
    await waitFor(() => screen.getByTestId("audit-table"));
    expect(screen.getByText("jane (user)")).toBeTruthy();
    expect(screen.getByText("coder (agent)")).toBeTruthy();
    expect(screen.getByText("in_progress → in_review")).toBeTruthy();
    expect(screen.getByText("fence 3")).toBeTruthy();
    expect(screen.getByText("…11111111")).toBeTruthy();
    expect(screen.getAllByTestId("audit-row")).toHaveLength(2);
  });

  it("403 renders the specific self-scope explanation (admin-scoped surface)", async () => {
    render(<AuditTrailScreen load={async () => jsonResponse(403, {})} />);
    await waitFor(() => screen.getByTestId("audit-forbidden"));
    expect(screen.getByTestId("audit-forbidden").textContent).toContain("admin-scoped");
  });

  it("deny collapse (401) renders denied; 501 renders unconfigured; 5xx renders error", async () => {
    const { unmount } = render(<AuditTrailScreen load={async () => jsonResponse(401, {})} />);
    await waitFor(() => screen.getByTestId("audit-denied"));
    unmount();

    render(<AuditTrailScreen load={async () => jsonResponse(501, {})} />);
    await waitFor(() => screen.getByTestId("audit-unconfigured"));
    cleanup();

    render(<AuditTrailScreen load={async () => jsonResponse(502, {})} />);
    await waitFor(() => screen.getByTestId("audit-error"));
  });

  it("an empty trail renders the honest no-match row, never a fabricated table body", async () => {
    render(<AuditTrailScreen load={async () => jsonResponse(200, page([]))} />);
    await waitFor(() => screen.getByTestId("audit-table"));
    expect(screen.getByText(/No audit events match/)).toBeTruthy();
  });

  it("applies filters by refetching with the serialized query", async () => {
    const calls: string[] = [];
    render(
      <AuditTrailScreen
        load={async (qs) => {
          calls.push(qs);
          return jsonResponse(200, page([]));
        }}
      />,
    );
    await waitFor(() => screen.getByTestId("audit-table"));
    expect(calls[0]).toBe("limit=50");

    fireEvent.change(screen.getByTestId("audit-filter-actor"), { target: { value: "user:jane" } });
    fireEvent.click(screen.getByTestId("audit-filter-apply"));
    await waitFor(() => expect(calls[1]).toBe("actor=user%3Ajane&limit=50"));
  });

  it("load older appends the next page via the `before` cursor and hides at the tail", async () => {
    let call = 0;
    render(
      <AuditTrailScreen
        load={async (qs) => {
          call++;
          if (call === 1) {
            return jsonResponse(200, page([
              { id: 20, eventType: "comment_added", principal: "user:jane", createdAt: "2026-08-21T09:00:00Z" },
            ], 10));
          }
          expect(qs).toContain("before=10");
          return jsonResponse(200, page([
            { id: 5, eventType: "completed", principal: "agent:coder", createdAt: "2026-08-20T09:00:00Z" },
          ]));
        }}
      />,
    );
    await waitFor(() => screen.getByTestId("audit-load-older"));
    fireEvent.click(screen.getByTestId("audit-load-older"));
    await waitFor(() => expect(screen.getAllByTestId("audit-row")).toHaveLength(2));
    // Tail reached: the affordance swaps for the end-of-trail note.
    await waitFor(() => screen.getByTestId("audit-tail"));
  });
});
