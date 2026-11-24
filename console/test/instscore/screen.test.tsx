import { describe, it, expect, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  fireEvent,
} from "@testing-library/react";
import { InstScoreScreen } from "@/components/instscore/InstScoreScreen";
import type {
  InstScoreServiceDetail,
  InstScoreServicesResponse,
} from "@/lib/instscore";

afterEach(cleanup);

// The ISI-7750 console smoke against SEEDED windows: the component boundary
// renders both read models — the services list (scores, tallies, the pending
// mid-throttle row, the missing-service.name bucket) and the drill-in detail
// (rule-by-rule verdicts with impact + failed-rule guidance, hourly history) —
// plus the honest states (denied / unconfigured / empty / error).
//
// The fixtures mirror what migration 0042's store returns after the phase-3
// tap folded a two-service deployment: one evaluated service (24/26 disclosed,
// a Critical failure with guidance) and one that just arrived (NULL score).

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function seededList(): InstScoreServicesResponse {
  return {
    rules_total: 26,
    services: [
      {
        service: "checkout",
        score: 63.86,
        category: "Needs Improvement",
        implemented: 24,
        total: 26,
        evaluated_at: "2026-11-24T13:20:00Z",
        pass: 17,
        fail: 5,
        not_evaluated: 2,
        spans: 120,
        logs: 40,
        metrics: 12,
        resources: 8,
        first_batch_at: "2026-11-23T13:20:00Z",
        last_batch_at: "2026-11-24T13:19:00Z",
      },
      {
        service: "",
        score: 0,
        category: "Poor",
        implemented: 24,
        total: 26,
        evaluated_at: "2026-11-24T13:10:00Z",
        pass: 3,
        fail: 19,
        not_evaluated: 2,
        spans: 6,
        logs: 0,
        metrics: 0,
        resources: 2,
      },
      {
        service: "just-arrived",
        implemented: 0,
        total: 0,
        pass: 0,
        fail: 0,
        not_evaluated: 0,
        spans: 3,
        logs: 0,
        metrics: 0,
        resources: 1,
        first_batch_at: "2026-11-24T13:22:00Z",
        last_batch_at: "2026-11-24T13:22:00Z",
      },
    ],
  };
}

function seededDetail(): InstScoreServiceDetail {
  return {
    ...seededList().services[0],
    verdicts: [
      {
        rule: "LOG-001",
        target: ["Log"],
        impact: "Important",
        status: "fail",
        evidence: "debug-level log records observed over 15 days",
        description:
          "Debug-level logs are not enabled in production environments for longer than 14 days.",
        guidance:
          "Log records with `severity.text` = `DEBUG` are not observed in a production environment for longer than 14 days.",
        evaluated_at: "2026-11-24T13:20:00Z",
      },
      {
        rule: "RES-005",
        target: ["Resource"],
        impact: "Critical",
        status: "pass",
        evidence: "service.name present on all resources",
        description: "`service.name` is present",
        evaluated_at: "2026-11-24T13:20:00Z",
      },
      {
        rule: "SPA-003",
        target: ["Span"],
        impact: "Important",
        status: "not_evaluated",
        evidence: "vendored criteria is an upstream TODO at pin ef080a8a",
        description: "Span names have bound cardinality.",
        guidance: "TODO",
        evaluated_at: "2026-11-24T13:20:00Z",
      },
    ],
    history: [
      { bucket_hour: "2026-11-24T11:00:00Z", score: 71.43 },
      { bucket_hour: "2026-11-24T12:00:00Z", score: 67.14 },
      { bucket_hour: "2026-11-24T13:00:00Z", score: 63.86 },
    ],
  };
}

describe("<InstScoreScreen> — ISI-7750 console smoke against seeded windows", () => {
  it("renders the services list: scores, categories, tallies, pending row, unnamed bucket", async () => {
    render(
      <InstScoreScreen load={async () => jsonResponse(200, seededList())} />,
    );

    await waitFor(() => {
      expect(screen.getByRole("table")).toBeVisible();
    });

    const checkout = screen.getByRole("button", { name: "checkout" });
    expect(checkout).toBeVisible();
    expect(screen.getByText("63.9")).toBeVisible();
    expect(screen.getByText("Needs Improvement")).toBeVisible();
    expect(screen.getByText("17 pass · 5 fail · 2 n/a")).toBeVisible();
    expect(screen.getByText("120 spans · 40 logs · 12 metrics")).toBeVisible();

    // The mid-throttle row: honest pending score, no fabricated zero.
    expect(screen.getByText("pending")).toBeVisible();
    // The missing-service.name bucket renders, non-navigable (no button).
    expect(screen.getByText("(missing service.name)")).toBeVisible();
    expect(
      screen.queryByRole("button", { name: "(missing service.name)" }),
    ).toBeNull();
  });

  it("drills into a service and renders verdicts with failed-rule guidance + history", async () => {
    render(
      <InstScoreScreen
        load={async () => jsonResponse(200, seededList())}
        loadDetail={async () => jsonResponse(200, seededDetail())}
      />,
    );

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "checkout" })).toBeVisible();
    });
    fireEvent.click(screen.getByRole("button", { name: "checkout" }));

    await waitFor(() => {
      expect(
        screen.getByText(
          "Rule-by-rule verdicts (impact order within rule-ID order)",
        ),
      ).toBeVisible();
    });

    // Disclosure + summary.
    expect(screen.getByText(/disclosed 24\/26 rules/)).toBeVisible();

    // Failed rule: evidence + the expandable guidance.
    expect(screen.getByText(/debug-level log records observed/)).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "How to fix" }));
    expect(
      screen.getByText(
        /severity\.text.*DEBUG.*not observed in a production environment/,
      ),
    ).toBeVisible();

    // Passed rule carries no guidance affordance.
    expect(screen.queryByRole("button", { name: "How to fix" })).toBeNull();

    // History sparkline is described for assistive tech.
    expect(
      screen.getByRole("img", { name: /Hourly score history/ }),
    ).toBeVisible();
  });

  it("renders the honest states: empty, denied, unconfigured, error", async () => {
    const { rerender } = render(
      <InstScoreScreen
        load={async () => jsonResponse(200, { services: [], rules_total: 26 })}
      />,
    );
    await waitFor(() => {
      expect(screen.getByText(/No services observed yet/)).toBeVisible();
    });

    rerender(
      <InstScoreScreen
        load={async () => jsonResponse(401, { error: "unauthenticated" })}
      />,
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Denied — this surface needs a console session/),
      ).toBeVisible();
    });

    rerender(
      <InstScoreScreen
        load={async () => jsonResponse(501, { error: "not implemented" })}
      />,
    );
    await waitFor(() => {
      expect(screen.getByText(/Unconfigured/)).toBeVisible();
    });

    rerender(
      <InstScoreScreen load={async () => Promise.reject(new Error("net"))} />,
    );
    await waitFor(() => {
      expect(screen.getByText(/read model is unavailable/)).toBeVisible();
    });
  });

  it("surfaces a 404 drill-in as a window-missed note", async () => {
    render(
      <InstScoreScreen
        load={async () => jsonResponse(200, seededList())}
        loadDetail={async () => jsonResponse(404, { error: "not found" })}
      />,
    );
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "checkout" })).toBeVisible();
    });
    fireEvent.click(screen.getByRole("button", { name: "checkout" }));
    await waitFor(() => {
      expect(
        screen.getByText(/no longer observed in the window/),
      ).toBeVisible();
    });
  });
});
