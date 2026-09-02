"use client";

// components/ArtifactBrowser.tsx — the story 8.3 screen body (ISI-2900).
//
// The artifact + handoff-output inspection view of ONE Run, fed by the coordination record
// through the BFF (app/api/runs/[runId]/artifacts[/[artifactId]]). The apiserver applies the
// 8.7d per-principal + Team-scope gate and collapses every deny-or-missing path to 404
// (existence-hiding) — so this component's 404 rendering is honest for BOTH "no such Run"
// and "not yours to see", with no way to distinguish them from the browser.
//
// Layout: the structured handoff (story 2.8 contract — did/decisions/next/blockers/
// findings/recommended_next/artifacts_for_downstream) renders as summary cards; the
// registered artifact rows render as a table; selecting a row fetches its digest-verified
// canonical bytes (capped at 512 KiB server-side; base64 JSON envelope) into a read-only
// viewer. Strictly read-only — inspection, not mutation (R6 scope guard).

import { useEffect, useRef, useState } from "react";

/** One coord.artifact row as the apiserver lists it. */
export interface ArtifactRow {
  id: string;
  workItemId: string;
  runId: string;
  kind: string;
  uri: string;
  sha256: string;
  createdAt: string;
}

/** The story 2.8 structured handoff contract (coord.HandoffDoc), snake_case as stored. */
export interface HandoffDoc {
  did?: string[];
  decisions?: string[];
  next?: string[];
  blockers?: string[];
  findings?: string;
  recommended_next?: { title: string; body?: string }[];
  artifacts_for_downstream?: { kind: string; uri: string; sha256: string }[];
}

interface Listing {
  runId: string;
  artifacts: ArtifactRow[] | null;
  handoff?: HandoffDoc | null;
}

/** GET .../artifacts/{id} — content is base64 (Go []byte JSON marshalling). */
interface ContentResult {
  artifact: ArtifactRow;
  content: string;
  size: number;
  truncated: boolean;
}

type ListState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "not-found" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: Listing };

export function classifyArtifactsStatus(status: number): ListState {
  switch (status) {
    case 401:
      return { kind: "unauthenticated" };
    case 404:
      return { kind: "not-found" };
    case 501:
      return { kind: "not-wired" };
    default:
      return { kind: "error", status };
  }
}

/** Decode the base64 content envelope to displayable text (best-effort for JSON blobs). */
export function decodeContent(b64: string): string {
  try {
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch {
    return "(undecodable content)";
  }
}

function Section({ title, items }: { title: string; items: string[] | undefined }) {
  if (!items || items.length === 0) return null;
  return (
    <div data-testid={`handoff-${title}`}>
      <h3 style={{ margin: "8px 0 4px", textTransform: "capitalize" }}>{title}</h3>
      <ul style={{ margin: 0, paddingLeft: 18 }}>
        {items.map((it, i) => (
          <li key={i}>{it}</li>
        ))}
      </ul>
    </div>
  );
}

export function ArtifactBrowser({ runId }: { runId: string }) {
  const [state, setState] = useState<ListState>({ kind: "loading" });
  const [selected, setSelected] = useState<{ row: ArtifactRow; text: string; truncated: boolean; size: number } | null>(
    null,
  );
  const [loadingRow, setLoadingRow] = useState<string | null>(null);
  // Latest in-flight inspect request: a response is applied only if it is still the newest
  // request. Two rapid Inspect clicks must not let the slower response overwrite the newer
  // row's content, and a late response after navigation must not set state at all.
  const latestRequest = useRef<number>(0);

  useEffect(() => {
    let alive = true;
    fetch(`/api/runs/${encodeURIComponent(runId)}/artifacts`, {
      headers: { accept: "application/json" },
    })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          setState(classifyArtifactsStatus(res.status));
          return;
        }
        setState({ kind: "ready", data: (await res.json()) as Listing });
      })
      .catch(() => {
        if (alive) setState({ kind: "error", status: 0 });
      });
    return () => {
      alive = false;
    };
  }, [runId]);

  const openArtifact = async (row: ArtifactRow) => {
    const request = ++latestRequest.current;
    setLoadingRow(row.id);
    const stale = () => request !== latestRequest.current;
    try {
      const res = await fetch(
        `/api/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(row.id)}`,
        { headers: { accept: "application/json" } },
      );
      if (stale()) return;
      if (!res.ok) {
        setSelected({ row, text: `(content unavailable — HTTP ${res.status})`, truncated: false, size: 0 });
        return;
      }
      const body = (await res.json()) as ContentResult;
      if (stale()) return;
      setSelected({
        row,
        text: decodeContent(body.content),
        truncated: body.truncated,
        size: body.size,
      });
    } catch {
      if (!stale()) setSelected({ row, text: "(content fetch failed)", truncated: false, size: 0 });
    } finally {
      if (!stale()) setLoadingRow(null);
    }
  };

  if (state.kind === "loading") {
    return (
      <div className="card" data-testid="artifacts-loading">
        Loading artifacts…
      </div>
    );
  }
  if (state.kind === "unauthenticated") {
    return (
      <div className="card" data-testid="artifacts-unauthenticated">
        <p className="muted" style={{ margin: 0 }}>
          Sign in to inspect artifacts — the view is scoped to your session.
        </p>
      </div>
    );
  }
  // 404 renders the SAME card for a missing Run and a denied caller (existence-hiding).
  if (state.kind === "not-found") {
    return (
      <div className="card" data-testid="artifacts-not-found">
        <p className="muted" style={{ margin: 0 }}>
          No artifact view for this Run — it does not exist or is not in your Team&apos;s scope.
        </p>
      </div>
    );
  }
  if (state.kind === "not-wired") {
    return (
      <div className="card" data-testid="artifacts-not-wired">
        <p className="muted" style={{ margin: 0 }}>
          This apiserver runs without the artifact-browser read model (dev / DB-less run) and
          answers its documented 501.
        </p>
      </div>
    );
  }
  if (state.kind === "error") {
    return (
      <div className="card" data-testid="artifacts-error">
        <p className="muted" style={{ margin: 0 }}>
          Artifacts could not be reached (HTTP {state.status || "network error"}). Retry shortly.
        </p>
      </div>
    );
  }

  const { artifacts, handoff } = state.data;

  return (
    <div data-testid="artifacts-ready">
      {handoff ? (
        <section className="card" data-testid="handoff-card">
          <h2 style={{ margin: "0 0 4px" }}>Structured handoff</h2>
          <p className="muted" style={{ margin: "0 0 8px", fontSize: 13 }}>
            The advisory handoff contract this Run registered with the coordination record.
          </p>
          <Section title="did" items={handoff.did} />
          <Section title="decisions" items={handoff.decisions} />
          <Section title="next" items={handoff.next} />
          <Section title="blockers" items={handoff.blockers} />
          {handoff.findings ? (
            <div data-testid="handoff-findings">
              <h3 style={{ margin: "8px 0 4px" }}>Findings</h3>
              <p style={{ margin: 0 }}>{handoff.findings}</p>
            </div>
          ) : null}
          {handoff.recommended_next && handoff.recommended_next.length > 0 ? (
            <div data-testid="handoff-recommended">
              <h3 style={{ margin: "8px 0 4px" }}>Recommended next</h3>
              <ul style={{ margin: 0, paddingLeft: 18 }}>
                {handoff.recommended_next.map((d, i) => (
                  <li key={i}>
                    <strong>{d.title}</strong>
                    {d.body ? <span className="muted"> — {d.body}</span> : null}
                  </li>
                ))}
              </ul>
            </div>
          ) : null}
          {handoff.artifacts_for_downstream && handoff.artifacts_for_downstream.length > 0 ? (
            <div data-testid="handoff-downstream">
              <h3 style={{ margin: "8px 0 4px" }}>Artifacts for downstream</h3>
              <ul style={{ margin: 0, paddingLeft: 18 }}>
                {handoff.artifacts_for_downstream.map((a, i) => (
                  <li key={i}>
                    <code>{a.kind}</code> — {a.uri}
                  </li>
                ))}
              </ul>
            </div>
          ) : null}
        </section>
      ) : null}

      <section className="card">
        <h2 style={{ margin: "0 0 8px" }}>Artifacts</h2>
        {!artifacts || artifacts.length === 0 ? (
          <p className="muted" style={{ margin: 0 }} data-testid="artifacts-empty">
            The coordination record holds no artifacts for this Run yet.
          </p>
        ) : (
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr className="muted" style={{ textAlign: "left", fontSize: 12 }}>
                <th style={{ padding: "4px 8px 4px 0" }}>Kind</th>
                <th style={{ padding: "4px 8px 4px 0" }}>Digest</th>
                <th style={{ padding: "4px 8px 4px 0" }}>Registered</th>
                <th style={{ padding: "4px 8px 4px 0" }} />
              </tr>
            </thead>
            <tbody>
              {artifacts.map((a) => (
                <tr key={a.id} data-testid="artifact-row">
                  <td style={{ padding: "4px 8px 4px 0" }}>
                    <span className="kind-badge">{a.kind}</span>
                  </td>
                  <td style={{ padding: "4px 8px 4px 0" }}>
                    <code title={a.sha256}>{a.sha256.slice(0, 12)}</code>
                  </td>
                  <td style={{ padding: "4px 8px 4px 0" }}>{new Date(a.createdAt).toLocaleString()}</td>
                  <td style={{ padding: "4px 8px 4px 0" }}>
                    <button
                      type="button"
                      onClick={() => void openArtifact(a)}
                      disabled={loadingRow === a.id}
                      data-testid="artifact-open"
                    >
                      {loadingRow === a.id ? "Loading…" : "Inspect"}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {selected ? (
        <section className="card" data-testid="artifact-viewer">
          <h2 style={{ margin: "0 0 4px" }}>
            <span className="kind-badge">{selected.row.kind}</span> content
          </h2>
          <p className="muted" style={{ margin: "0 0 8px", fontSize: 12 }}>
            {selected.row.uri} · {selected.size} bytes
            {selected.truncated ? " · truncated at 512 KiB cap" : ""}
          </p>
          <pre
            style={{
              margin: 0,
              padding: 12,
              borderRadius: "var(--radius)",
              background: "color-mix(in srgb, var(--accent) 8%, transparent)",
              overflowX: "auto",
              fontFamily: "var(--font-mono)",
              fontSize: 13,
            }}
          >
            {selected.text}
          </pre>
        </section>
      ) : null}
    </div>
  );
}
