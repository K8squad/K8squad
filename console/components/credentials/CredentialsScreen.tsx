"use client";

// CredentialsScreen — the Settings → Credentials surface (story 8.6, mock 05).
//
// A pure consumer of the apiserver's credential read model behind the BFF choke point: rows are
// per-agent BYO Secret refs with health (connected / refreshing / expired), and the clearest
// operator signal on the page is the paused-on-expiry banner (S10 / 7.4): which Run is held, by
// which credential, and the one-click re-login affordance (7.7 Connect Claude — legible
// not-configured state until ISI-2899 lands the OAuth flow).
//
// Honesty rules: unknown expiry renders "—", the 501 (read model not wired) renders an explicit
// unconfigured state, and the deny collapse (401/403/404) renders not-found — never a fabricated
// table and never another Team's rows. The user sees status but NEVER a token string (FR-G1/G2).

import { useCallback, useEffect, useState } from "react";
import {
  bannerHold,
  classifyCredentialsStatus,
  expiryLabel,
  healthBadge,
  tokenTypeLabel,
  type AgentCredentialRow,
  type CredentialsOutcome,
} from "@/lib/credentials";
import { EmptyState } from "@/components/forms/EmptyState";
import "./credentials.css";

export interface CredentialsScreenProps {
  /** Loader for the credential overview (BFF GET /api/credentials). Injectable for tests. */
  load?: () => Promise<Response>;
  /** Connect-Claude action (BFF POST /api/credentials/connect). Injectable for tests. */
  connect?: () => Promise<Response>;
  /** Clock for deterministic expiry derivations in tests. */
  now?: () => Date;
}

type LoadState =
  | "loading"
  | "ok"
  | "not-found"
  | "unconfigured"
  | "error";

export function CredentialsScreen({
  load = defaultLoad,
  connect = defaultConnect,
  now,
}: CredentialsScreenProps) {
  const [state, setState] = useState<LoadState>("loading");
  const [rows, setRows] = useState<AgentCredentialRow[]>([]);
  const [connectMsg, setConnectMsg] = useState<string | null>(null);
  const [connectBusy, setConnectBusy] = useState(false);

  useEffect(() => {
    let alive = true;
    load()
      .then((res) => {
        if (!alive) return;
        if (res.status === 501) {
          setState("unconfigured");
          return;
        }
        if (res.status >= 200 && res.status < 300) {
          return res.json().then((body) => {
            if (!alive) return;
            setRows(Array.isArray(body?.agents) ? body.agents : []);
            setState("ok");
          });
        }
        setState(classifyCredentialsStatus(res.status) === "not-found" ? "not-found" : "error");
      })
      .catch(() => alive && setState("error"));
    return () => {
      alive = false;
    };
  }, [load]);

  const onConnect = useCallback(async () => {
    setConnectBusy(true);
    setConnectMsg(null);
    try {
      const res = await connect();
      if (res.status === 501) {
        const body = await res.json().catch(() => null);
        setConnectMsg(
          body?.detail ??
            "Connect Claude is not configured yet — the OAuth flow is not available in this deployment.",
        );
      } else if (res.status >= 200 && res.status < 300) {
        setConnectMsg("Connect Claude flow started — check the opened authorization window.");
      } else {
        setConnectMsg("Connect Claude is unavailable right now — try again.");
      }
    } catch {
      setConnectMsg("Connect Claude is unreachable — try again.");
    } finally {
      setConnectBusy(false);
    }
  }, [connect]);

  const clock = now ?? (() => new Date());
  const hold = state === "ok" ? bannerHold(rows) : null;

  return (
    <section className="creds" data-testid="creds-screen" data-state={state}>
      <header className="creds__head">
        <div>
          <h1>Credentials &amp; auth state</h1>
          <p className="muted">
            Per-agent BYO subscription tokens — KSquad holds no master credential
          </p>
        </div>
        <div className="creds__connect">
          <button
            type="button"
            className="creds__connect-btn"
            onClick={onConnect}
            disabled={connectBusy}
            data-testid="connect-claude"
          >
            Connect Claude
          </button>
          <span className="creds__connect-hint muted">or `ksquad auth login` (CLI parity)</span>
        </div>
      </header>

      {connectMsg && (
        <p className="creds__connect-msg" data-testid="connect-msg" role="status">
          {connectMsg}
        </p>
      )}

      {hold && (
        <div className="creds__banner" data-testid="paused-banner" data-tone="bad">
          <span className="creds__banner-icon" aria-hidden>
            !
          </span>
          <div>
            <strong>
              Run <code>{hold.run.name}</code> paused — token expired
            </strong>
            <p className="muted">
              Agent <code>{hold.agent}</code> paused gracefully — coordination
              state preserved, resumes on refresh.
            </p>
          </div>
          <div className="creds__banner-actions">
            <button
              type="button"
              className="creds__refresh-btn"
              onClick={onConnect}
              disabled={connectBusy}
              data-testid="refresh-token"
            >
              Refresh token
            </button>
            <details className="creds__howto">
              <summary>How to (setup-token)</summary>
              <p className="muted">
                Re-login is one click once the zero-touch OAuth lifecycle
                is wired: <code>ksquad auth login</code> or the button above
                writes fresh tokens into the same per-user Secret — you never
                handle token strings.
              </p>
            </details>
          </div>
        </div>
      )}

      {state === "loading" && <p className="muted" data-testid="creds-loading">Loading credential state…</p>}

      {state === "unconfigured" && (
        <EmptyState
          testId="creds-unconfigured"
          title="Credential read model not configured"
          why="The apiserver answers its documented 501 — no credential read model is wired on this host (cluster-less run). The screen lights up when the informer cache backs GET /api/credentials."
        />
      )}

      {state === "not-found" && (
        <EmptyState
          testId="creds-not-found"
          title="No credential surface for this session"
          why="Sign in with a squad-scoped session — deny and missing are indistinguishable here by design."
        />
      )}

      {state === "error" && (
        <EmptyState
          testId="creds-error"
          title="Credential state unavailable"
          why="The apiserver could not serve the read model — retry shortly."
        />
      )}

      {state === "ok" && (
        <div className="creds__table-wrap" data-testid="creds-table">
          <table className="creds__table">
            <thead>
              <tr>
                <th>Agent</th>
                <th>Runtime</th>
                <th>Credential (Secret ref)</th>
                <th>Token</th>
                <th>Expires</th>
                <th>Status</th>
                <th>Runs</th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 && (
                <tr>
                  <td colSpan={7} className="creds__empty-row muted">
                    No agents with credentials in this squad yet — compose an
                    Agent with a per-user Secret ref.
                  </td>
                </tr>
              )}
              {rows.map((row) => {
                const badge = healthBadge(row, clock());
                return (
                  <tr key={row.agent} data-agent={row.agent} data-health={row.health}>
                    <td className="creds__agent">{row.agent}</td>
                    <td>{row.runtime}</td>
                    <td>
                      <code className="creds__ref">{row.credentialRef}</code>
                    </td>
                    <td>{tokenTypeLabel(row)}</td>
                    <td>{expiryLabel(row, clock())}</td>
                    <td>
                      <span className={`creds__badge creds__badge--${badge.tone}`} data-testid="health-badge">
                        {badge.label}
                      </span>
                    </td>
                    <td>
                      {(row.pausedRuns ?? []).map((pr) => (
                        <a
                          key={pr.name}
                          className="creds__run-link"
                          href={`/runs/${encodeURIComponent(pr.name)}`}
                        >
                          #{pr.name}
                        </a>
                      ))}
                      {!(row.pausedRuns ?? []).length && <span className="muted">idle</span>}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      <footer className="creds__foot muted">
        KSquad never stores a shared master credential. Each token is a per-user
        Kubernetes Secret ref (FR-G1). Expiry pauses the Run, never fails it
        opaquely (FR-G3 · S10).
      </footer>
    </section>
  );
}

async function defaultLoad(): Promise<Response> {
  return fetch("/api/credentials", { cache: "no-store" });
}

async function defaultConnect(): Promise<Response> {
  return fetch("/api/credentials/connect", { method: "POST" });
}
