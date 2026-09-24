import { describe, it, expect } from "vitest";
import {
  deriveDestination,
  fromWire,
  fromWireShared,
  toWire,
  toWireShared,
  toFormShared,
  emptyConfig,
  OTELCONFIG_API_VERSION,
  type OtelConfigWire,
  type SignalForm,
  type DestinationModel,
} from "@/lib/otelconfig";

// ISI-4831 S1 — the shared-Destination model is UI sugar over the per-signal wire:
// one Destination (endpoint/protocol/auth/attrs + traces sampling) that enabled
// signals INHERIT, with per-signal OVERRIDE only when a signal differs. The wire
// shape is unchanged, so a load→save with no edits must reproduce the wire exactly.

const collector = "otel-gateway-collector.observability:4317";

function sig(over: Partial<SignalForm> = {}): SignalForm {
  return {
    endpoint: collector,
    protocol: "grpc",
    authSecretRef: "",
    resourceAttributes: {},
    sampling: null,
    ...over,
  };
}

describe("deriveDestination — common-or-first across enabled signals", () => {
  it("empty form → neutral default Destination (no enabled signal contributes)", () => {
    const d = deriveDestination(emptyConfig());
    expect(d).toEqual({
      endpoint: "",
      protocol: "grpc",
      authSecretRef: "",
      resourceAttributes: {},
      sampling: null,
    });
  });

  it("the reported case (all 3 signals point at one collector) → that shared endpoint", () => {
    const form = { traces: sig(), metrics: sig(), logs: sig() };
    const d = deriveDestination(form);
    expect(d.endpoint).toBe(collector);
    expect(d.protocol).toBe("grpc");
  });

  it("disabled (null) signals never contribute to the derived Destination", () => {
    const form = { traces: sig({ endpoint: "traces-host:4317" }), metrics: null, logs: null };
    expect(deriveDestination(form).endpoint).toBe("traces-host:4317");
  });

  it("divergent endpoints → falls back to the first non-empty", () => {
    const form = {
      traces: sig({ endpoint: "" }),
      metrics: sig({ endpoint: "metrics-host:4317" }),
      logs: sig({ endpoint: "logs-host:4317" }),
    };
    expect(deriveDestination(form).endpoint).toBe("metrics-host:4317");
  });

  it("shared resource attributes derived when all enabled signals agree", () => {
    const attrs = { "service.namespace": "squad-a", env: "prod" };
    const form = {
      traces: sig({ resourceAttributes: { ...attrs } }),
      metrics: sig({ resourceAttributes: { ...attrs } }),
      logs: null,
    };
    expect(deriveDestination(form).resourceAttributes).toEqual(attrs);
  });

  it("sampling is traces-only: comes from traces, ignored on metrics/logs", () => {
    const form = {
      traces: sig({ sampling: 0.25 }),
      metrics: sig({ sampling: 0.9 }),
      logs: null,
    };
    expect(deriveDestination(form).sampling).toBe(0.25);
  });

  it("no traces signal → destination sampling is null even if metrics carries one", () => {
    const form = { traces: null, metrics: sig({ sampling: 0.5 }), logs: sig() };
    expect(deriveDestination(form).sampling).toBeNull();
  });
});

describe("fromWireShared — inherit vs override derivation", () => {
  it("all signals match the shared collector → all inherit, none override", () => {
    const wire: OtelConfigWire = {
      spec: {
        traces: { endpoint: collector, protocol: "grpc" },
        metrics: { endpoint: collector, protocol: "grpc" },
        logs: { endpoint: collector, protocol: "grpc" },
      },
    };
    const model = fromWireShared(wire);
    expect(model.destination.endpoint).toBe(collector);
    expect(model.signals.traces).toEqual({ enabled: true, mode: "inherit" });
    expect(model.signals.metrics).toEqual({ enabled: true, mode: "inherit" });
    expect(model.signals.logs).toEqual({ enabled: true, mode: "inherit" });
  });

  it("a signal with its own endpoint is an override carrying its own values", () => {
    const wire: OtelConfigWire = {
      spec: {
        traces: { endpoint: collector, protocol: "grpc" },
        metrics: { endpoint: collector, protocol: "grpc" },
        logs: { endpoint: "logs-host:4318", protocol: "http" },
      },
    };
    const model = fromWireShared(wire);
    expect(model.signals.traces).toEqual({ enabled: true, mode: "inherit" });
    const logs = model.signals.logs;
    expect(logs.enabled).toBe(true);
    if (logs.enabled && logs.mode === "override") {
      expect(logs.override.endpoint).toBe("logs-host:4318");
      expect(logs.override.protocol).toBe("http");
    } else {
      throw new Error("logs should be an override slot");
    }
  });

  it("same endpoint but divergent auth/attrs/sampling → override (protects round-trip)", () => {
    const wire: OtelConfigWire = {
      spec: {
        // traces sets the shared shape (endpoint+auth+attrs), plus sampling
        traces: {
          endpoint: collector,
          protocol: "grpc",
          authSecretRef: "otlp-token",
          resourceAttributes: { env: "prod" },
          sampling: 0.1,
        },
        // metrics shares the endpoint but has NO auth → must not silently inherit the token
        metrics: { endpoint: collector, protocol: "grpc" },
      },
    };
    const model = fromWireShared(wire);
    expect(model.signals.traces).toEqual({ enabled: true, mode: "inherit" });
    expect(model.signals.metrics).toMatchObject({ enabled: true, mode: "override" });
  });

  it("absent signals are opt-out (disabled) slots", () => {
    const model = fromWireShared({ spec: { traces: { endpoint: collector } } });
    expect(model.signals.traces).toEqual({ enabled: true, mode: "inherit" });
    expect(model.signals.metrics).toEqual({ enabled: false });
    expect(model.signals.logs).toEqual({ enabled: false });
  });

  it("null/empty wire → all disabled", () => {
    const model = fromWireShared(null);
    expect(model.signals.traces).toEqual({ enabled: false });
    expect(model.signals.metrics).toEqual({ enabled: false });
    expect(model.signals.logs).toEqual({ enabled: false });
  });
});

describe("toWireShared — inherit copies shared, override keeps own; wire shape unchanged", () => {
  it("inherit serialises a copy of the shared Destination per enabled signal", () => {
    const model: DestinationModel = {
      destination: {
        endpoint: collector,
        protocol: "grpc",
        authSecretRef: "otlp-token",
        resourceAttributes: { env: "prod" },
        sampling: 0.2,
      },
      signals: {
        traces: { enabled: true, mode: "inherit" },
        metrics: { enabled: true, mode: "inherit" },
        logs: { enabled: false },
      },
    };
    const wire = toWireShared(model);
    expect(wire.apiVersion).toBe(OTELCONFIG_API_VERSION);
    // traces carries sampling; metrics inherits endpoint/auth/attrs but NOT sampling
    expect(wire.spec?.traces).toEqual({
      endpoint: collector,
      protocol: "grpc",
      authSecretRef: "otlp-token",
      resourceAttributes: { env: "prod" },
      sampling: 0.2,
    });
    expect(wire.spec?.metrics).toEqual({
      endpoint: collector,
      protocol: "grpc",
      authSecretRef: "otlp-token",
      resourceAttributes: { env: "prod" },
    });
    // disabled signal is omitted entirely (opt-out)
    expect(wire.spec?.logs).toBeUndefined();
  });

  it("override signal serialises its own values, ignoring the Destination", () => {
    const model: DestinationModel = {
      destination: { endpoint: collector, protocol: "grpc", authSecretRef: "", resourceAttributes: {}, sampling: null },
      signals: {
        traces: { enabled: true, mode: "inherit" },
        metrics: { enabled: false },
        logs: { enabled: true, mode: "override", override: sig({ endpoint: "logs-host:4318", protocol: "http" }) },
      },
    };
    const wire = toWireShared(model);
    expect(wire.spec?.logs).toEqual({ endpoint: "logs-host:4318", protocol: "http" });
  });

  it("toFormShared drops disabled signals to null (opt-out)", () => {
    const model: DestinationModel = {
      destination: { endpoint: collector, protocol: "grpc", authSecretRef: "", resourceAttributes: {}, sampling: null },
      signals: {
        traces: { enabled: true, mode: "inherit" },
        metrics: { enabled: false },
        logs: { enabled: false },
      },
    };
    const form = toFormShared(model);
    expect(form.traces).not.toBeNull();
    expect(form.metrics).toBeNull();
    expect(form.logs).toBeNull();
  });
});

describe("round-trip fidelity — shared model is lossless over the wire", () => {
  const cases: Record<string, OtelConfigWire> = {
    "all-inherit one collector": {
      spec: {
        traces: { endpoint: collector, protocol: "grpc", sampling: 0.1 },
        metrics: { endpoint: collector, protocol: "grpc" },
        logs: { endpoint: collector, protocol: "grpc" },
      },
    },
    "shared with one override": {
      spec: {
        traces: { endpoint: collector, protocol: "grpc", authSecretRef: "tok", resourceAttributes: { env: "prod" } },
        metrics: { endpoint: collector, protocol: "grpc", authSecretRef: "tok", resourceAttributes: { env: "prod" } },
        logs: { endpoint: "logs-host:4318", protocol: "http" },
      },
    },
    "partial opt-in (traces only)": {
      spec: { traces: { endpoint: collector, protocol: "grpc", sampling: 0.5 } },
    },
    "divergent everything": {
      spec: {
        traces: { endpoint: "t:4317", protocol: "grpc", authSecretRef: "a" },
        metrics: { endpoint: "m:4318", protocol: "http", resourceAttributes: { k: "v" } },
        logs: { endpoint: "l:4317", protocol: "grpc" },
      },
    },
    "empty / opt-out": {},
  };

  for (const [name, wire] of Object.entries(cases)) {
    it(`reproduces the normalised wire: ${name}`, () => {
      // The normalised baseline is what the existing per-signal path produces.
      const baseline = toWire(fromWire(wire));
      const throughShared = toWireShared(fromWireShared(wire));
      expect(throughShared).toEqual(baseline);
    });
  }
});
