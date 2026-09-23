// lib/otelconfig.ts — the OTLP exporter config form model (story 8.12 / ADR-029).
//
// A read-write compose surface over the `OTelConfig` CRD (1.5), written via the apiserver BFF
// (never direct kube). Per-signal routing: traces/metrics/logs each carry endpoint, protocol
// (grpc|http), auth (a Secret NAME — the UI never shows, stores, or transmits a raw token),
// resource attributes, and sampling. The default state is OPT-IN: no exporter configured →
// telemetry stays in-cluster. This module is pure and unit-tested; the wire shape matches the
// CRD spec the apiserver (13.8 / ISI-2917) will serve under /api/otelconfig.

export type SignalKey = "traces" | "metrics" | "logs";

export const SIGNAL_KEYS: readonly SignalKey[] = [
  "traces",
  "metrics",
  "logs",
] as const;

export type OtlpProtocol = "grpc" | "http";

/** One signal's exporter config as the form holds it. `null` = not configured (opt-in default). */
export type SignalForm = {
  endpoint: string;
  protocol: OtlpProtocol;
  /** Secret NAME (or namespace/name) — a reference only. No token value ever enters this model. */
  authSecretRef: string;
  resourceAttributes: Record<string, string>;
  /** Sampling ratio 0–1; null = collector default (unset on the CRD). */
  sampling: number | null;
};

export type OtelConfigForm = Record<SignalKey, SignalForm | null>;

export type SignalWire = {
  endpoint?: string;
  protocol?: string;
  authSecretRef?: string;
  resourceAttributes?: Record<string, string>;
  sampling?: number;
};

export type OtelConfigWire = {
  apiVersion?: string;
  kind?: string;
  spec?: {
    traces?: SignalWire | null;
    metrics?: SignalWire | null;
    logs?: SignalWire | null;
  };
  status?: {
    signals?: Partial<
      Record<SignalKey, { state?: string; detail?: string }>
    >;
  };
};

export const OTELCONFIG_API_VERSION = "ksquad.io/v1alpha1";

/** Default state: NO exporter configured (opt-in — telemetry stays in-cluster until set). */
export function emptyConfig(): OtelConfigForm {
  return { traces: null, metrics: null, logs: null };
}

export function fromWire(wire: OtelConfigWire | null | undefined): OtelConfigForm {
  const spec = wire?.spec ?? {};
  const form = emptyConfig();
  for (const key of SIGNAL_KEYS) {
    const s = spec[key];
    if (!s || !s.endpoint) continue;
    form[key] = {
      endpoint: s.endpoint,
      protocol: s.protocol === "grpc" ? "grpc" : "http",
      authSecretRef: s.authSecretRef ?? "",
      resourceAttributes: { ...(s.resourceAttributes ?? {}) },
      sampling: typeof s.sampling === "number" ? s.sampling : null,
    };
  }
  return form;
}

/**
 * Compose the CRD-shaped body. Unconfigured signals are OMITTED from the wire entirely —
 * absence is the opt-in "no exporter for this signal" state. `authSecretRef` empty → omitted
 * (no auth), never an empty string. There is no field anywhere in the composed body that can
 * carry a token VALUE — only the Secret reference name.
 */
export function toWire(form: OtelConfigForm): OtelConfigWire {
  const spec: NonNullable<OtelConfigWire["spec"]> = {};
  for (const key of SIGNAL_KEYS) {
    const s = form[key];
    if (!s || !s.endpoint) continue;
    spec[key] = {
      endpoint: s.endpoint,
      protocol: s.protocol,
      ...(s.authSecretRef ? { authSecretRef: s.authSecretRef } : {}),
      ...(Object.keys(s.resourceAttributes).length > 0
        ? { resourceAttributes: { ...s.resourceAttributes } }
        : {}),
      ...(s.sampling !== null ? { sampling: s.sampling } : {}),
    };
  }
  return { apiVersion: OTELCONFIG_API_VERSION, kind: "OTelConfig", spec };
}

export function hasAnyExporter(form: OtelConfigForm): boolean {
  return SIGNAL_KEYS.some((k) => form[k] !== null);
}

export type SignalValidation = {
  endpoint?: string;
  protocol?: string;
  authSecretRef?: string;
  sampling?: string;
};

const HTTP_ENDPOINT = /^https?:\/\/\S+$/;
const GRPC_ENDPOINT = /^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:\d{1,5})?$/i;
const SECRET_REF = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\/[a-z0-9]([a-z0-9-]*[a-z0-9])?)?$/i;

/** Field-level validation mirroring the CRD's CEL/webhook rules (1.5) at the form edge. */
export function validateSignal(s: SignalForm): SignalValidation {
  const v: SignalValidation = {};
  const endpoint = s.endpoint.trim();
  if (!endpoint) {
    v.endpoint = "Endpoint is required";
  } else if (s.protocol === "http" && !HTTP_ENDPOINT.test(endpoint)) {
    v.endpoint = "HTTP protocol needs an http(s):// endpoint URL";
  } else if (s.protocol === "grpc" && !GRPC_ENDPOINT.test(endpoint)) {
    v.endpoint = "gRPC protocol needs a host or host:port endpoint";
  }
  const ref = s.authSecretRef.trim();
  if (ref && !SECRET_REF.test(ref)) {
    v.authSecretRef = "Expected a Secret name or namespace/name";
  }
  if (s.sampling !== null && (s.sampling < 0 || s.sampling > 1)) {
    v.sampling = "Sampling must be between 0 and 1";
  }
  return v;
}

export function validateConfig(form: OtelConfigForm): Record<SignalKey, SignalValidation> {
  const out: Record<SignalKey, SignalValidation> = {
    traces: {},
    metrics: {},
    logs: {},
  };
  for (const key of SIGNAL_KEYS) {
    const s = form[key];
    if (s) out[key] = validateSignal(s);
  }
  return out;
}

export function isValid(form: OtelConfigForm): boolean {
  const v = validateConfig(form);
  return SIGNAL_KEYS.every((k) => Object.keys(v[k]).length === 0);
}

// ── Shared-Destination model (ISI-4831 S1) ────────────────────────────────────
//
// UI-level sugar over the per-signal `OtelConfigForm`. The redesigned OTLP screen
// presents a single **Destination** (endpoint · protocol · auth secret · shared
// resource attributes · traces sampling) that all enabled signals INHERIT, plus a
// per-signal OVERRIDE only when a signal genuinely differs. The wire stays
// per-signal — this is a lossless re-shape: `toWireShared(fromWireShared(w))`
// reproduces `toWire(fromWire(w))`. No backend/CRD/schema change.

/** The shared Destination all enabled signals inherit unless they override it. */
export type Destination = {
  endpoint: string;
  protocol: OtlpProtocol;
  /** Secret NAME (or namespace/name) — a reference only, never a token value. */
  authSecretRef: string;
  /** Shared resource attributes copied onto every inheriting signal. */
  resourceAttributes: Record<string, string>;
  /** Sampling ratio 0–1; null = collector default. Applies to TRACES only. */
  sampling: number | null;
};

/**
 * A signal's slot in the shared model: disabled (opt-out, `null` on the wire),
 * enabled+inherit (serialises a copy of the shared Destination), or
 * enabled+override (serialises its own `SignalForm`).
 */
export type SignalSlot =
  | { enabled: false }
  | { enabled: true; mode: "inherit" }
  | { enabled: true; mode: "override"; override: SignalForm };

export type DestinationModel = {
  destination: Destination;
  signals: Record<SignalKey, SignalSlot>;
};

function attrsEqual(
  a: Record<string, string>,
  b: Record<string, string>,
): boolean {
  const ak = Object.keys(a);
  if (ak.length !== Object.keys(b).length) return false;
  return ak.every((k) => b[k] === a[k]);
}

/** Common value across enabled signals if they all agree, else the first non-empty. */
function commonString(vals: string[]): string {
  if (vals.length === 0) return "";
  const first = vals[0];
  if (vals.every((v) => v === first)) return first;
  return vals.find((v) => v.trim() !== "") ?? first;
}

function commonProtocol(vals: OtlpProtocol[]): OtlpProtocol {
  // protocol is never empty; "all agree" or "first" collapse to the same fallback.
  return vals.length > 0 ? vals[0] : "grpc";
}

function commonAttrs(
  vals: Record<string, string>[],
): Record<string, string> {
  if (vals.length === 0) return {};
  const first = vals[0];
  if (vals.every((v) => attrsEqual(v, first))) return { ...first };
  const nonEmpty = vals.find((v) => Object.keys(v).length > 0);
  return { ...(nonEmpty ?? first) };
}

/**
 * Derive the shared Destination from a per-signal form: the common
 * endpoint/protocol/authSecretRef/resourceAttributes across ENABLED signals
 * (fallback: first non-empty). Sampling is traces-only, so it comes from the
 * traces signal if enabled. Disabled (null) signals never contribute.
 */
export function deriveDestination(form: OtelConfigForm): Destination {
  const enabled = SIGNAL_KEYS.map((k) => form[k]).filter(
    (s): s is SignalForm => s !== null,
  );
  return {
    endpoint: commonString(enabled.map((s) => s.endpoint)),
    protocol: commonProtocol(enabled.map((s) => s.protocol)),
    authSecretRef: commonString(enabled.map((s) => s.authSecretRef)),
    resourceAttributes: commonAttrs(enabled.map((s) => s.resourceAttributes)),
    sampling: form.traces ? form.traces.sampling : null,
  };
}

/** The exact SignalForm an inheriting signal serialises (shared copy; sampling traces-only). */
function inheritedSignal(dest: Destination, key: SignalKey): SignalForm {
  return {
    endpoint: dest.endpoint,
    protocol: dest.protocol,
    authSecretRef: dest.authSecretRef,
    resourceAttributes: { ...dest.resourceAttributes },
    sampling: key === "traces" ? dest.sampling : null,
  };
}

/**
 * A signal INHERITS the Destination only when serialising the shared copy would
 * reproduce it byte-for-byte — endpoint/protocol/auth match (per the spec) AND the
 * copied resourceAttributes/sampling match too. Otherwise it OVERRIDES, so its own
 * divergent values survive the round-trip.
 */
function signalInheritsDestination(
  s: SignalForm,
  dest: Destination,
  key: SignalKey,
): boolean {
  const inh = inheritedSignal(dest, key);
  return (
    s.endpoint === inh.endpoint &&
    s.protocol === inh.protocol &&
    s.authSecretRef === inh.authSecretRef &&
    s.sampling === inh.sampling &&
    attrsEqual(s.resourceAttributes, inh.resourceAttributes)
  );
}

/** Load the per-signal form into the shared-Destination model (inherit/override derived). */
export function fromWireShared(
  wire: OtelConfigWire | null | undefined,
): DestinationModel {
  const form = fromWire(wire);
  const destination = deriveDestination(form);
  const signals = {} as Record<SignalKey, SignalSlot>;
  for (const key of SIGNAL_KEYS) {
    const s = form[key];
    if (!s) {
      signals[key] = { enabled: false };
    } else if (signalInheritsDestination(s, destination, key)) {
      signals[key] = { enabled: true, mode: "inherit" };
    } else {
      signals[key] = { enabled: true, mode: "override", override: s };
    }
  }
  return { destination, signals };
}

/** Flatten the shared-Destination model back to the per-signal form. */
export function toFormShared(model: DestinationModel): OtelConfigForm {
  const form = emptyConfig();
  for (const key of SIGNAL_KEYS) {
    const slot = model.signals[key];
    if (!slot.enabled) continue; // null = opt-out
    form[key] =
      slot.mode === "override"
        ? slot.override
        : inheritedSignal(model.destination, key);
  }
  return form;
}

/** Compose the CRD-shaped body from the shared model. Wire shape is UNCHANGED. */
export function toWireShared(model: DestinationModel): OtelConfigWire {
  return toWire(toFormShared(model));
}
