"use client";

// AuditTrailScreen — the audit-trail surface (story 2.6 / ISI-2881).
//
// A pure consumer of the apiserver's GET /api/audit read model behind the BFF choke point: the
// immutable coord.audit_log rendered as who / what / when / result rows, filterable by work
// item, run, actor, event type, and time window, with id-cursor "load older" pagination riding
// the log's monotonic sequence.
//
// Honesty rules (house pattern, 8.6): a 403 renders the specific self-scope explanation (the
// server uses it for exactly one thing — a non-admin querying a foreign actor), the deny collapse
// (401/404) renders denied, the 501 renders unconfigured, and unknown event types still render
// with their raw type — never a fabricated label.

import { useCallback, useEffect, useState } from "react";
import {
  AUDIT_EVENT_TYPES,
  actorLabel,
  auditQueryString,
  classifyAuditStatus,
  emptyAuditFilters,
  eventBadge,
  resultLabel,
  shortId,
  whenLabel,
  type AuditEvent,
  type AuditFilters,
} from "@/lib/audit";
import "./audit.css";

export interface AuditTrailScreenProps {
  /** Loader (BFF GET /api/audit?<qs>). Injectable for tests. */
  load?: (queryString: string) => Promise<Response>;
}

type LoadState = "loading" | "ok" | "denied" | "forbidden-actor" | "unconfigured" | "error";

export function AuditTrailScreen({ load = defaultLoad }: AuditTrailScreenProps) {
  const [filters, setFilters] = useState<AuditFilters>(emptyAuditFilters);
  const [applied, setApplied] = useState<AuditFilters>(emptyAuditFilters);
  const [state, setState] = useState<LoadState>("loading");
  const [rows, setRows] = useState<AuditEvent[]>([]);
  const [nextBefore, setNextBefore] = useState<number | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);

  const fetchPage = useCallback(
    async (qs: string, mode: "replace" | "append") => {
      try {
        const res = await load(qs);
        if (res.status >= 200 && res.status < 300) {
          const body = await res.json();
          const events: AuditEvent[] = Array.isArray(body?.events) ? body.events : [];
          const cursor = typeof body?.nextBefore === "number" ? body.nextBefore : null;
          if (mode === "append") setRows((prev) => [...prev, ...events]);
          else setRows(events);
          setNextBefore(cursor);
          setState("ok");
          return;
        }
        const kind = classifyAuditStatus(res.status);
        if (mode === "replace" || kind !== "error") setState(kind);
      } catch {
        if (mode === "replace") setState("error");
      }
    },
    [load],
  );

  useEffect(() => {
    let alive = true;
    setState("loading");
    setRows([]);
    setNextBefore(null);
    auditLoad(alive, applied, fetchPage);
    return () => {
      alive = false;
    };
  }, [applied, fetchPage]);

  const onFilterSubmit = (ev: React.FormEvent) => {
    ev.preventDefault();
    setApplied({ ...filters });
  };

  const onLoadOlder = async () => {
    if (nextBefore == null || loadingOlder) return;
    setLoadingOlder(true);
    await fetchPage(auditQueryString(applied, nextBefore), "append");
    setLoadingOlder(false);
  };

  const set = (patch: Partial<AuditFilters>) => setFilters((f) => ({ ...f, ...patch }));

  return (
    <section className="audit" data-testid="audit-screen" data-state={state}>
      <header className="audit__head">
        <div>
          <h1>Audit trail</h1>
          <p className="muted">
            The coordination record — every checkout, comment, artifact, and transition as an
            immutable who / what / when / result row
          </p>
        </div>
      </header>

      <form className="audit__filters" onSubmit={onFilterSubmit} data-testid="audit-filters">
        <label>
          Work item
          <input
            value={filters.workItem}
            onChange={(e) => set({ workItem: e.target.value })}
            placeholder="uuid"
            data-testid="audit-filter-work-item"
          />
        </label>
        <label>
          Run
          <input
            value={filters.run}
            onChange={(e) => set({ run: e.target.value })}
            placeholder="uuid"
            data-testid="audit-filter-run"
          />
        </label>
        <label>
          Actor
          <input
            value={filters.actor}
            onChange={(e) => set({ actor: e.target.value })}
            placeholder="principal (admin-only for others)"
            data-testid="audit-filter-actor"
          />
        </label>
        <label>
          Event
          <select
            value={filters.eventType}
            onChange={(e) => set({ eventType: e.target.value })}
            data-testid="audit-filter-event"
          >
            <option value="">All</option>
            {AUDIT_EVENT_TYPES.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </label>
        <label>
          From
          <input
            type="datetime-local"
            value={filters.from}
            onChange={(e) => set({ from: e.target.value })}
            data-testid="audit-filter-from"
          />
        </label>
        <label>
          To
          <input
            type="datetime-local"
            value={filters.to}
            onChange={(e) => set({ to: e.target.value })}
            data-testid="audit-filter-to"
          />
        </label>
        <button type="submit" data-testid="audit-filter-apply">
          Apply
        </button>
      </form>

      {state === "loading" ? (
        <p className="audit__state" data-testid="audit-loading">Loading audit trail…</p>
      ) : state === "denied" ? (
        <p className="audit__state" data-testid="audit-denied">
          Sign in to view the audit trail.
        </p>
      ) : state === "forbidden-actor" ? (
        <p className="audit__state" data-testid="audit-forbidden">
          The audit trail is admin-scoped — you can query your own principal&apos;s activity only.
          Clear the actor filter to see your own trail.
        </p>
      ) : state === "unconfigured" ? (
        <p className="audit__state" data-testid="audit-unconfigured">
          The audit read model is not wired on this apiserver (dev run without a database reader).
        </p>
      ) : state === "error" ? (
        <p className="audit__state" data-testid="audit-error">
          The audit trail is unavailable right now — try again.
        </p>
      ) : (
        <>
          <div className="audit__table-wrap">
            <table className="audit__table" data-testid="audit-table">
              <thead>
                <tr>
                  <th>#</th>
                  <th>When</th>
                  <th>Actor</th>
                  <th>Event</th>
                  <th>Work item</th>
                  <th>Run</th>
                  <th>Result</th>
                </tr>
              </thead>
              <tbody>
                {rows.length === 0 ? (
                  <tr className="audit__empty">
                    <td colSpan={7}>No audit events match these filters.</td>
                  </tr>
                ) : (
                  rows.map((e) => {
                    const badge = eventBadge(e);
                    return (
                      <tr key={e.id} data-testid="audit-row">
                        <td>{e.id}</td>
                        <td title={e.createdAt}>{whenLabel(e.createdAt)}</td>
                        <td title={e.principal}>{actorLabel(e.principal)}</td>
                        <td>
                          <span className={`audit__badge audit__badge--${badge.tone}`}>
                            {badge.label}
                          </span>
                        </td>
                        <td title={e.workItemId ?? undefined}>{shortId(e.workItemId)}</td>
                        <td title={e.runId ?? undefined}>{shortId(e.runId)}</td>
                        <td>{resultLabel(e)}</td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
          {nextBefore != null ? (
            <button
              type="button"
              className="audit__older"
              onClick={onLoadOlder}
              disabled={loadingOlder}
              data-testid="audit-load-older"
            >
              {loadingOlder ? "Loading…" : "Load older"}
            </button>
          ) : (
            rows.length > 0 && (
              <p className="audit__tail muted" data-testid="audit-tail">
                End of the trail.
              </p>
            )
          )}
        </>
      )}
    </section>
  );
}

function defaultLoad(queryString: string): Promise<Response> {
  return fetch(`/api/audit?${queryString}`, { cache: "no-store" });
}

// auditLoad keeps the mount-effect body tiny: fetch + state transitions only run while `alive`
// so a fast unmount never sets state on a dead component.
async function auditLoad(
  alive: boolean,
  filters: AuditFilters,
  fetchPage: (qs: string, mode: "replace" | "append") => Promise<void>,
) {
  if (!alive) return;
  await fetchPage(auditQueryString(filters), "replace");
}
