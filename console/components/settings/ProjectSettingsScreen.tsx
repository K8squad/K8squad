"use client";

// components/settings/ProjectSettingsScreen.tsx — ISI-4000 S2: the project
// Settings tab.
//
// Renders the S1 read projection (GET /api/projects/{id}/settings) and, for a
// viewer with canEdit, lets an operator (a) set the repo URL + tracked ref,
// (b) set/replace the SCM PAT, and (c) test the connection — composing ONLY
// endpoints that already exist (compose PUT + POST /api/credentials + repo-auth
// test). No new cluster write. Honesty rules, top to bottom:
//   • The credential status is a tri-state badge derived STRICTLY off
//     auth.{connected,lastTest} — a ref's presence alone never reads "healthy".
//   • The PAT is write-only: it is POSTed once, referenced by NAME, and the input
//     clears on submit — the token is never read back, never rendered.
//   • A write is a FULL-SPEC compose PUT, so we round-trip the whole authoring
//     detail (goals/egress preserved) via buildProjectPutBody — a repo save never
//     silently wipes a project's goals.
//   • Every terminal state is honest: loading / 401 / 404 / 501 (not wired) /
//     no-repo / 5xx get distinct frames; a validation 422 surfaces the
//     apiserver's field error VERBATIM; the repo-auth {ok,detail} renders VERBATIM
//     (an honest red is a valid outcome, never rewritten to success).
//   • canEdit gates the UI (defense in depth); the apiserver still gates the write.

import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { EmptyState } from "@/components/forms/EmptyState";
import { classifyCreateStatus } from "@/lib/credentials";
import {
  buildProjectPutBody,
  createScmCredential,
  credentialStatusLabel,
  fetchProjectDetail,
  fetchProjectSettings,
  putProject,
  testRepoAuth,
  SCM_PAT_SECRET_KEY,
  type ProjectSettings,
  type ProjectSettingsState,
  type RepoAuthTestResult,
} from "@/lib/projectSettings";

type Msg = { tone: "ok" | "bad"; text: string };

/** Parse the compose error body ({error?, fields?}) into a single verbatim line
 *  (AC3: the apiserver's field message, never a generic toast). */
async function composeErrorText(res: Response): Promise<string> {
  try {
    const j = (await res.json()) as { error?: string; fields?: Array<{ field: string; message: string }> };
    if (Array.isArray(j.fields) && j.fields.length) {
      return j.fields.map((f) => `${f.field}: ${f.message}`).join("; ");
    }
    if (j.error) return j.error;
  } catch {
    /* non-JSON */
  }
  if (res.status === 401) return "You must sign in to change these settings.";
  if (res.status === 403) return "You don't have write access to this project.";
  if (res.status === 409) return "Concurrent modification — reload and retry.";
  if (res.status === 501) return "Compose is not available on this deployment yet.";
  return `Save failed (status ${res.status}).`;
}

export function ProjectSettingsScreen({ projectId }: { projectId: string }) {
  const [state, setState] = useState<ProjectSettingsState>({ kind: "loading" });

  const load = useCallback(async () => {
    try {
      setState(await fetchProjectSettings(projectId));
    } catch {
      setState({ kind: "error", status: 0 });
    }
  }, [projectId]);

  useEffect(() => {
    void load();
  }, [load]);

  if (state.kind === "loading") {
    return (
      <section className="settings" data-testid="settings-loading">
        <div className="card skeleton" aria-busy="true">
          Loading project settings…
        </div>
      </section>
    );
  }
  if (state.kind === "unauthenticated") {
    return (
      <section className="settings">
        <EmptyState
          testId="settings-unauthenticated"
          title="Sign in to view settings"
          why="Your session isn't authenticated for this project."
        />
      </section>
    );
  }
  if (state.kind === "not-found") {
    return (
      <section className="settings">
        <EmptyState
          testId="settings-not-found"
          title="Project not found"
          why="No such project in your scope — or you don't have access to it."
        />
      </section>
    );
  }
  if (state.kind === "not-wired") {
    return (
      <section className="settings">
        <EmptyState
          testId="settings-not-wired"
          title="Settings not available yet"
          why="This deployment hasn't wired the project-settings read model (S1). It will appear here once enabled."
        />
      </section>
    );
  }
  if (state.kind === "error") {
    return (
      <section className="settings">
        <EmptyState
          testId="settings-error"
          title="Couldn't load settings"
          why={`The settings read model returned an error${state.status ? ` (HTTP ${state.status})` : ""}. Try again.`}
          ctaLabel="Retry"
          onCta={() => void load()}
        />
      </section>
    );
  }

  return <ReadyView data={state.data} projectId={projectId} onReload={load} />;
}

function ReadyView({
  data,
  projectId,
  onReload,
}: {
  data: ProjectSettings;
  projectId: string;
  onReload: () => Promise<void>;
}) {
  const { repo, auth, canEdit } = data;
  const badge = useMemo(() => credentialStatusLabel(auth.connected, auth.lastTest), [auth]);
  const hasRepo = repo.url.trim().length > 0;

  return (
    <section className="settings" data-testid="settings-ready">
      <header className="settings__header">
        <h1 className="settings__title">Settings</h1>
        <p className="muted">{data.project.namespace}/{data.project.name}</p>
      </header>

      {/* Repo panel — always read; edit affordances only when canEdit (AC1/AC6). */}
      <div className="card settings__panel" data-testid="settings-repo-panel">
        <h2>Repository</h2>
        {hasRepo ? (
          <dl className="settings__facts">
            <div>
              <dt>Repo URL</dt>
              <dd data-testid="repo-url">{repo.url}</dd>
            </div>
            <div>
              <dt>Tracked ref</dt>
              <dd data-testid="repo-ref">{repo.ref ? repo.ref : "default branch"}</dd>
            </div>
            <div>
              <dt>Provider</dt>
              <dd data-testid="repo-provider">{repo.provider}</dd>
            </div>
          </dl>
        ) : (
          <EmptyState
            testId="settings-no-repo"
            title="No repo connected"
            why="This project has no repository configured yet."
          />
        )}

        {/* Sync (v1 read-only) — show the truth without a half-built form. */}
        <p className="muted settings__sync" data-testid="repo-sync">
          Sync {repo.syncEnabled ? `enabled (poll ${repo.pollIntervalSeconds}s)` : "not configured"}
          {repo.reflectOutbound ? " · reflects outbound" : ""}
        </p>
      </div>

      {/* Credential status — honest tri-state (AC2). */}
      <div className="card settings__panel" data-testid="settings-cred-panel">
        <h2>SCM credential</h2>
        <span
          className={`settings__badge settings__badge--${badge.tone}`}
          data-testid="cred-status-badge"
          data-tone={badge.tone}
        >
          {badge.label}
        </span>
        {auth.connected && auth.credentialSecretRefName ? (
          <p className="muted" data-testid="cred-ref">
            Secret: {auth.credentialSecretRefName}
          </p>
        ) : null}
      </div>

      {canEdit ? (
        <EditPanels data={data} projectId={projectId} onReload={onReload} />
      ) : (
        <p className="muted settings__readonly" data-testid="settings-readonly-note">
          You don&apos;t have permission to change these settings.
        </p>
      )}
    </section>
  );
}

function EditPanels({
  data,
  projectId,
  onReload,
}: {
  data: ProjectSettings;
  projectId: string;
  onReload: () => Promise<void>;
}) {
  const { repo, auth } = data;
  const [repoUrl, setRepoUrl] = useState(repo.url);
  const [repoRef, setRepoRef] = useState(repo.ref);
  const [repoBusy, setRepoBusy] = useState(false);
  const [repoMsg, setRepoMsg] = useState<Msg | null>(null);

  const [credName, setCredName] = useState("");
  const [pat, setPat] = useState("");
  const [credBusy, setCredBusy] = useState(false);
  const [credMsg, setCredMsg] = useState<Msg | null>(null);

  const [testBusy, setTestBusy] = useState(false);
  const [testResult, setTestResult] = useState<RepoAuthTestResult | null>(null);

  // ── AC3: save repo URL/ref (full-spec round-trip preserves goals/egress). ────
  const onSaveRepo = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      setRepoMsg(null);
      if (!repoUrl.trim()) {
        setRepoMsg({ tone: "bad", text: "repo.url: is required" });
        return;
      }
      setRepoBusy(true);
      try {
        const detail = await fetchProjectDetail(projectId);
        if (!detail) {
          setRepoMsg({ tone: "bad", text: "Couldn't read the current project to save — try again." });
          return;
        }
        const body = buildProjectPutBody(detail, { repoUrl, repoRef });
        const res = await putProject(projectId, body);
        if (res.ok) {
          setRepoMsg({ tone: "ok", text: "Repository saved." });
          await onReload();
        } else {
          setRepoMsg({ tone: "bad", text: await composeErrorText(res) });
        }
      } catch {
        setRepoMsg({ tone: "bad", text: "The compose endpoint is unreachable — try again." });
      } finally {
        setRepoBusy(false);
      }
    },
    [projectId, repoUrl, repoRef, onReload],
  );

  // ── AC4: set/replace the SCM PAT, then reference it on the repo spec. ─────────
  const onSavePat = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      setCredMsg(null);
      if (!credName.trim() || !pat) {
        setCredMsg({ tone: "bad", text: "A credential name and a pasted token are both required." });
        return;
      }
      if (!repoUrl.trim()) {
        setCredMsg({ tone: "bad", text: "Set the repo URL before attaching a credential." });
        return;
      }
      setCredBusy(true);
      try {
        const credRes = await createScmCredential({ name: credName.trim(), value: pat });
        const outcome = classifyCreateStatus(credRes.status);
        // Clear the token from state the moment the write returns — never held longer.
        setPat("");
        if (outcome !== "created") {
          setCredMsg({ tone: "bad", text: credCreateErrorText(outcome) });
          return;
        }
        // Reference the new Secret on the repo spec (key pinned to the write's key).
        const detail = await fetchProjectDetail(projectId);
        if (!detail) {
          setCredMsg({ tone: "bad", text: "Credential stored, but couldn't read the project to attach it — retry." });
          return;
        }
        const body = buildProjectPutBody(detail, {
          repoUrl,
          repoRef,
          credentialSecretRef: { name: credName.trim(), key: SCM_PAT_SECRET_KEY },
        });
        const putRes = await putProject(projectId, body);
        if (putRes.ok) {
          setCredMsg({ tone: "ok", text: "Credential stored and attached." });
          setCredName("");
          await onReload();
        } else {
          setCredMsg({ tone: "bad", text: await composeErrorText(putRes) });
        }
      } catch {
        setCredMsg({ tone: "bad", text: "The credential store is unreachable — try again." });
      } finally {
        setCredBusy(false);
      }
    },
    [projectId, credName, pat, repoUrl, repoRef, onReload],
  );

  // ── AC5: test connection — render {ok,detail} VERBATIM. ──────────────────────
  const onTest = useCallback(async () => {
    setTestBusy(true);
    setTestResult(null);
    try {
      const result = await testRepoAuth(repo.url, {
        name: auth.credentialSecretRefName,
        key: SCM_PAT_SECRET_KEY,
      });
      setTestResult(result);
    } catch {
      setTestResult({ ok: false, detail: "Couldn't reach the test probe — try again." });
    } finally {
      setTestBusy(false);
    }
  }, [repo.url, auth.credentialSecretRefName]);

  const canTest = repo.url.trim().length > 0 && auth.connected;

  return (
    <>
      <form className="card settings__panel" data-testid="settings-repo-form" onSubmit={onSaveRepo}>
        <h2>Set repository</h2>
        <label className="settings__field">
          <span>Repo URL</span>
          <input
            type="text"
            value={repoUrl}
            onChange={(e) => setRepoUrl(e.target.value)}
            placeholder="https://github.com/org/repo"
            data-testid="repo-url-input"
            autoComplete="off"
          />
        </label>
        <label className="settings__field">
          <span>Tracked ref (optional)</span>
          <input
            type="text"
            value={repoRef}
            onChange={(e) => setRepoRef(e.target.value)}
            placeholder="default branch"
            data-testid="repo-ref-input"
            autoComplete="off"
          />
        </label>
        <button className="btn btn--primary" type="submit" disabled={repoBusy} data-testid="repo-save">
          {repoBusy ? "Saving…" : "Save repository"}
        </button>
        {repoMsg ? (
          <p className={`settings__msg settings__msg--${repoMsg.tone}`} data-testid="repo-msg" role="status">
            {repoMsg.text}
          </p>
        ) : null}
      </form>

      <form className="card settings__panel" data-testid="settings-pat-form" onSubmit={onSavePat}>
        <h2>{auth.connected ? "Replace SCM credential" : "Attach SCM credential"}</h2>
        <p className="muted">
          Paste a personal access token — it&apos;s stored as a Kubernetes Secret and never shown back.
        </p>
        <label className="settings__field">
          <span>Credential name</span>
          <input
            type="text"
            value={credName}
            onChange={(e) => setCredName(e.target.value)}
            placeholder="e.g. project-scm-pat"
            data-testid="pat-name-input"
            autoComplete="off"
          />
        </label>
        <label className="settings__field">
          <span>Personal access token</span>
          <input
            type="password"
            value={pat}
            onChange={(e) => setPat(e.target.value)}
            placeholder="Paste the PAT"
            data-testid="pat-value-input"
            autoComplete="off"
          />
        </label>
        <button className="btn btn--primary" type="submit" disabled={credBusy} data-testid="pat-save">
          {credBusy ? "Saving…" : "Save credential"}
        </button>
        {credMsg ? (
          <p className={`settings__msg settings__msg--${credMsg.tone}`} data-testid="pat-msg" role="status">
            {credMsg.text}
          </p>
        ) : null}
      </form>

      <div className="card settings__panel" data-testid="settings-test-panel">
        <h2>Test connection</h2>
        <button
          className="btn"
          type="button"
          onClick={() => void onTest()}
          disabled={testBusy || !canTest}
          data-testid="test-connection"
        >
          {testBusy ? "Testing…" : "Test connection"}
        </button>
        {!canTest ? (
          <p className="muted" data-testid="test-hint">
            Connect a credential and set a repo URL to test.
          </p>
        ) : null}
        {testResult ? (
          <p
            className={`settings__msg settings__msg--${testResult.ok ? "ok" : "bad"}`}
            data-testid="test-result"
            role="status"
          >
            {testResult.detail}
          </p>
        ) : null}
      </div>
    </>
  );
}

/** Map a credential-create outcome to honest copy (the value never appears). */
function credCreateErrorText(
  outcome: ReturnType<typeof classifyCreateStatus>,
): string {
  switch (outcome) {
    case "select-team":
      return "Select a team before storing this credential.";
    case "conflict":
      return "A credential with this name already exists — choose a different name.";
    case "invalid":
      return "The credential name or token is invalid.";
    case "denied":
      return "You don't have permission to store a credential here.";
    case "unsupported":
      return "This credential type can't be stored from here.";
    default:
      return "The credential store is unhappy — try again.";
  }
}
