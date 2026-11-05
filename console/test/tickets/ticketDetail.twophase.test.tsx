// test/tickets/ticketDetail.twophase.test.tsx — the two-phase comment UI
// (ISI-7095 / ISI-7088) on the ticket-detail screen. ISI-7094 split the thread:
// the default read carries the human conversation + the ONE per-run summary,
// while the run sub-step firehose (kind='progress') is served from …/activity
// and surfaces under a per-run "Activity (N events)" expander, collapsed by
// default. These tests assert the AC end-to-end against a stubbed BFF:
//   - a completed run's summary renders FIRST-CLASS (its own chip), not as a log;
//   - the progress log is COLLAPSED by default and reveals per-run on demand;
//   - no [run/[tool/[status wire literals leak into the default comment view.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  within,
  fireEvent,
} from "@testing-library/react";
import { TicketDetail } from "@/components/tickets/TicketDetail";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

class NoopEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: (() => void) | null = null;
  addEventListener() {}
  removeEventListener() {}
  close() {}
}
vi.stubGlobal("EventSource", NoopEventSource as unknown as typeof EventSource);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

// A completed (done) run: the default thread carries ONE summary comment + a
// human reply — never a progress row (ISI-7094 drops those from this read).
const THREAD: Record<string, unknown> = {
  WorkItemID: "wi-1",
  Title: "ship the thing",
  Description: "the full description",
  State: "done",
  BlockedReason: "",
  Comments: [
    {
      author: "agent:builder",
      body: "**What I did:** wired the seam. **Next:** review.",
      createdAt: "2026-11-05T10:10:00Z",
      kind: "summary",
      runId: "abcd1234-0000-0000-0000-000000000000",
    },
    {
      author: "user:alice",
      body: "looks good",
      createdAt: "2026-11-05T10:15:00Z",
      kind: "human",
    },
  ],
  Holder: "",
  RunID: "",
  statusHistory: [],
};

// The progress feed GET …/activity serves — two runs' worth of sub-steps, each
// carrying the raw [run/[tool/[status envelope the default view must never show.
const ACTIVITY = [
  {
    author: "run/abcd1234",
    body: "[run abcd1234][untrusted] reading the spec",
    createdAt: "2026-11-05T10:00:00Z",
    kind: "progress",
    runId: "abcd1234-0000-0000-0000-000000000000",
  },
  {
    author: "run/abcd1234",
    body: "[run abcd1234][tool:edit/result(ok)] wired the seam",
    createdAt: "2026-11-05T10:05:00Z",
    kind: "progress",
    runId: "abcd1234-0000-0000-0000-000000000000",
  },
  {
    author: "run/ef567890",
    body: "[run ef567890][status] in_review",
    createdAt: "2026-11-05T09:00:00Z",
    kind: "progress",
    runId: "ef567890-0000-0000-0000-000000000000",
  },
];

function routeFetch(activity: unknown = ACTIVITY) {
  fetchMock.mockImplementation((url: string) => {
    const u = String(url);
    if (u.includes("/api/session")) {
      return Promise.resolve(jsonResponse({ globalRole: "viewer" }));
    }
    if (u.includes("/api/squad/agents")) {
      return Promise.resolve(jsonResponse({ agents: [] }));
    }
    // The two-phase progress feed — MUST be matched before the thread catch-all.
    if (u.includes("/api/work-items/") && u.includes("/activity")) {
      return Promise.resolve(jsonResponse(activity));
    }
    if (u.includes("/api/work-items/") && u.endsWith("wi-1")) {
      return Promise.resolve(jsonResponse(THREAD));
    }
    if (u.includes("/api/work-items/")) {
      return Promise.resolve(jsonResponse(THREAD));
    }
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse([]));
    return Promise.resolve(jsonResponse([], 200));
  });
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail two-phase comments (ISI-7095)", () => {
  it("renders the summary first-class and no progress wire literals in the default view", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() =>
      expect(screen.getByTestId("detail-description")).toBeTruthy(),
    );

    // The summary comment wears its first-class marker chip.
    const summaryChip = screen.getByTestId("activity-summary-chip");
    expect(summaryChip.textContent).toMatch(/summary/i);

    // No raw [run / [tool / [status literals anywhere in the rendered thread.
    const activitySection = screen.getByTestId("detail-activity");
    expect(activitySection.textContent).not.toMatch(/\[run\b/);
    expect(activitySection.textContent).not.toMatch(/\[tool:/);
    expect(activitySection.textContent).not.toMatch(/\[status\]/);
  });

  it("collapses the progress log under an 'Activity (N events)' expander, revealed on demand, grouped per run", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    // The expander appears once the feed loads, labelled with the total count.
    const toggle = await screen.findByTestId("run-activity-toggle");
    expect(toggle.textContent).toMatch(/Activity \(3 events\)/);
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    // Collapsed by default — the log body is not in the DOM.
    expect(screen.queryByTestId("run-activity-body")).toBeNull();

    // Expand → the per-run groups appear.
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const body = screen.getByTestId("run-activity-body");
    const groups = within(body).getAllByTestId("run-activity-group");
    expect(groups).toHaveLength(2);
    // Runs ordered chronologically by first event: ef567890 (09:00) before
    // abcd1234 (10:00).
    expect(groups[0].getAttribute("data-run-id")).toBe(
      "ef567890-0000-0000-0000-000000000000",
    );
    expect(groups[1].getAttribute("data-run-id")).toBe(
      "abcd1234-0000-0000-0000-000000000000",
    );
    // All three progress events render (parsed, not raw).
    expect(within(body).getAllByTestId("run-activity-event")).toHaveLength(3);
  });

  it("renders no expander when the run has no progress (discussion threads unaffected)", async () => {
    routeFetch([]);
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() =>
      expect(screen.getByTestId("detail-description")).toBeTruthy(),
    );
    // Give the activity fetch a tick to resolve, then assert no expander.
    await waitFor(() =>
      expect(screen.queryByTestId("run-activity-toggle")).toBeNull(),
    );
  });
});
