import { describe, it, expect, vi, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  fireEvent,
} from "@testing-library/react";
import { OtlpConfigScreen } from "@/components/settings/OtlpConfigScreen";
import type { OtelConfigWire } from "@/lib/otelconfig";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const SHARED = {
  endpoint: "otel-collector:4317",
  protocol: "grpc",
  authSecretRef: "otlp-sec",
};

function loadedWire(over: Partial<OtelConfigWire> = {}): OtelConfigWire {
  return {
    apiVersion: "ksquad.io/v1alpha1",
    kind: "OTelConfig",
    spec: {
      traces: { ...SHARED },
      metrics: { ...SHARED },
      logs: { ...SHARED },
    },
    status: {
      signals: {
        traces: { state: "healthy" },
        metrics: { state: "healthy" },
        logs: { state: "erroring", detail: "connection refused" },
      },
    },
    ...over,
  };
}

/** Route GET/PUT /api/otelconfig; capture the PUT body for assertions. */
function stubOtlp(
  getResp: Response,
  onPut?: (body: string) => void,
): void {
  vi.stubGlobal(
    "fetch",
    vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "PUT") {
        onPut?.(String(init.body));
        return Promise.resolve(jsonResponse(200, {}));
      }
      return Promise.resolve(getResp);
    }),
  );
}

describe("<OtlpConfigScreen> — ISI-4831 S2 Signals list & row", () => {
  it("renders a row per signal with the inherit chip when all inherit the destination", async () => {
    stubOtlp(jsonResponse(200, loadedWire()));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    for (const key of ["traces", "metrics", "logs"]) {
      expect(screen.getByTestId(`signal-row-${key}`)).toBeTruthy();
      expect(screen.getByTestId(`signal-chip-${key}`).textContent).toBe(
        "→ destination",
      );
    }
    // Destination card carries the shared endpoint the rows inherit.
    expect(
      (screen.getByTestId("dest-endpoint") as HTMLInputElement).value,
    ).toBe("otel-collector:4317");
  });

  it("shows the 'own endpoint' chip when a signal diverges from the destination", async () => {
    const wire = loadedWire();
    wire.spec!.logs = { ...SHARED, endpoint: "logs-collector:4317" };
    stubOtlp(jsonResponse(200, wire));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    expect(screen.getByTestId("signal-chip-logs").textContent).toBe(
      "own endpoint",
    );
    expect(screen.getByTestId("signal-chip-traces").textContent).toBe(
      "→ destination",
    );
  });

  it("rolls up enabled-signal health in the Signals header", async () => {
    stubOtlp(jsonResponse(200, loadedWire()));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    expect(screen.getByTestId("signals-rollup").textContent).toBe(
      "2 healthy · 1 erroring",
    );
  });

  it("renders the live status pill + detail from CRD status", async () => {
    stubOtlp(jsonResponse(200, loadedWire()));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    expect(screen.getByTestId("signal-pill-traces").textContent).toBe("Healthy");
    expect(screen.getByTestId("signal-pill-logs").textContent).toContain(
      "Erroring",
    );
    expect(screen.getByTestId("signal-pill-logs").textContent).toContain(
      "connection refused",
    );
  });

  it("toggling a signal off removes it from the composed wire on Apply", async () => {
    let putBody = "";
    stubOtlp(jsonResponse(200, loadedWire()), (b) => (putBody = b));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    fireEvent.click(screen.getByTestId("signal-toggle-metrics"));
    fireEvent.click(screen.getByTestId("otlp-apply"));

    await waitFor(() => expect(putBody).not.toBe(""));
    const wire = JSON.parse(putBody) as OtelConfigWire;
    expect(wire.spec?.metrics).toBeUndefined();
    expect(wire.spec?.traces?.endpoint).toBe("otel-collector:4317");
    expect(wire.spec?.logs?.endpoint).toBe("otel-collector:4317");
  });

  it("expanding a signal and overriding surfaces its own endpoint fields in the wire", async () => {
    let putBody = "";
    stubOtlp(jsonResponse(200, loadedWire()), (b) => (putBody = b));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    fireEvent.click(screen.getByTestId("signal-expand-logs"));
    fireEvent.click(screen.getByTestId("signal-override-logs"));
    fireEvent.change(screen.getByTestId("logs-endpoint"), {
      target: { value: "logs-only:4317" },
    });
    fireEvent.click(screen.getByTestId("otlp-apply"));

    await waitFor(() => expect(putBody).not.toBe(""));
    const wire = JSON.parse(putBody) as OtelConfigWire;
    expect(wire.spec?.logs?.endpoint).toBe("logs-only:4317");
    expect(wire.spec?.traces?.endpoint).toBe("otel-collector:4317");
  });

  it("protocol is a segmented control that reflects the active protocol", async () => {
    stubOtlp(jsonResponse(200, loadedWire()));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    const grpc = screen.getByTestId("dest-proto-grpc");
    const http = screen.getByTestId("dest-proto-http");
    expect(grpc.getAttribute("aria-checked")).toBe("true");
    expect(http.getAttribute("aria-checked")).toBe("false");

    fireEvent.click(http);
    expect(screen.getByTestId("dest-proto-http").getAttribute("aria-checked")).toBe(
      "true",
    );
    expect(screen.getByTestId("dest-proto-grpc").getAttribute("aria-checked")).toBe(
      "false",
    );
  });

  it("validation is inline and only after edit (P4)", async () => {
    stubOtlp(jsonResponse(200, loadedWire()));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    const endpoint = screen.getByTestId("dest-endpoint") as HTMLInputElement;
    fireEvent.change(endpoint, { target: { value: "" } });
    // Not blurred yet → no error shown.
    expect(screen.queryByText("Endpoint is required")).toBeNull();

    fireEvent.blur(endpoint);
    await waitFor(() =>
      expect(screen.getByText("Endpoint is required")).toBeTruthy(),
    );
  });

  it("shows the empty state when no signal is enabled (opt-in default)", async () => {
    stubOtlp(jsonResponse(404, {}));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    expect(screen.getByTestId("signals-empty")).toBeTruthy();
    // No chip renders for a disabled signal.
    expect(screen.queryByTestId("signal-chip-traces")).toBeNull();
    // All rows render but show 'Off'.
    expect(screen.getByTestId("signal-pill-traces").textContent).toBe("Off");
  });
});

describe("<OtlpConfigScreen> — ISI-4831 S3 Right rail + action bar", () => {
  it("rolls up export health verdict + N of 3 exporting from CRD status", async () => {
    stubOtlp(jsonResponse(200, loadedWire()));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("export-health"));

    // 2 healthy + 1 erroring → Degraded, 2 of 3 exporting.
    expect(screen.getByTestId("export-health-verdict").textContent).toBe(
      "Degraded",
    );
    expect(screen.getByTestId("export-health-count").textContent).toBe(
      "2 of 3 exporting",
    );
  });

  it("reads 'Healthy' when every enabled signal is healthy", async () => {
    const wire = loadedWire();
    wire.status!.signals!.logs = { state: "healthy" };
    stubOtlp(jsonResponse(200, wire));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("export-health"));

    expect(screen.getByTestId("export-health-verdict").textContent).toBe(
      "Healthy",
    );
    expect(screen.getByTestId("export-health-count").textContent).toBe(
      "3 of 3 exporting",
    );
  });

  it("hides the sparkline when status carries no throughput (degrade-don't-blank)", async () => {
    stubOtlp(jsonResponse(200, loadedWire()));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("export-health"));

    expect(screen.queryByTestId("export-health-sparkline")).toBeNull();
    expect(screen.getByTestId("export-health-no-spark")).toBeTruthy();
  });

  it("renders the spans/min sparkline when throughput is present", async () => {
    const wire = loadedWire();
    wire.status!.signals!.traces = {
      state: "healthy",
      throughput: [10, 40, 25, 60, 55],
    };
    stubOtlp(jsonResponse(200, wire));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("export-health"));

    expect(screen.getByTestId("export-health-sparkline")).toBeTruthy();
    expect(screen.queryByTestId("export-health-no-spark")).toBeNull();
  });

  it("edits shared resource attributes and serialises them onto every inheriting signal", async () => {
    let putBody = "";
    const wire = loadedWire();
    wire.spec!.traces = { ...SHARED, resourceAttributes: { env: "prod" } };
    wire.spec!.metrics = { ...SHARED, resourceAttributes: { env: "prod" } };
    wire.spec!.logs = { ...SHARED, resourceAttributes: { env: "prod" } };
    stubOtlp(jsonResponse(200, wire), (b) => (putBody = b));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("resource-attributes"));

    // Seeded from the shared destination attributes.
    expect((screen.getByTestId("attr-key-0") as HTMLInputElement).value).toBe(
      "env",
    );

    // Add a second shared attribute.
    fireEvent.click(screen.getByTestId("attr-add"));
    fireEvent.change(screen.getByTestId("attr-key-1"), {
      target: { value: "region" },
    });
    fireEvent.change(screen.getByTestId("attr-value-1"), {
      target: { value: "eu" },
    });
    fireEvent.click(screen.getByTestId("otlp-apply"));

    await waitFor(() => expect(putBody).not.toBe(""));
    const put = JSON.parse(putBody) as OtelConfigWire;
    for (const key of ["traces", "metrics", "logs"] as const) {
      expect(put.spec?.[key]?.resourceAttributes).toEqual({
        env: "prod",
        region: "eu",
      });
    }
  });

  it("removing a shared attribute row drops it from the wire", async () => {
    let putBody = "";
    const wire = loadedWire();
    wire.spec!.traces = { ...SHARED, resourceAttributes: { env: "prod" } };
    wire.spec!.metrics = { ...SHARED, resourceAttributes: { env: "prod" } };
    wire.spec!.logs = { ...SHARED, resourceAttributes: { env: "prod" } };
    stubOtlp(jsonResponse(200, wire), (b) => (putBody = b));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("resource-attributes"));

    fireEvent.click(screen.getByTestId("attr-remove-0"));
    fireEvent.click(screen.getByTestId("otlp-apply"));

    await waitFor(() => expect(putBody).not.toBe(""));
    const put = JSON.parse(putBody) as OtelConfigWire;
    expect(put.spec?.traces?.resourceAttributes).toBeUndefined();
  });

  it("shows the unsaved-changes indicator and Discard reverts every edit", async () => {
    stubOtlp(jsonResponse(200, loadedWire()));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("otlp-signals"));

    // Clean: no dirty indicator, Discard + Apply disabled.
    expect(screen.queryByTestId("otlp-dirty")).toBeNull();
    expect((screen.getByTestId("otlp-discard") as HTMLButtonElement).disabled).toBe(
      true,
    );

    // Edit the endpoint → dirty.
    fireEvent.change(screen.getByTestId("dest-endpoint"), {
      target: { value: "changed:4317" },
    });
    expect(screen.getByTestId("otlp-dirty")).toBeTruthy();
    expect((screen.getByTestId("otlp-discard") as HTMLButtonElement).disabled).toBe(
      false,
    );

    // Discard → back to the loaded endpoint, dirty cleared.
    fireEvent.click(screen.getByTestId("otlp-discard"));
    expect((screen.getByTestId("dest-endpoint") as HTMLInputElement).value).toBe(
      "otel-collector:4317",
    );
    expect(screen.queryByTestId("otlp-dirty")).toBeNull();
  });

  it("Discard re-seeds the resource-attribute rows from the loaded config", async () => {
    const wire = loadedWire();
    wire.spec!.traces = { ...SHARED, resourceAttributes: { env: "prod" } };
    wire.spec!.metrics = { ...SHARED, resourceAttributes: { env: "prod" } };
    wire.spec!.logs = { ...SHARED, resourceAttributes: { env: "prod" } };
    stubOtlp(jsonResponse(200, wire));
    render(<OtlpConfigScreen />);
    await waitFor(() => screen.getByTestId("resource-attributes"));

    fireEvent.click(screen.getByTestId("attr-add"));
    expect(screen.getByTestId("attr-row-1")).toBeTruthy();

    fireEvent.click(screen.getByTestId("otlp-discard"));
    // The added blank row is gone; only the loaded one remains.
    expect(screen.queryByTestId("attr-row-1")).toBeNull();
    expect((screen.getByTestId("attr-key-0") as HTMLInputElement).value).toBe(
      "env",
    );
  });
});
