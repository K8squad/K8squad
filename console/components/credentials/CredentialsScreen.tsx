"use client";

// CredentialsScreen — the Settings → Credentials surface (story 8.6, mock 05; ISI-3983 write UX).
//
// A consumer of the apiserver's credential read model behind the BFF choke point — rows are
// per-agent BYO Secret refs with health (connected / refreshing / expired) — AND the v1 WRITE
// surface: a bring-your-own service-account key paste form (POST /api/credentials, ISI-3679) with
// a fleet-admin team picker (ISI-3937). The clearest operator signal on the page is still the
// paused-on-expiry banner (S10 / 7.4): which Run is held, by which credential, and the one-click
// re-login affordance (7.7 Connect Claude — a legible not-configured state until ISI-2899 lands
// the OAuth flow; the paste form is the honest v1 path in the meantime).
//
// Honesty rules: unknown expiry renders "—", the 501 (read model not wired) renders an explicit
// unconfigured state, the deny collapse (401/403/404) renders not-found, and a fleet admin who
// must choose a team (400 "select a team") gets the team picker — never a fabricated table and
// never another Team's rows. The user sees status but NEVER a token string (FR-G1/G2).

import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  bannerHold,
  classifyCreateStatus,
  classifyCredentialsStatus,
  credentialCreateBody,
  expiryLabel,
  healthBadge,
  tokenTypeLabel,
  CREATE_RUNTIME_OPTIONS,
  type AgentCredentialRow,
  type CredentialTeamList,
  type CredentialTeamOption,
} from "@/lib/credentials";
import { EmptyState } from "@/components/forms/EmptyState";
import "./credentials.css";

export interface CredentialsScreenProps {
  /** Loader for the credential overview (BFF GET /api/credentials?teamId=). Injectable for tests. */
  load?: (teamId?: string) => Promise<Response>;
  /** Fleet team list loader (BFF GET /api/squad/teams) for the admin picker. Injectable for tests. */
  loadTeams?: () => Promise<Response>;
  /** BYO-key create action (BFF POST /api/credentials). Injectable for tests. */
  create?: (body: Record<string, string>) => Promise<Response>;
  /** Test-connection probe (BFF POST /api/credentials/{name}/test). Injectable for tests. */
  test?: (name: string, body: { runtime: string; teamId?: string }) => Promise<Response>;
  /** Connect-Claude action (BFF POST /api/credentials/connect). Injectable for tests. */
  connect?: () => Promise<Response>;
  /** Clock for deterministic expiry derivations in tests. */
  now?: () => Date;
}

type LoadState =
  | "loading"
  | "ok"
  | "needs-team" // fleet admin must pick a team before the read/write model resolves (400)
  | "not-found"
  | "unconfigured"
  | "error";

export function CredentialsScreen({
  load = defaultLoad,
  loadTeams = defaultLoadTeams,
  create = defaultCreate,
  test = defaultTest,
  connect = defaultConnect,
  now,
}: CredentialsScreenProps) {
  const [state, setState] = useState<LoadState>("loading");
  const [rows, setRows] = useState<AgentCredentialRow[]>([]);
  const [connectMsg, setConnectMsg] = useState<string | null>(null);
  const [connectBusy, setConnectBusy] = useState(false);

  // Fleet-admin team context (ISI-3937). `teams` is populated lazily when the apiserver asks the
  // admin to select one (400) or when it hands back a fleet team list; a bound single-team caller
  // never loads it and `selectedTeam` stays empty (teamId omitted upstream → own-team default).
  const [teams, setTeams] = useState<CredentialTeamOption[]>([]);
  const [selectedTeam, setSelectedTeam] = useState<string>("");

  const fetchCreds = useCallback(
    (teamId: string) => {
      setState("loading");
      load(teamId || undefined)
        .then(async (res) => {
          if (res.status === 501) {
            setState("unconfigured");
            return;
          }
          if (res.status >= 200 && res.status < 300) {
            const body = await res.json().catch(() => ({}));
            setRows(Array.isArray(body?.agents) ? body.agents : []);
            setState("ok");
            return;
          }
          if (res.status === 400) {
            // Fleet admin with >1 team and no teamId (ISI-3937): show the picker, not an error.
            // Load the fleet team list; auto-select + re-fetch when exactly one team exists (the
            // single-team fallback the issue calls for).
            setState("needs-team");
            try {
              const tRes = await loadTeams();
              if (tRes.status >= 200 && tRes.status < 300) {
                const tBody = (await tRes.json().catch(() => ({}))) as CredentialTeamList;
                const list = (tBody?.teams ?? []).filter(
                  (t): t is CredentialTeamOption => !!t,
                );
                setTeams(list);
                if (!teamId && list.length === 1) {
                  setSelectedTeam(list[0].uid);
                  fetchCreds(list[0].uid);
                }
              }
            } catch {
              // Leave the picker in its prompt; the admin can retry the page.
            }
            return;
          }
          setState(
            classifyCredentialsStatus(res.status) === "not-found" ? "not-found" : "error",
          );
        })
        .catch(() => setState("error"));
    },
    [load, loadTeams],
  );

  useEffect(() => {
    fetchCreds("");
    // fetchCreds is stable per `load`; re-run only when the loader identity changes (tests).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [load]);

  const onSelectTeam = useCallback(
    (uid: string) => {
      setSelectedTeam(uid);
      if (uid) fetchCreds(uid);
    },
    [fetchCreds],
  );

  // Silent post-create refresh: re-read the rows WITHOUT flipping to the "loading" spinner, so the
  // create form (a child with local success/test state) stays mounted and the confirmation persists.
  const silentRefresh = useCallback(
    (teamId: string) => {
      load(teamId || undefined)
        .then(async (res) => {
          if (res.status >= 200 && res.status < 300) {
            const body = await res.json().catch(() => ({}));
            setRows(Array.isArray(body?.agents) ? body.agents : []);
            setState("ok");
          }
        })
        .catch(() => {
          /* keep the current view — a failed refresh must not erase the success message */
        });
    },
    [load],
  );

  const onConnect = useCallback(async () => {
    setConnectBusy(true);
    setConnectMsg(null);
    try {
      const res = await connect();
      if (res.status === 501) {
        // Honest + friendly: the OAuth flow isn't hosted yet (ISI-2899). We do NOT surface the raw
        // apiserver `detail` (leaks internals) and we do NOT point at a `ksquad auth login` CLI —
        // no such CLI ships today (ISI-3945). We DO route to the v1 path: the paste form below.
        setConnectMsg(
          "Connect Claude isn't available yet — one-click sign-in is coming soon (ISI-2899). Add a service-account key below to bring your own credential now.",
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
  const showForm = state === "ok" || state === "needs-team";
  const needsTeamSelection = teams.length > 1;
  const teamReady = !needsTeamSelection || selectedTeam !== "";

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
          <span className="creds__connect-hint muted">Zero-touch OAuth — coming soon (ISI-2899). No CLI is shipped yet.</span>
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
                Re-login becomes one click once the zero-touch OAuth lifecycle
                is wired (ISI-2899): the Connect Claude button above will write
                fresh tokens into the same per-user Secret — you never handle
                token strings. No <code>ksquad auth</code> CLI ships today.
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

      {(needsTeamSelection || state === "needs-team") && (
        <TeamPicker
          teams={teams}
          selected={selectedTeam}
          onSelect={onSelectTeam}
          prompt={state === "needs-team"}
        />
      )}

      {showForm && (
        <CreateCredentialForm
          create={create}
          test={test}
          teamId={selectedTeam}
          teamReady={teamReady}
          onCreated={() => silentRefresh(selectedTeam)}
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

/** Fleet-admin team picker (ISI-3937). Rendered only when the fleet exposes more than one Team;
 *  a single-team caller is auto-selected upstream and never sees it. */
function TeamPicker({
  teams,
  selected,
  onSelect,
  prompt,
}: {
  teams: CredentialTeamOption[];
  selected: string;
  onSelect: (uid: string) => void;
  prompt: boolean;
}) {
  return (
    <div className="creds__team" data-testid="creds-team-picker">
      <label className="creds__team-label" htmlFor="creds-team-select">
        Team
      </label>
      <select
        id="creds-team-select"
        className="creds__team-select"
        value={selected}
        onChange={(e) => onSelect(e.target.value)}
        data-testid="creds-team-select"
      >
        <option value="">Select a team…</option>
        {teams.map((t) => (
          <option key={t.uid} value={t.uid}>
            {t.name} ({t.namespace})
          </option>
        ))}
      </select>
      {prompt && selected === "" && (
        <p className="muted" data-testid="creds-team-prompt">
          You administer more than one squad — pick a team to view and add its credentials.
        </p>
      )}
    </div>
  );
}

/** The BYO service-account paste form (ISI-3983). Value is write-only; the apiserver stores it and
 *  returns a secretRef, never the material. Human-seat OAuth stays the Connect Claude path. */
function CreateCredentialForm({
  create,
  test,
  teamId,
  teamReady,
  onCreated,
}: {
  create: (body: Record<string, string>) => Promise<Response>;
  test: (name: string, body: { runtime: string; teamId?: string }) => Promise<Response>;
  teamId: string;
  teamReady: boolean;
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [runtime, setRuntime] = useState(CREATE_RUNTIME_OPTIONS[0].value);
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ tone: "ok" | "bad"; text: string } | null>(null);
  const [created, setCreated] = useState<{ name: string; runtime: string } | null>(null);
  const [testMsg, setTestMsg] = useState<{ tone: "ok" | "bad"; text: string } | null>(null);
  const [testBusy, setTestBusy] = useState(false);

  const onSubmit = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      setMsg(null);
      setTestMsg(null);
      setCreated(null);
      if (!name.trim() || !value) {
        setMsg({ tone: "bad", text: "Name and key are both required." });
        return;
      }
      setBusy(true);
      try {
        const res = await create(credentialCreateBody({ name, runtime, value, teamId }));
        const outcome = classifyCreateStatus(res.status);
        if (outcome === "created") {
          const body = await res.json().catch(() => ({}));
          const ref = typeof body?.secretRef === "string" ? body.secretRef : "";
          setMsg({
            tone: "ok",
            text: ref ? `Credential stored — ${ref}` : "Credential stored.",
          });
          setCreated({ name: name.trim(), runtime });
          setValue(""); // never keep the pasted key in state longer than the write
          onCreated();
        } else {
          setMsg({ tone: "bad", text: await createErrorText(outcome, res) });
        }
      } catch {
        setMsg({ tone: "bad", text: "The credential store is unreachable — try again." });
      } finally {
        setBusy(false);
      }
    },
    [name, runtime, value, teamId, create, onCreated],
  );

  const onTest = useCallback(async () => {
    if (!created) return;
    setTestBusy(true);
    setTestMsg(null);
    try {
      const res = await test(created.name, {
        runtime: created.runtime,
        teamId: teamId || undefined,
      });
      if (res.status >= 200 && res.status < 300) {
        const body = await res.json().catch(() => ({}));
        const ok = body?.ok === true;
        const detail = typeof body?.detail === "string" ? body.detail : "";
        setTestMsg({
          tone: ok ? "ok" : "bad",
          text: ok ? `Connected${detail ? ` — ${detail}` : ""}` : detail || "The endpoint declined the credential.",
        });
      } else if (res.status === 501) {
        setTestMsg({ tone: "bad", text: "This credential type can't be tested yet." });
      } else {
        setTestMsg({ tone: "bad", text: "Couldn't reach the test probe — try again." });
      }
    } catch {
      setTestMsg({ tone: "bad", text: "Couldn't reach the test probe — try again." });
    } finally {
      setTestBusy(false);
    }
  }, [created, test, teamId]);

  return (
    <form className="creds__form" data-testid="creds-create-form" onSubmit={onSubmit}>
      <h2 className="creds__form-title">Add a credential</h2>
      <p className="muted creds__form-lede">
        Bring your own provider key — the v1 path. KSquad stores it as a per-team Kubernetes Secret
        and never shows it back. Human-seat OAuth uses Connect Claude above (coming soon).
      </p>
      <div className="creds__form-grid">
        <label className="creds__field">
          <span>Name</span>
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. team-anthropic"
            data-testid="creds-name"
            autoComplete="off"
          />
        </label>
        <label className="creds__field">
          <span>Runtime</span>
          <select
            value={runtime}
            onChange={(e) => setRuntime(e.target.value)}
            data-testid="creds-runtime"
          >
            {CREATE_RUNTIME_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </label>
        <label className="creds__field creds__field--wide">
          <span>Service-account key</span>
          <input
            type="password"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="Paste the provider API key"
            data-testid="creds-value"
            autoComplete="off"
          />
        </label>
      </div>
      <div className="creds__form-actions">
        <button
          type="submit"
          className="creds__form-submit"
          disabled={busy || !teamReady}
          data-testid="creds-submit"
        >
          {busy ? "Saving…" : "Save credential"}
        </button>
        {!teamReady && (
          <span className="muted" data-testid="creds-team-required">
            Pick a team above first.
          </span>
        )}
        {created && (
          <button
            type="button"
            className="creds__form-test"
            onClick={onTest}
            disabled={testBusy}
            data-testid="creds-test"
          >
            {testBusy ? "Testing…" : "Test connection"}
          </button>
        )}
      </div>
      {msg && (
        <p
          className={`creds__form-msg creds__form-msg--${msg.tone}`}
          data-testid="creds-create-msg"
          data-tone={msg.tone}
          role="status"
        >
          {msg.text}
        </p>
      )}
      {testMsg && (
        <p
          className={`creds__form-msg creds__form-msg--${testMsg.tone}`}
          data-testid="creds-test-msg"
          data-tone={testMsg.tone}
          role="status"
        >
          {testMsg.text}
        </p>
      )}
    </form>
  );
}

/** Honest, non-leaking copy for each create failure. Field detail for 422 is the apiserver's own
 *  field/message list (it never echoes the value); everything else stays generic. */
async function createErrorText(
  outcome: ReturnType<typeof classifyCreateStatus>,
  res: Response,
): Promise<string> {
  switch (outcome) {
    case "select-team":
      return "Pick a team for this credential first.";
    case "conflict":
      return "A credential with this name already exists in this team.";
    case "unsupported":
      return "Human-seat OAuth isn't pasted — use Connect Claude for that (coming soon).";
    case "denied":
      return "You don't have access to write credentials for this team.";
    case "invalid": {
      const body = await res.json().catch(() => ({}));
      const fields = Array.isArray(body?.fields) ? body.fields : [];
      if (fields.length) {
        return fields
          .map((f: { field?: string; message?: string }) => `${f.field ?? "field"}: ${f.message ?? "invalid"}`)
          .join("; ");
      }
      return "The credential was rejected — check the name, runtime, and key.";
    }
    default:
      return "The credential store rejected the write — try again.";
  }
}

async function defaultLoad(teamId?: string): Promise<Response> {
  const q = teamId ? `?teamId=${encodeURIComponent(teamId)}` : "";
  return fetch(`/api/credentials${q}`, { cache: "no-store" });
}

async function defaultLoadTeams(): Promise<Response> {
  return fetch("/api/squad/teams", { headers: { accept: "application/json" }, cache: "no-store" });
}

async function defaultCreate(body: Record<string, string>): Promise<Response> {
  return fetch("/api/credentials", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

async function defaultTest(
  name: string,
  body: { runtime: string; teamId?: string },
): Promise<Response> {
  return fetch(`/api/credentials/${encodeURIComponent(name)}/test`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

async function defaultConnect(): Promise<Response> {
  return fetch("/api/credentials/connect", { method: "POST" });
}
