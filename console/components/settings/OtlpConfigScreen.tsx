"use client";

// components/settings/OtlpConfigScreen.tsx — Settings → Configuration: the OTLP exporter
// config surface (story 8.12 / ADR-029 / UX screen 12; redesign ISI-4831).
//
// A read-write COMPOSE surface over the `OTelConfig` CRD (1.5): per-signal (traces/metrics/logs)
// exporter routing — endpoint, protocol (grpc|http), auth as a Secret NAME (a reference — the UI
// never shows, stores, or transmits a token value), resource attributes, sampling. Default state
// is OPT-IN: with nothing configured here the app emits no OTLP of its OWN — this screen governs
// app-level routing only, NOT platform telemetry, which the cluster's infrastructure OTel
// Collector may already export. Saves go through the BFF (app/api/otelconfig → apiserver).
//
// ISI-4831 S2 (Signals list & row): the three redundant per-signal forms collapse into a single
// shared **Destination** (S1 `lib/otelconfig` shared model) that every enabled signal INHERITS,
// plus compact **signal rows** (toggle · icon · name · inherit/override chip · live status pill ·
// expand). A signal opts into its **own endpoint** only when it genuinely differs; Remove lives in
// the expanded panel (no standing danger button). Wire shape is unchanged — this is pure form
// sugar over the per-signal CRD. Right-rail health/attrs + sticky action bar land in S3.

import { useEffect, useMemo, useState } from "react";
import {
  SIGNAL_KEYS,
  deriveDestination,
  emptyConfig,
  fromWireShared,
  isValid,
  toFormShared,
  toWireShared,
  validateConfig,
  validateSignal,
  type Destination,
  type OtelConfigForm,
  type OtelConfigWire,
  type OtlpProtocol,
  type SignalForm,
  type SignalKey,
  type SignalSlot,
} from "@/lib/otelconfig";

type LoadState =
  | { kind: "loading" }
  | { kind: "empty" } // 404 / absent — the opt-in default
  | { kind: "loaded"; wire: OtelConfigWire }
  | { kind: "error"; status: number };

const SIGNAL_LABEL: Record<SignalKey, string> = {
  traces: "Traces",
  metrics: "Metrics",
  logs: "Logs",
};

/** Compact per-signal glyph. Inherits currentColor so it tints with the row state. */
function SignalIcon({ signalKey }: { signalKey: SignalKey }) {
  const paths: Record<SignalKey, JSX.Element> = {
    // traces — a span waterfall
    traces: (
      <>
        <rect x="2" y="3" width="9" height="3" rx="1.5" />
        <rect x="5" y="8.5" width="9" height="3" rx="1.5" />
        <rect x="8" y="14" width="6" height="3" rx="1.5" />
      </>
    ),
    // metrics — a small bar chart
    metrics: (
      <>
        <rect x="2.5" y="10" width="3" height="7" rx="1" />
        <rect x="8.5" y="6" width="3" height="11" rx="1" />
        <rect x="14.5" y="2.5" width="3" height="14.5" rx="1" />
      </>
    ),
    // logs — stacked lines
    logs: (
      <>
        <rect x="2.5" y="4" width="15" height="2" rx="1" />
        <rect x="2.5" y="9" width="15" height="2" rx="1" />
        <rect x="2.5" y="14" width="10" height="2" rx="1" />
      </>
    ),
  };
  return (
    <svg
      className="signal-row__icon"
      viewBox="0 0 20 20"
      width="18"
      height="18"
      fill="currentColor"
      aria-hidden="true"
    >
      {paths[signalKey]}
    </svg>
  );
}

type StatusTone = "running" | "blocked" | "paused" | "idle";

/** Map CRD status.state → a pill label + tone. Degrade-don't-blank: unknown → Pending/idle. */
function statusView(
  enabled: boolean,
  state: string | undefined,
): { label: string; tone: StatusTone } {
  if (!enabled) return { label: "Off", tone: "idle" };
  switch (state) {
    case "healthy":
      return { label: "Healthy", tone: "running" };
    case "erroring":
      return { label: "Erroring", tone: "blocked" };
    case "degraded":
      return { label: "Degraded", tone: "paused" };
    default:
      return { label: state ? state : "Pending", tone: "idle" };
  }
}

/** Human rollup of enabled-signal health for the Signals card header. */
function healthRollup(
  wire: OtelConfigWire | null,
  signals: Record<SignalKey, SignalSlot>,
): string {
  let healthy = 0;
  let erroring = 0;
  let degraded = 0;
  let pending = 0;
  let enabled = 0;
  for (const key of SIGNAL_KEYS) {
    if (!signals[key].enabled) continue;
    enabled += 1;
    const state = wire?.status?.signals?.[key]?.state;
    if (state === "healthy") healthy += 1;
    else if (state === "erroring") erroring += 1;
    else if (state === "degraded") degraded += 1;
    else pending += 1;
  }
  if (enabled === 0) return "No signals enabled";
  const parts: string[] = [];
  if (healthy) parts.push(`${healthy} healthy`);
  if (erroring) parts.push(`${erroring} erroring`);
  if (degraded) parts.push(`${degraded} degraded`);
  if (pending) parts.push(`${pending} pending`);
  return parts.join(" · ");
}

/** The SignalForm an inheriting signal serialises (shared copy; sampling traces-only). */
function inheritedCopy(dest: Destination, key: SignalKey): SignalForm {
  return {
    endpoint: dest.endpoint,
    protocol: dest.protocol,
    authSecretRef: dest.authSecretRef,
    resourceAttributes: { ...dest.resourceAttributes },
    sampling: key === "traces" ? dest.sampling : null,
  };
}

function Segmented({
  value,
  onChange,
  idBase,
}: {
  value: OtlpProtocol;
  onChange: (p: OtlpProtocol) => void;
  idBase: string;
}) {
  const opts: { p: OtlpProtocol; label: string }[] = [
    { p: "grpc", label: "gRPC" },
    { p: "http", label: "HTTP" },
  ];
  return (
    <div className="segmented" role="radiogroup" aria-label="Protocol">
      {opts.map(({ p, label }) => (
        <button
          key={p}
          type="button"
          role="radio"
          aria-checked={value === p}
          data-testid={`${idBase}-proto-${p}`}
          className={"segmented__opt" + (value === p ? " is-active" : "")}
          onClick={() => onChange(p)}
        >
          {label}
        </button>
      ))}
    </div>
  );
}

export function OtlpConfigScreen() {
  const [load, setLoad] = useState<LoadState>({ kind: "loading" });
  const [destination, setDestination] = useState<Destination>(
    () => deriveDestination(emptyConfig()),
  );
  const [signals, setSignals] = useState<Record<SignalKey, SignalSlot>>(() => ({
    traces: { enabled: false },
    metrics: { enabled: false },
    logs: { enabled: false },
  }));
  const [dirty, setDirty] = useState(false);
  const [expanded, setExpanded] = useState<SignalKey | null>(null);
  // Fields validate only AFTER they're touched (P4: inline, post-edit only).
  const [touched, setTouched] = useState<Set<string>>(new Set());
  const [saveState, setSaveState] = useState<
    | { kind: "idle" }
    | { kind: "saving" }
    | { kind: "ok" }
    | { kind: "error"; status: number; body: string }
  >({ kind: "idle" });

  useEffect(() => {
    let alive = true;
    fetch("/api/otelconfig", { cache: "no-store" })
      .then(async (r) => {
        if (r.status === 404) return { kind: "empty" as const };
        if (!r.ok) return { kind: "error" as const, status: r.status };
        return { kind: "loaded" as const, wire: (await r.json()) as OtelConfigWire };
      })
      .then((s) => {
        if (!alive) return;
        setLoad(s);
        if (s.kind === "loaded") {
          const model = fromWireShared(s.wire);
          setDestination(model.destination);
          setSignals(model.signals);
        }
      })
      .catch(() => alive && setLoad({ kind: "error", status: 0 }));
    return () => {
      alive = false;
    };
  }, []);

  const wire = load.kind === "loaded" ? load.wire : null;

  const form: OtelConfigForm = useMemo(
    () => toFormShared({ destination, signals }),
    [destination, signals],
  );
  const validation = useMemo(() => validateConfig(form), [form]);
  const valid = isValid(form);
  const anyEnabled = SIGNAL_KEYS.some((k) => signals[k].enabled);

  // Any enabled+inherit signal serialises the shared Destination, so the Destination
  // fields must be valid when at least one signal inherits.
  const anyInherit = SIGNAL_KEYS.some(
    (k) => signals[k].enabled && signals[k].mode === "inherit",
  );
  const destErrors = useMemo(
    () =>
      anyInherit
        ? validateSignal({
            endpoint: destination.endpoint,
            protocol: destination.protocol,
            authSecretRef: destination.authSecretRef,
            resourceAttributes: destination.resourceAttributes,
            sampling: destination.sampling,
          })
        : {},
    [anyInherit, destination],
  );

  function markDirty() {
    setDirty(true);
    setSaveState({ kind: "idle" });
  }
  function touch(field: string) {
    setTouched((t) => (t.has(field) ? t : new Set(t).add(field)));
  }
  function show(field: string, err: string | undefined): string | undefined {
    return touched.has(field) ? err : undefined;
  }

  function patchDestination(p: Partial<Destination>) {
    setDestination((d) => ({ ...d, ...p }));
    markDirty();
  }
  function setSlot(key: SignalKey, slot: SignalSlot) {
    setSignals((s) => ({ ...s, [key]: slot }));
    markDirty();
  }
  function toggleSignal(key: SignalKey) {
    const cur = signals[key];
    if (cur.enabled) {
      setSlot(key, { enabled: false });
      if (expanded === key) setExpanded(null);
    } else {
      setSlot(key, { enabled: true, mode: "inherit" });
    }
  }
  function setOwnEndpoint(key: SignalKey, own: boolean) {
    if (own) {
      setSlot(key, {
        enabled: true,
        mode: "override",
        override: inheritedCopy(destination, key),
      });
    } else {
      setSlot(key, { enabled: true, mode: "inherit" });
    }
  }
  function patchOverride(key: SignalKey, p: Partial<SignalForm>) {
    const cur = signals[key];
    if (!cur.enabled || cur.mode !== "override") return;
    setSlot(key, { ...cur, override: { ...cur.override, ...p } });
  }

  async function save() {
    setSaveState({ kind: "saving" });
    const res = await fetch("/api/otelconfig", {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(toWireShared({ destination, signals })),
    });
    if (res.ok) {
      setDirty(false);
      setSaveState({ kind: "ok" });
    } else {
      setSaveState({ kind: "error", status: res.status, body: await res.text() });
    }
  }

  return (
    <div className="settings-otlp">
      <h1>Settings · Configuration</h1>
      <p className="muted">
        Application-level OTLP routing — where <em>this app</em> sends its own
        traces, metrics, and logs. Opt-in: with nothing configured here, the app
        emits no OTLP of its own. This screen does not govern platform telemetry,
        which your cluster&apos;s infrastructure OTel Collector may already be
        exporting. Auth is a Secret reference; token values are never shown or
        stored here.
      </p>

      {load.kind === "loading" && <p className="muted">Loading…</p>}
      {load.kind === "error" && (
        <p className="state state--error" role="alert" data-testid="otlp-load-error">
          Could not read exporter config (status {load.status || "network"}).
        </p>
      )}

      {load.kind !== "loading" && load.kind !== "error" && (
        <>
          {/* Destination — the shared endpoint every enabled signal inherits. */}
          <div className="card otlp-destination" data-testid="otlp-destination">
            <h2>Destination</h2>
            <p className="muted">
              The collector every enabled signal exports to, unless a signal
              overrides it below.
            </p>
            <div className="signal__grid">
              <label>
                <span>Endpoint</span>
                <input
                  data-testid="dest-endpoint"
                  value={destination.endpoint}
                  onChange={(e) => patchDestination({ endpoint: e.target.value })}
                  onBlur={() => touch("dest-endpoint")}
                  placeholder={
                    destination.protocol === "http"
                      ? "https://backend:4318"
                      : "otel-collector:4317"
                  }
                  aria-invalid={!!show("dest-endpoint", destErrors.endpoint)}
                />
                {show("dest-endpoint", destErrors.endpoint) && (
                  <em className="field-error">{destErrors.endpoint}</em>
                )}
              </label>
              <label>
                <span>Protocol</span>
                <Segmented
                  idBase="dest"
                  value={destination.protocol}
                  onChange={(p) => patchDestination({ protocol: p })}
                />
              </label>
              <label>
                <span>Auth Secret ref</span>
                <input
                  data-testid="dest-auth"
                  value={destination.authSecretRef}
                  onChange={(e) =>
                    patchDestination({ authSecretRef: e.target.value })
                  }
                  onBlur={() => touch("dest-auth")}
                  placeholder="my-exporter-secret (name only — never a token)"
                  aria-invalid={!!show("dest-auth", destErrors.authSecretRef)}
                />
                {show("dest-auth", destErrors.authSecretRef) && (
                  <em className="field-error">{destErrors.authSecretRef}</em>
                )}
              </label>
              <label>
                <span>Sampling (0–1, traces)</span>
                <input
                  data-testid="dest-sampling"
                  type="number"
                  min={0}
                  max={1}
                  step={0.05}
                  value={destination.sampling ?? ""}
                  onChange={(e) =>
                    patchDestination({
                      sampling:
                        e.target.value === "" ? null : Number(e.target.value),
                    })
                  }
                  onBlur={() => touch("dest-sampling")}
                  aria-invalid={!!show("dest-sampling", destErrors.sampling)}
                />
                {show("dest-sampling", destErrors.sampling) && (
                  <em className="field-error">{destErrors.sampling}</em>
                )}
              </label>
            </div>
          </div>

          {/* Signals — compact rows inheriting the Destination unless overridden. */}
          <div className="card otlp-signals" data-testid="otlp-signals">
            <div className="otlp-signals__head">
              <h2>Signals</h2>
              <span className="otlp-signals__rollup" data-testid="signals-rollup">
                {healthRollup(wire, signals)}
              </span>
            </div>
            <ul className="signal-rows" role="list">
              {SIGNAL_KEYS.map((key) => {
                const slot = signals[key];
                const enabled = slot.enabled;
                const isOverride = enabled && slot.mode === "override";
                const status = statusView(
                  enabled,
                  wire?.status?.signals?.[key]?.state,
                );
                const detail = enabled
                  ? wire?.status?.signals?.[key]?.detail
                  : undefined;
                const isOpen = expanded === key;
                return (
                  <li
                    key={key}
                    className={"signal-row" + (enabled ? "" : " is-off")}
                    data-testid={`signal-row-${key}`}
                    data-tone={status.tone}
                  >
                    <div className="signal-row__main">
                      <button
                        type="button"
                        role="switch"
                        aria-checked={enabled}
                        aria-label={`Enable ${SIGNAL_LABEL[key]} exporter`}
                        data-testid={`signal-toggle-${key}`}
                        className={"toggle" + (enabled ? " is-on" : "")}
                        onClick={() => toggleSignal(key)}
                      >
                        <span className="toggle__knob" />
                      </button>
                      <SignalIcon signalKey={key} />
                      <span className="signal-row__name">{SIGNAL_LABEL[key]}</span>
                      {enabled && (
                        <span
                          className={
                            "chip signal-row__chip" +
                            (isOverride ? " chip--override" : " chip--inherit")
                          }
                          data-testid={`signal-chip-${key}`}
                        >
                          {isOverride ? "own endpoint" : "→ destination"}
                        </span>
                      )}
                      <span
                        className={`pill pill--${status.tone} signal-row__pill`}
                        data-testid={`signal-pill-${key}`}
                        title={detail || undefined}
                      >
                        {status.label}
                        {detail ? ` · ${detail}` : ""}
                      </span>
                      <button
                        type="button"
                        className={"chevron" + (isOpen ? " is-open" : "")}
                        aria-expanded={isOpen}
                        aria-label={`${isOpen ? "Collapse" : "Expand"} ${SIGNAL_LABEL[key]}`}
                        data-testid={`signal-expand-${key}`}
                        onClick={() => setExpanded(isOpen ? null : key)}
                      >
                        <svg viewBox="0 0 20 20" width="16" height="16" aria-hidden="true">
                          <path
                            d="M6 8l4 4 4-4"
                            fill="none"
                            stroke="currentColor"
                            strokeWidth="2"
                            strokeLinecap="round"
                            strokeLinejoin="round"
                          />
                        </svg>
                      </button>
                    </div>

                    {isOpen && (
                      <div
                        className="signal-row__panel"
                        data-testid={`signal-panel-${key}`}
                      >
                        {!enabled ? (
                          <p className="muted">
                            {SIGNAL_LABEL[key]} export is off. Enable it with the
                            toggle to route this signal to the destination.
                          </p>
                        ) : (
                          <SignalOverridePanel
                            signalKey={key}
                            slot={slot}
                            errors={validation[key]}
                            touched={touched}
                            onTouch={touch}
                            onUseOwn={(own) => setOwnEndpoint(key, own)}
                            onPatch={(p) => patchOverride(key, p)}
                            onRemove={() => toggleSignal(key)}
                          />
                        )}
                      </div>
                    )}
                  </li>
                );
              })}
            </ul>
            {!anyEnabled && (
              <p className="muted" data-testid="signals-empty">
                No app-level OTLP exporter enabled. Platform telemetry may still be
                exported by the cluster&apos;s infrastructure OTel Collector — that
                layer is managed outside this screen.
              </p>
            )}
          </div>

          <div className="settings-otlp__actions">
            <button
              type="button"
              className="btn btn--primary"
              onClick={save}
              disabled={!dirty || !valid || saveState.kind === "saving"}
              data-testid="otlp-apply"
            >
              {saveState.kind === "saving" ? "Saving…" : "Apply OTLP configuration"}
            </button>
            {saveState.kind === "error" && (
              <span className="state state--error" role="alert">
                Save failed (status {saveState.status}).
              </span>
            )}
            {saveState.kind === "ok" && <span className="state state--ok">Saved.</span>}
          </div>
        </>
      )}
    </div>
  );
}

function SignalOverridePanel({
  signalKey,
  slot,
  errors,
  touched,
  onTouch,
  onUseOwn,
  onPatch,
  onRemove,
}: {
  signalKey: SignalKey;
  slot: Extract<SignalSlot, { enabled: true }>;
  errors: Record<string, string | undefined>;
  touched: Set<string>;
  onTouch: (field: string) => void;
  onUseOwn: (own: boolean) => void;
  onPatch: (p: Partial<SignalForm>) => void;
  onRemove: () => void;
}) {
  const isOverride = slot.mode === "override";
  const ov = isOverride ? slot.override : null;
  const f = (name: string) => `${signalKey}-${name}`;
  const show = (name: string, err: string | undefined) =>
    touched.has(f(name)) ? err : undefined;

  const [attrsText, setAttrsText] = useState(
    ov
      ? Object.entries(ov.resourceAttributes)
          .map(([k, v]) => `${k}=${v}`)
          .join("\n")
      : "",
  );

  function parseAttrs(text: string): Record<string, string> {
    const out: Record<string, string> = {};
    for (const line of text.split("\n")) {
      const t = line.trim();
      if (!t) continue;
      const i = t.indexOf("=");
      if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
    }
    return out;
  }

  return (
    <div className="signal-override">
      <label className="signal-override__own">
        <input
          type="checkbox"
          data-testid={`signal-override-${signalKey}`}
          checked={isOverride}
          onChange={(e) => onUseOwn(e.target.checked)}
        />
        <span>Use its own endpoint (override the destination)</span>
      </label>

      {ov ? (
        <div className="signal__grid">
          <label>
            <span>Endpoint</span>
            <input
              data-testid={`${signalKey}-endpoint`}
              value={ov.endpoint}
              onChange={(e) => onPatch({ endpoint: e.target.value })}
              onBlur={() => onTouch(f("endpoint"))}
              placeholder={
                ov.protocol === "http"
                  ? "https://backend:4318"
                  : "otel-collector:4317"
              }
              aria-invalid={!!show("endpoint", errors.endpoint)}
            />
            {show("endpoint", errors.endpoint) && (
              <em className="field-error">{errors.endpoint}</em>
            )}
          </label>
          <label>
            <span>Protocol</span>
            <Segmented
              idBase={signalKey}
              value={ov.protocol}
              onChange={(p) => onPatch({ protocol: p })}
            />
          </label>
          <label>
            <span>Auth Secret ref</span>
            <input
              data-testid={`${signalKey}-auth`}
              value={ov.authSecretRef}
              onChange={(e) => onPatch({ authSecretRef: e.target.value })}
              onBlur={() => onTouch(f("auth"))}
              placeholder="my-exporter-secret (name only — never a token)"
              aria-invalid={!!show("auth", errors.authSecretRef)}
            />
            {show("auth", errors.authSecretRef) && (
              <em className="field-error">{errors.authSecretRef}</em>
            )}
          </label>
          {signalKey === "traces" && (
            <label>
              <span>Sampling (0–1)</span>
              <input
                data-testid={`${signalKey}-sampling`}
                type="number"
                min={0}
                max={1}
                step={0.05}
                value={ov.sampling ?? ""}
                onChange={(e) =>
                  onPatch({
                    sampling: e.target.value === "" ? null : Number(e.target.value),
                  })
                }
                onBlur={() => onTouch(f("sampling"))}
                aria-invalid={!!show("sampling", errors.sampling)}
              />
              {show("sampling", errors.sampling) && (
                <em className="field-error">{errors.sampling}</em>
              )}
            </label>
          )}
          <label className="signal__attrs">
            <span>Resource attributes (one key=value per line)</span>
            <textarea
              data-testid={`${signalKey}-attrs`}
              rows={3}
              value={attrsText}
              onChange={(e) => {
                setAttrsText(e.target.value);
                onPatch({ resourceAttributes: parseAttrs(e.target.value) });
              }}
              placeholder={"deployment=production\nregion=eu"}
            />
          </label>
        </div>
      ) : (
        <p className="muted signal-override__inherits">
          Inheriting the shared destination. Enable the override above to give{" "}
          {SIGNAL_LABEL[signalKey]} its own endpoint.
        </p>
      )}

      <button
        type="button"
        className="btn btn--danger signal-override__remove"
        data-testid={`signal-remove-${signalKey}`}
        onClick={onRemove}
      >
        Remove {SIGNAL_LABEL[signalKey].toLowerCase()} exporter
      </button>
    </div>
  );
}
