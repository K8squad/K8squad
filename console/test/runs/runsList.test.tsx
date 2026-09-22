// test/runs/runsList.test.tsx — ISI-4575 runs list screen (shared by /runs and
// /projects/{id}/runs). Covers: fleet vs project-scoped endpoints, filter params on the wire
// (phase / agent / window / limit / offset), pagination (prev/next disable rules), phase chips,
// deep links into /runs/{runId}?wi=…, the honest degrade paths (404/501 not-available, 5xx
// error + retry), and the empty state with clear-filters CTA. Route-aware fetch stub, same
// discipline as test/overview/fleetOverview.test.tsx.

import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import {
  RunsList,
  RUNS_PAGE_SIZE,
  RUNS_POLL_MS,
  formatAge,
  formatDuration,
  runHref,
  type RunListItem,
} from "@/components/runs/RunsList";

// next/navigation's useRouter needs the App Router context (absent in jsdom) — mock it so
// row-click navigation is observable without a Next runtime.
const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
}));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
beforeEach(() => push.mockClear());

function run(over: Partial<RunListItem> = {}): RunListItem {
  return {
    id: "run-1",
    name: "run-1",
    phase: "Running",
    workItemRef: "11111111-2222-3333-4444-555555555555",
    projectRef: "webapp",
    agents: ["coder"],
    totalTokens: 1234,
    startedAt: new Date(Date.now() - 5 * 60_000).toISOString(),
    durationSeconds: 300,
    ...over,
  };
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

/** Record every fetch; answer per-URL via the supplied matcher. */
function stubFetch(answer: (url: string) => Response) {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      calls.push(url);
      return answer(url);
    }),
  );
  return calls;
}

describe("RunsList — endpoints and ready render", () => {
  it("fleet view reads GET /api/runs with default pagination params", async () => {
    const calls = stubFetch(() => jsonResponse(200, [run()]));
    render(<RunsList />);
    await screen.findByTestId("runs-table");
    expect(calls[0]).toContain("/api/runs?");
    expect(calls[0]).toContain(`limit=${RUNS_PAGE_SIZE}`);
    expect(calls[0]).toContain("offset=0");
  });

  it("project view reads the project-scoped endpoint and hides the Project column", async () => {
    const calls = stubFetch(() => jsonResponse(200, [run()]));
    render(<RunsList projectId="webapp" projectName="webapp" />);
    await screen.findByTestId("runs-table");
    expect(calls[0]).toContain("/api/projects/webapp/runs?");
    expect(screen.queryByText("Project")).toBeNull();
    expect(screen.getByText("Runs — webapp")).toBeTruthy();
  });

  it("renders phase chip, agent, tokens and a deep link to /runs/{runId}?wi=…", async () => {
    stubFetch(() => jsonResponse(200, [run()]));
    render(<RunsList />);
    const row = await screen.findByTestId("runs-row-run-1");
    const chip = row.querySelector(".phase-chip");
    expect(chip?.getAttribute("data-tone")).toBe("running");
    expect(chip?.textContent).toBe("Running");
    expect(screen.getByText("coder")).toBeTruthy();
    expect(screen.getByText("1.2k")).toBeTruthy();
    const link = screen.getByTestId("runs-link-run-1") as HTMLAnchorElement;
    expect(link.href).toContain("/runs/run-1");
    expect(link.href).toContain("wi=11111111-2222-3333-4444-555555555555");
  });

  it("renders — for agent/tokens when the read model does not surface them", async () => {
    stubFetch(() =>
      jsonResponse(200, [run({ agents: null, totalTokens: null })]),
    );
    render(<RunsList />);
    await screen.findByTestId("runs-row-run-1");
    const dashes = screen.getAllByText("—");
    expect(dashes.length).toBeGreaterThanOrEqual(2);
  });

  it("row click navigates to the run detail", async () => {
    stubFetch(() => jsonResponse(200, [run()]));
    render(<RunsList />);
    fireEvent.click(await screen.findByTestId("runs-row-run-1"));
    expect(push).toHaveBeenCalledWith(
      `/runs/run-1?wi=${encodeURIComponent("11111111-2222-3333-4444-555555555555")}`,
    );
  });
});

describe("RunsList — filters", () => {
  it("phase and window selects ride the query string; agent debounces then applies", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const calls = stubFetch(() => jsonResponse(200, [run()]));
      render(<RunsList />);
      await screen.findByTestId("runs-table");

      fireEvent.change(screen.getByTestId("runs-filter-phase"), {
        target: { value: "Paused" },
      });
      fireEvent.change(screen.getByTestId("runs-filter-window"), {
        target: { value: "24h" },
      });
      fireEvent.change(screen.getByTestId("runs-filter-agent"), {
        target: { value: "security-scanner" },
      });
      await vi.advanceTimersByTimeAsync(400);

      await waitFor(() => {
        const last = calls[calls.length - 1];
        expect(last).toContain("phase=Paused");
        expect(last).toContain("window=24h");
        expect(last).toContain("agent=security-scanner");
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("a filter change resets pagination to offset 0", async () => {
    const full = Array.from({ length: RUNS_PAGE_SIZE }, (_, i) =>
      run({ id: `run-${i}`, name: `run-${i}` }),
    );
    const calls = stubFetch(() => jsonResponse(200, full));
    render(<RunsList />);
    await screen.findByTestId("runs-table");

    fireEvent.click(screen.getByTestId("runs-next"));
    await waitFor(() =>
      expect(calls[calls.length - 1]).toContain(`offset=${RUNS_PAGE_SIZE}`),
    );

    fireEvent.change(screen.getByTestId("runs-filter-phase"), {
      target: { value: "Failed" },
    });
    await waitFor(() =>
      expect(calls[calls.length - 1]).toContain("offset=0"),
    );
  });

  it("clear-all restores the unfiltered query", async () => {
    const calls = stubFetch(() => jsonResponse(200, [run()]));
    render(<RunsList />);
    await screen.findByTestId("runs-table");
    fireEvent.change(screen.getByTestId("runs-filter-phase"), {
      target: { value: "Failed" },
    });
    await screen.findByTestId("runs-clear-filters");
    fireEvent.click(screen.getByTestId("runs-clear-filters"));
    await waitFor(() => {
      const last = calls[calls.length - 1];
      expect(last).not.toContain("phase=");
    });
  });
});

describe("RunsList — pagination", () => {
  it("Previous is disabled on the first page; Next is disabled on a short page", async () => {
    stubFetch(() => jsonResponse(200, [run()]));
    render(<RunsList />);
    await screen.findByTestId("runs-pagination");
    expect((screen.getByTestId("runs-prev") as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId("runs-next") as HTMLButtonElement).disabled).toBe(true);
  });

  it("Next advances the offset on a full page; Previous goes back", async () => {
    const full = Array.from({ length: RUNS_PAGE_SIZE }, (_, i) =>
      run({ id: `run-${i}`, name: `run-${i}` }),
    );
    const calls = stubFetch(() => jsonResponse(200, full));
    render(<RunsList />);
    await screen.findByTestId("runs-pagination");
    expect((screen.getByTestId("runs-next") as HTMLButtonElement).disabled).toBe(false);

    fireEvent.click(screen.getByTestId("runs-next"));
    await waitFor(() =>
      expect(calls[calls.length - 1]).toContain(`offset=${RUNS_PAGE_SIZE}`),
    );
    await screen.findByTestId("runs-table");
    expect((screen.getByTestId("runs-prev") as HTMLButtonElement).disabled).toBe(false);

    fireEvent.click(screen.getByTestId("runs-prev"));
    await waitFor(() =>
      expect(calls[calls.length - 1]).toContain("offset=0"),
    );
  });
});

describe("RunsList — degrade and empty states", () => {
  it("404/501 renders the honest not-available card", async () => {
    stubFetch(() => jsonResponse(501, { error: "not wired" }));
    render(<RunsList />);
    await screen.findByTestId("runs-not-available");
    expect(screen.queryByTestId("runs-table")).toBeNull();
  });

  it("5xx renders the error card and Retry refetches", async () => {
    let ok = false;
    const calls = stubFetch(() =>
      ok ? jsonResponse(200, [run()]) : jsonResponse(502, { error: "bad gateway" }),
    );
    render(<RunsList />);
    await screen.findByTestId("runs-error");
    ok = true;
    fireEvent.click(screen.getByText("Retry"));
    await screen.findByTestId("runs-table");
    expect(calls.length).toBeGreaterThanOrEqual(2);
  });

  it("empty without filters shows the plain empty state; with filters a clear CTA", async () => {
    stubFetch((url) =>
      jsonResponse(200, url.includes("phase=") ? [] : [run()]),
    );
    render(<RunsList />);
    await screen.findByTestId("runs-table");
    fireEvent.change(screen.getByTestId("runs-filter-phase"), {
      target: { value: "Cancelled" },
    });
    await screen.findByTestId("runs-empty");
    fireEvent.click(screen.getByText("Clear filters"));
    await screen.findByTestId("runs-table");
  });
});

describe("RunsList — run→action traceability (ISI-4777)", () => {
  it("surfaces the work-item title and triggering principal on the row", async () => {
    stubFetch(() =>
      jsonResponse(200, [
        run({ workItemTitle: "Ask: add auth badge", triggeredBy: "henrik" }),
      ]),
    );
    render(<RunsList />);
    const wi = await screen.findByTestId("runs-row-wi-run-1");
    expect(wi.textContent).toContain("Ask: add auth badge");
    expect(wi.textContent).toContain("henrik");
    // The full title is the hover target, not the truncated uuid.
    expect(wi.getAttribute("title")).toBe("Ask: add auth badge");
  });

  it("falls back to the wi-<uuid> stub when the title is not surfaced", async () => {
    stubFetch(() => jsonResponse(200, [run({ workItemTitle: undefined })]));
    render(<RunsList />);
    const wi = await screen.findByTestId("runs-row-wi-run-1");
    expect(wi.textContent).toContain("wi 11111111");
  });

  it("shows the Live auto-refresh indicator on the newest page", async () => {
    stubFetch(() => jsonResponse(200, [run()]));
    render(<RunsList />);
    await screen.findByTestId("runs-table");
    expect(screen.getByTestId("runs-live")).toBeTruthy();
  });
});

describe("RunsList — auto-refresh (ISI-4777)", () => {
  it("silently refetches the newest page on an interval, surfacing new runs", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let rows = [run({ id: "run-1", name: "run-1" })];
      const calls = stubFetch(() => jsonResponse(200, rows));
      render(<RunsList />);
      await screen.findByTestId("runs-row-run-1");
      const initialCalls = calls.length;

      // A new run fires after the page loaded (Henrik's comment path).
      rows = [run({ id: "run-2", name: "run-2" }), rows[0]];
      await vi.advanceTimersByTimeAsync(RUNS_POLL_MS + 50);

      await waitFor(() => expect(calls.length).toBeGreaterThan(initialCalls));
      await screen.findByTestId("runs-row-run-2");
      // No spinner flash — the table stayed mounted throughout.
      expect(screen.queryByTestId("runs-loading")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not auto-refresh once the user pages past the newest page", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const full = Array.from({ length: RUNS_PAGE_SIZE }, (_, i) =>
        run({ id: `run-${i}`, name: `run-${i}` }),
      );
      const calls = stubFetch(() => jsonResponse(200, full));
      render(<RunsList />);
      await screen.findByTestId("runs-table");

      fireEvent.click(screen.getByTestId("runs-next"));
      await waitFor(() =>
        expect(calls[calls.length - 1]).toContain(`offset=${RUNS_PAGE_SIZE}`),
      );
      const afterPaging = calls.length;

      await vi.advanceTimersByTimeAsync(RUNS_POLL_MS * 2 + 50);
      expect(calls.length).toBe(afterPaging); // no background polls off page 1
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("RunsList — pure helpers", () => {
  it("runHref omits ?wi when the run has no work item ref", () => {
    expect(runHref(run({ workItemRef: "" }))).toBe("/runs/run-1");
    expect(runHref(run())).toContain("?wi=");
  });

  it("formatAge renders relative ages and — for missing/zero dates", () => {
    expect(formatAge(null)).toBe("—");
    expect(formatAge("0001-01-01T00:00:00Z")).toBe("—");
    expect(formatAge(new Date(Date.now() - 90_000).toISOString())).toBe("1m ago");
    expect(formatAge(new Date(Date.now() - 3 * 3_600_000).toISOString())).toBe("3h ago");
  });

  it("formatDuration renders compact durations and — for absent values", () => {
    expect(formatDuration(null)).toBe("—");
    expect(formatDuration(0)).toBe("—");
    expect(formatDuration(45)).toBe("45s");
    expect(formatDuration(300)).toBe("5m 0s");
    expect(formatDuration(3720)).toBe("1h 2m");
  });
});
