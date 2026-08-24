"use client";

// components/settings/OtlpConfigScreen.tsx — Settings → Configuration: the OTLP exporter
// config surface (story 8.12 / ADR-029 / UX screen 12).
//
// A read-write COMPOSE surface over the `OTelConfig` CRD (1.5), like Compose (8.5): per-signal
// (traces/metrics/logs) exporter routing — endpoint, protocol (grpc|http), auth as a Secret
// NAME (a reference — the UI never shows, stores, or transmits a token value), resource
// attributes, sampling. Default state is OPT-IN: no exporter configured → telemetry stays
// in-cluster. Saves go through the BFF (app/api/otelconfig → apiserver), which composes and
// applies the CRD via the reconciler (13.8); export state (healthy/erroring per signal)
// renders from the CRD status when present.

import { useEffect, useMemo, useState } from "react";
import {
  SIGNAL_KEYS,
  emptyConfig,
  fromWire,
  hasAnyExporter,
  isValid,
  toWire,
  validateConfig,
  type OtelConfigForm,
  type OtelConfigWire,
  type SignalForm,
  type SignalKey,
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

function newSignal(): SignalForm {
  return {
    endpoint: "",
    protocol: "http",
    authSecretRef: "",
    resourceAttributes: {},
    sampling: null,
  }
}

export function OtlpConfigScreen() {
  const [load, setLoad] = useState<LoadState>({ kind: "loading" });
  const [form, setForm] = useState<OtelConfigForm>(emptyConfig());
  const [dirty, setDirty] = useState(false);
  const [saveState, setSaveState] = useState<
    { kind: "idle" } | { kind: "saving" } | { kind: "ok" } | { kind: "error"; status: number; body: string }
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
        if (s.kind === "loaded") setForm(fromWire(s.wire));
      })
      .catch(() => alive && setLoad({ kind: "error", status: 0 }));
    return () => {
      alive = false;
    };
  }, []);

  const validation = useMemo(() => validateConfig(form), [form]);
  const valid = isValid(form);

  function setSignal(key: SignalKey, next: SignalForm | null) {
    setForm((f) => ({ ...f, [key]: next }));
    setDirty(true);
    setSaveState({ kind: "idle" });
  }

  async function save() {
    setSaveState({ kind: "saving" });
    const res = await fetch("/api/otelconfig", {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(toWire(form)),
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
        OTLP exporter routing — where traces, metrics, and logs are sent. Opt-in:
        with no exporter configured, telemetry stays in-cluster. Auth is a
        Secret reference; token values are never shown or stored here.
      </p>

      <div className="card">
        <h2>Export state</h2>
        {load.kind === "loading" && <p className="muted">Loading…</p>}
        {load.kind === "error" && (
          <p className="state state--error" role="alert">
            Could not read exporter config (status {load.status || "network"}).
          </p>
        )}
        {(load.kind === "empty" || (!dirty && !hasAnyExporter(form))) && (
          <p className="muted">No exporter configured — telemetry stays in-cluster.</p>
        )}
        {load.kind === "loaded" &&
          SIGNAL_KEYS.map((key) => {
            const st = load.wire.status?.signals?.[key];
            if (!form[key] || !st) return null;
            const healthy = st.state === "healthy";
            return (
              <p key={key} className="state" data-state={st.state}>
                <strong>{SIGNAL_LABEL[key]}</strong>: {form[key]!.endpoint} —{" "}
                {healthy ? "healthy" : `erroring${st.detail ? ` (${st.detail})` : ""}`}
              </p>
            );
          })}
      </div>

      {SIGNAL_KEYS.map((key) => (
        <SignalCard
          key={key}
          signalKey={key}
          value={form[key]}
          errors={validation[key]}
          onChange={(v) => setSignal(key, v)}
        />
      ))}

      <div className="settings-otlp__actions">
        <button
          type="button"
          className="btn btn--primary"
          onClick={save}
          disabled={!dirty || !valid || saveState.kind === "saving"}
        >
          {saveState.kind === "saving" ? "Saving…" : "Apply OTLP configuration"}
        </button>
        {saveState.kind === "error" && (
          <span className="state state--error" role="alert">
            Save failed (status {saveState.status}).
          </span>
        )}
        {saveState.kind === "ok" && (
          <span className="state state--ok">Saved.</span>
        )}
      </div>
    </div>
  );
}

function SignalCard({
  signalKey,
  value,
  errors,
  onChange,
}: {
  signalKey: SignalKey;
  value: SignalForm | null;
  errors: Record<string, string | undefined>;
  onChange: (v: SignalForm | null) => void;
}) {
  const [attrsText, setAttrsText] = useState(
    value
      ? Object.entries(value.resourceAttributes)
          .map(([k, v]) => `${k}=${v}`)
          .join("\n")
      : "",
  );

  if (!value) {
    return (
      <div className="card signal signal--empty">
        <h2>{SIGNAL_LABEL[signalKey]}</h2>
        <p className="muted">Not configured — stays in-cluster until you add an exporter.</p>
        <button
          type="button"
          className="btn"
          onClick={() => onChange(newSignal())}
        >
          Add {SIGNAL_LABEL[signalKey].toLowerCase()} exporter
        </button>
      </div>
    );
  }

  function patch(p: Partial<SignalForm>) {
    onChange({ ...value, ...p } as SignalForm);
  }

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
    <div className="card signal">
      <h2>{SIGNAL_LABEL[signalKey]}</h2>
      <div className="signal__grid">
        <label>
          <span>Endpoint</span>
          <input
            value={value.endpoint}
            onChange={(e) => patch({ endpoint: e.target.value || "" })}
            placeholder={
              value.protocol === "http" ? "https://backend:4318" : "otel-collector:4317"
            }
            aria-invalid={!!errors.endpoint}
          />
          {errors.endpoint && <em className="field-error">{errors.endpoint}</em>}
        </label>
        <label>
          <span>Protocol</span>
          <select
            value={value.protocol}
            onChange={(e) => patch({ protocol: e.target.value as SignalForm["protocol"] })}
          >
            <option value="http">http</option>
            <option value="grpc">grpc</option>
          </select>
        </label>
        <label>
          <span>Auth Secret ref</span>
          <input
            value={value.authSecretRef}
            onChange={(e) => patch({ authSecretRef: e.target.value || "" })}
            placeholder="my-exporter-secret (name only — never a token)"
            aria-invalid={!!errors.authSecretRef}
          />
          {errors.authSecretRef && (
            <em className="field-error">{errors.authSecretRef}</em>
          )}
        </label>
        <label>
          <span>Sampling (0–1)</span>
          <input
            type="number"
            min={0}
            max={1}
            step={0.05}
            value={value.sampling ?? ""}
            onChange={(e) =>
              patch({ sampling: e.target.value === "" ? null : Number(e.target.value) })
            }
            aria-invalid={!!errors.sampling}
          />
          {errors.sampling && <em className="field-error">{errors.sampling}</em>}
        </label>
        <label className="signal__attrs">
          <span>Resource attributes (one key=value per line)</span>
          <textarea
            rows={3}
            value={attrsText}
            onChange={(e) => {
              setAttrsText(e.target.value);
              patch({ resourceAttributes: parseAttrs(e.target.value) });
            }}
            placeholder={"deployment=production\nregion=eu"}
          />
        </label>
      </div>
      <button type="button" className="btn btn--danger" onClick={() => onChange(null)}>
        Remove {SIGNAL_LABEL[signalKey].toLowerCase()} exporter
      </button>
    </div>
  );
}
