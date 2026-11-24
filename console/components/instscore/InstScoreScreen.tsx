"use client";

// InstScoreScreen — the Instrumentation Score surface (ISI-7750, phase 4 of the
// ISI-7741 plan): the report-only view of the vendored instrumentation-score
// conformance run against observed workload telemetry.
//
// A pure consumer of the two apiserver read models behind the BFF choke point:
// GET /api/instscore/services (the list — every observed service, worst score
// first) and GET /api/instscore/services/{name} (the drill-in — the
// rule-by-rule verdicts with impact + failed-rule guidance, and the hourly
// score history). Both endpoints render on this one page: selecting a row
// loads the detail panel beneath.
//
// Honesty rules (house pattern, 8.6): the deny collapse (401/403/404) renders
// denied, the 501 renders unconfigured, an empty deployment renders empty —
// never a fabricated fallback. The empty-string service bucket (telemetry with
// a missing service.name) stays visible for its RES-005 violation but cannot
// be drilled into ({name} needs a path segment).

import { useCallback, useEffect, useState } from "react";
import {
  classifyInstScoreStatus,
  categoryTone,
  scoreLabel,
  serviceLabel,
  serviceNavigable,
  whenLabel,
  type InstScoreServiceDetail,
  type InstScoreServiceSummary,
  type InstScoreServicesResponse,
  type InstScoreLoadState,
  type InstScoreVerdict,
} from "@/lib/instscore";
import "./instscore.css";

export interface InstScoreScreenProps {
  /** List loader (BFF GET /api/instscore/services). Injectable for tests. */
  load?: () => Promise<Response>;
  /** Detail loader (BFF GET /api/instscore/services/{name}). Injectable for tests. */
  loadDetail?: (service: string) => Promise<Response>;
}

type DetailState =
  | { phase: "idle" }
  | { phase: "loading"; service: string }
  | { phase: "ok"; detail: InstScoreServiceDetail }
  | { phase: "missing"; service: string }
  | { phase: "error"; service: string };

export function InstScoreScreen({
  load = defaultLoad,
  loadDetail = defaultLoadDetail,
}: InstScoreScreenProps) {
  const [state, setState] = useState<InstScoreLoadState>("loading");
  const [rows, setRows] = useState<InstScoreServiceSummary[]>([]);
  const [rulesTotal, setRulesTotal] = useState(0);
  const [detail, setDetail] = useState<DetailState>({ phase: "idle" });

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await load();
        const kind = classifyInstScoreStatus(res.status);
        if (kind === "ok") {
          const body: InstScoreServicesResponse = await res.json();
          const services = Array.isArray(body?.services) ? body.services : [];
          if (cancelled) return;
          setRows(services);
          setRulesTotal(typeof body?.rules_total === "number" ? body.rules_total : 0);
          setState(services.length === 0 ? "empty" : "ok");
          return;
        }
        if (!cancelled) setState(kind);
      } catch {
        if (!cancelled) setState("error");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [load]);

  const openDetail = useCallback(
    async (service: string) => {
      setDetail({ phase: "loading", service });
      try {
        const res = await loadDetail(service);
        if (res.status >= 200 && res.status < 300) {
          const body: InstScoreServiceDetail = await res.json();
          setDetail({ phase: "ok", detail: body });
        } else if (res.status === 404) {
          setDetail({ phase: "missing", service });
        } else {
          setDetail({ phase: "error", service });
        }
      } catch {
        setDetail({ phase: "error", service });
      }
    },
    [loadDetail],
  );

  return (
    <section className="instscore" aria-labelledby="instscore-title">
      <header className="instscore__head">
        <h1 id="instscore-title">Instrumentation Score</h1>
        <p className="instscore__sub">
          Conformance of observed workload telemetry against the vendored
          instrumentation-score rules — report-only, no gates. Rule set:{" "}
          {rulesTotal > 0 ? `${rulesTotal} rules` : "—"}; window 30 days.
        </p>
      </header>

      {state === "loading" && (
        <p className="instscore__note" role="status">
          Loading instrumentation scores…
        </p>
      )}
      {state === "denied" && (
        <p className="instscore__note instscore__note--denied" role="status">
          Denied — this surface needs a console session.
        </p>
      )}
      {state === "unconfigured" && (
        <p className="instscore__note" role="status">
          Unconfigured — the apiserver reports no instrumentation-score store
          (migration 0042 / the OTel gateway tap of the conformance plan).
        </p>
      )}
      {state === "error" && (
        <p className="instscore__note instscore__note--error" role="alert">
          The instrumentation-score read model is unavailable.
        </p>
      )}
      {state === "empty" && (
        <p className="instscore__note" role="status">
          No services observed yet — telemetry reaches this view through the
          OTel gateway&apos;s instrumentation-score tap.
        </p>
      )}

      {state === "ok" && (
        <table className="instscore__table">
          <caption className="instscore__caption">
            Services, worst score first
          </caption>
          <thead>
            <tr>
              <th scope="col">Service</th>
              <th scope="col">Score</th>
              <th scope="col">Category</th>
              <th scope="col">Rules</th>
              <th scope="col">Evidence</th>
              <th scope="col">Last evaluated</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.service || "(unset)"}>
                <td>
                  {serviceNavigable(row.service) ? (
                    <button
                      type="button"
                      className="instscore__svc"
                      onClick={() => openDetail(row.service)}
                      aria-expanded={
                        (detail.phase === "ok" &&
                          detail.detail.service === row.service) ||
                        (detail.phase !== "idle" &&
                          detail.phase !== "ok" &&
                          detail.service === row.service)
                      }
                    >
                      {serviceLabel(row.service)}
                    </button>
                  ) : (
                    <span title="No service.name on this telemetry — RES-005 violation; no detail route">
                      {serviceLabel(row.service)}
                    </span>
                  )}
                </td>
                <td>
                  <span
                    className={`instscore__badge instscore__badge--${categoryTone(
                      row.category,
                      row.score,
                    )}`}
                  >
                    {scoreLabel(row)}
                  </span>
                </td>
                <td>{row.category ?? "—"}</td>
                <td>
                  {row.implemented > 0 || row.total > 0 ? (
                    <>
                      {row.pass} pass · {row.fail} fail
                      {row.not_evaluated > 0
                        ? ` · ${row.not_evaluated} n/a`
                        : ""}
                    </>
                  ) : (
                    "—"
                  )}
                </td>
                <td>
                  {row.spans} spans · {row.logs} logs · {row.metrics} metrics
                </td>
                <td>{whenLabel(row.evaluated_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {detail.phase === "loading" && (
        <p className="instscore__note" role="status">
          Loading {serviceLabel(detail.service)} verdicts…
        </p>
      )}
      {detail.phase === "missing" && (
        <p className="instscore__note" role="status">
          {serviceLabel(detail.service)} is no longer observed in the window.
        </p>
      )}
      {detail.phase === "error" && (
        <p className="instscore__note instscore__note--error" role="alert">
          Could not load {serviceLabel(detail.service)}.
        </p>
      )}

      {detail.phase === "ok" && (
        <InstScoreDetailPanel detail={detail.detail} />
      )}
    </section>
  );
}

/** The drill-in panel: summary, hourly history sparkline, rule verdict table. */
function InstScoreDetailPanel({ detail }: { detail: InstScoreServiceDetail }) {
  return (
    <article className="instscore__detail" aria-label={`${serviceLabel(detail.service)} verdicts`}>
      <header className="instscore__detail-head">
        <h2>{serviceLabel(detail.service)}</h2>
        <p>
          <span
            className={`instscore__badge instscore__badge--${categoryTone(
              detail.category,
              detail.score,
            )}`}
          >
            {scoreLabel(detail)}
          </span>{" "}
          {detail.category ?? "not yet evaluated"}
          {detail.total > 0 && (
            <>
              {" "}
              · disclosed {detail.implemented}/{detail.total} rules
            </>
          )}
          {" "}· evaluated {whenLabel(detail.evaluated_at)}
        </p>
      </header>

      {detail.history.length > 1 && (
        <InstScoreHistorySparkline history={detail.history} />
      )}

      <table className="instscore__table instscore__table--verdicts">
        <caption className="instscore__caption">
          Rule-by-rule verdicts (impact order within rule-ID order)
        </caption>
        <thead>
          <tr>
            <th scope="col">Rule</th>
            <th scope="col">Impact</th>
            <th scope="col">Target</th>
            <th scope="col">Status</th>
            <th scope="col">Evidence{""}</th>
          </tr>
        </thead>
        <tbody>
          {detail.verdicts.map((v) => (
            <InstScoreVerdictRow key={v.rule} verdict={v} />
          ))}
        </tbody>
      </table>
    </article>
  );
}

function InstScoreVerdictRow({ verdict }: { verdict: InstScoreVerdict }) {
  const [open, setOpen] = useState(false);
  const failed = verdict.status === "fail";
  const skipped = verdict.status === "not_evaluated";
  return (
    <tr
      className={
        failed
          ? "instscore__row--fail"
          : skipped
            ? "instscore__row--na"
            : undefined
      }
    >
      <td className="instscore__rule">
        <span className="instscore__rule-id">{verdict.rule}</span>
        {verdict.description && (
          <span className="instscore__rule-desc">{verdict.description}</span>
        )}
      </td>
      <td>{verdict.impact}</td>
      <td>{verdict.target?.join(", ") || "—"}</td>
      <td>
        <span
          className={`instscore__status instscore__status--${verdict.status}`}
        >
          {verdict.status === "not_evaluated" ? "not evaluated" : verdict.status}
        </span>
      </td>
      <td>
        <div className="instscore__evidence">
          <span>{verdict.evidence}</span>
          {failed && verdict.guidance && (
            <>
              <button
                type="button"
                className="instscore__fix"
                aria-expanded={open}
                onClick={() => setOpen((o) => !o)}
              >
                {open ? "Hide guidance" : "How to fix"}
              </button>
              {open && (
                <p className="instscore__guidance">{verdict.guidance}</p>
              )}
            </>
          )}
        </div>
      </td>
    </tr>
  );
}

/** Minimal inline SVG sparkline over the hourly history (oldest → newest). */
function InstScoreHistorySparkline({
  history,
}: {
  history: InstScoreServiceDetail["history"];
}) {
  const w = 320;
  const h = 48;
  const n = history.length;
  const x = (i: number) => (i / Math.max(1, n - 1)) * (w - 2) + 1;
  const y = (score: number) => h - 2 - (Math.max(0, Math.min(100, score)) / 100) * (h - 6);
  const points = history.map((p, i) => `${x(i).toFixed(1)},${y(p.score).toFixed(1)}`).join(" ");
  const first = history[0]?.score ?? 0;
  const last = history[n - 1]?.score ?? 0;
  return (
    <figure className="instscore__spark">
      <svg
        viewBox={`0 0 ${w} ${h}`}
        width={w}
        height={h}
        role="img"
        aria-label={`Hourly score history, ${first.toFixed(1)} to ${last.toFixed(1)} over ${n} hours`}
      >
        <polyline
          className="instscore__spark-line"
          points={points}
          fill="none"
          strokeWidth="2"
        />
      </svg>
      <figcaption>
        Hourly history · {first.toFixed(1)} → {last.toFixed(1)}
      </figcaption>
    </figure>
  );
}

async function defaultLoad(): Promise<Response> {
  return fetch("/api/instscore/services");
}

async function defaultLoadDetail(service: string): Promise<Response> {
  return fetch(`/api/instscore/services/${encodeURIComponent(service)}`);
}
