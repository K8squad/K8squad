"use client";

// components/settings/ProjectSettingsScreen.tsx — ISI-4000 S2: the project
// Settings tab. ISI-4830 redesign (S1–S4): a real settings IA over the SAME
// read/compose surface — no behaviour change, no new backend.
//
// Layout (§2 of DESIGN-SPEC-ISI-4830): project header → section nav (left) →
// concern cards (middle) → connection-health rail (right). The flat vertical
// stack of five disconnected cards (repo read + duplicate repo form + lone badge
// + orphan replace-form + orphan test button) is collapsed into three concerns:
//   • RepositoryCard — unified read+edit (kills the duplicate URL/ref fields).
//   • AccessCredentialCard — status strip (badge + secret + Test) + Replace-token,
//     one concern instead of three cards.
//   • ConnectionHealthRail — a pure re-projection of the S1 payload; no new call.
//
// Honesty rules are UNCHANGED (ISI-4000): the credential badge is derived strictly
// off auth.{connected,lastTest}; the PAT is write-only (POSTed once, referenced by
// name, input cleared on submit); a write is a full-spec compose PUT that
// round-trips goals/egress; every terminal state has its own honest frame; repo-auth
// {ok,detail} renders VERBATIM; canEdit gates the UI (apiserver still gates the write).
//
// Deferred to follow-up stories (§4): editable Sync (needs a compose-PUT sync seam,
// ISI-4843) and the Danger zone (needs backend delete endpoints, ISI-4844). Sync is
// shown read-only in the rail, matching what the backend can actually round-trip today.

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

/** The left-rail section anchors — id must match each card's id for scroll-spy. */
const SECTIONS = [
  { id: "repository", label: "Repository" },
  { id: "access", label: "Access & credentials" },
  { id: "sync", label: "Sync" },
] as const;

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
  const { project } = data;

  return (
    <section className="settings settings--redesign" data-testid="settings-ready">
      <header className="settings__header">
        <div className="settings__crumb">
          <span className="settings__avatar" aria-hidden="true">
            {(project.name[0] ?? "?").toUpperCase()}
          </span>
          <div>
            <h1 className="settings__title">Settings</h1>
            <p className="muted settings__subtitle" data-testid="settings-project-slug">
              {project.namespace}/{project.name}
            </p>
          </div>
        </div>
      </header>

      <div className="settings__grid">
        <nav className="settings__nav" aria-label="Settings sections">
          {SECTIONS.map((s, i) => (
            <a
              key={s.id}
              href={`#settings-${s.id}`}
              className={`settings__nav-link${i === 0 ? " settings__nav-link--active" : ""}`}
              data-testid={`settings-nav-${s.id}`}
            >
              {s.label}
            </a>
          ))}
        </nav>

        <div className="settings__main">
          <RepositoryAndCredentials data={data} projectId={projectId} onReload={onReload} />
        </div>

        <aside className="settings__rail" aria-label="Connection health">
          <ConnectionHealthRail data={data} />
        </aside>
      </div>
    </section>
  );
}

/** The two editable concerns (Repository, Access & credentials) plus the read-only
 *  Sync card. Split out so the credential badge/state lives beside its Test button
 *  and Replace-token form — one concern, one card (§2). */
function RepositoryAndCredentials({
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
      {/* ── Repository — unified read + edit (kills the duplicate URL/ref). ── */}
      <section id="settings-repository" className="card settings__card" data-testid="settings-repo-panel">
        <div className="settings__card-head">
          <h2 className="settings__card-title">Repository</h2>
          {hasRepo ? (
            <span className="settings__chip settings__chip--ok" data-testid="repo-linked-chip">
              Linked
            </span>
          ) : null}
        </div>

        {canEdit ? (
          <form className="settings__form" data-testid="settings-repo-form" onSubmit={onSaveRepo}>
            {!hasRepo ? (
              <EmptyState
                testId="settings-no-repo"
                title="No repo connected"
                why="This project has no repository configured yet — add one below."
              />
            ) : null}
            <label className="settings__field">
              <span>Repository URL</span>
              <input
                type="text"
                value={repoUrl}
                onChange={(e) => setRepoUrl(e.target.value)}
                placeholder="https://github.com/org/repo"
                data-testid="repo-url-input"
                autoComplete="off"
              />
            </label>
            <div className="settings__field-row">
              <label className="settings__field">
                <span>Tracked ref</span>
                <input
                  type="text"
                  value={repoRef}
                  onChange={(e) => setRepoRef(e.target.value)}
                  placeholder="default branch"
                  data-testid="repo-ref-input"
                  autoComplete="off"
                />
              </label>
              <div className="settings__field settings__field--static">
                <span>Provider</span>
                <span className="settings__chip settings__chip--muted" data-testid="repo-provider">
                  {repo.provider}
                </span>
              </div>
            </div>
            <p className="muted settings__note">
              Full spec is round-tripped on save — changing the URL never wipes the project&apos;s goals or egress.
            </p>
            <div className="settings__actions">
              <button className="btn btn--primary" type="submit" disabled={repoBusy} data-testid="repo-save">
                {repoBusy ? "Saving…" : "Save repository"}
              </button>
              {repoMsg ? (
                <span className={`settings__msg settings__msg--${repoMsg.tone}`} data-testid="repo-msg" role="status">
                  {repoMsg.text}
                </span>
              ) : null}
            </div>
          </form>
        ) : hasRepo ? (
          <dl className="settings__facts">
            <div>
              <dt>Repository URL</dt>
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
      </section>

      {/* ── Access & credentials — status strip + Replace-token (one concern). ── */}
      <section id="settings-access" className="card settings__card" data-testid="settings-cred-panel">
        <div className="settings__card-head">
          <h2 className="settings__card-title">Access &amp; credentials</h2>
        </div>

        <div className="settings__cred-strip" data-testid="settings-cred-strip">
          <span
            className={`settings__badge settings__badge--${badge.tone}`}
            data-testid="cred-status-badge"
            data-tone={badge.tone}
          >
            {badge.label}
          </span>
          {auth.connected && auth.credentialSecretRefName ? (
            <span className="settings__chip settings__chip--muted" data-testid="cred-ref">
              Secret: {auth.credentialSecretRefName}
            </span>
          ) : null}
          <button
            className="btn"
            type="button"
            onClick={() => void onTest()}
            disabled={testBusy || !canTest || !canEdit}
            data-testid="test-connection"
          >
            {testBusy ? "Testing…" : "Test connection"}
          </button>
        </div>
        {!canTest ? (
          <p className="muted settings__note" data-testid="test-hint">
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

        {canEdit ? (
          <form className="settings__form settings__replace" data-testid="settings-pat-form" onSubmit={onSavePat}>
            <h3 className="settings__sub-title">{auth.connected ? "Replace token" : "Attach token"}</h3>
            <p className="muted settings__note">
              Paste a personal access token — it&apos;s stored as a Kubernetes Secret and never shown back.
            </p>
            <div className="settings__field-row">
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
            </div>
            <div className="settings__actions">
              <button className="btn btn--primary" type="submit" disabled={credBusy} data-testid="pat-save">
                {credBusy ? "Saving…" : "Save credential"}
              </button>
              {credMsg ? (
                <span className={`settings__msg settings__msg--${credMsg.tone}`} data-testid="pat-msg" role="status">
                  {credMsg.text}
                </span>
              ) : null}
            </div>
          </form>
        ) : (
          <p className="muted settings__readonly" data-testid="settings-readonly-note">
            You don&apos;t have permission to change these settings.
          </p>
        )}
      </section>

      {/* ── Sync — read-only in v1 (editable write is ISI-4843, needs a PUT seam). ── */}
      <section id="settings-sync" className="card settings__card" data-testid="settings-sync-panel">
        <div className="settings__card-head">
          <h2 className="settings__card-title">Sync</h2>
          <span
            className={`settings__chip settings__chip--${repo.syncEnabled ? "ok" : "muted"}`}
            data-testid="sync-state-chip"
          >
            {repo.syncEnabled ? "On" : "Off"}
          </span>
        </div>
        <p className="muted settings__note" data-testid="repo-sync">
          {repo.syncEnabled
            ? `Polling every ${repo.pollIntervalSeconds}s`
            : "Not configured"}
          {repo.reflectOutbound ? " · reflects outbound" : ""}
        </p>
      </section>
    </>
  );
}

/** ConnectionHealthRail — a pure re-projection of the S1 settings payload (no new
 *  endpoint). At-a-glance status-coloured readout of the whole connection (§2). */
function ConnectionHealthRail({ data }: { data: ProjectSettings }) {
  const { repo, auth } = data;
  const badge = credentialStatusLabel(auth.connected, auth.lastTest);
  const hasRepo = repo.url.trim().length > 0;

  const lastTestTone =
    auth.lastTest === "passed" ? "ok" : auth.lastTest === "failed" ? "bad" : "muted";
  const lastTestLabel =
    auth.lastTest === "passed"
      ? "Passed"
      : auth.lastTest === "failed"
        ? "Failed"
        : "Not run";

  const rows: Array<{ label: string; value: string; tone: string; testId: string }> = [
    {
      label: "Repository",
      value: hasRepo ? "Linked" : "Not linked",
      tone: hasRepo ? "ok" : "muted",
      testId: "health-repo",
    },
    { label: "Credential", value: badge.label, tone: badge.tone, testId: "health-cred" },
    { label: "Last test", value: lastTestLabel, tone: lastTestTone, testId: "health-test" },
    {
      label: "Sync",
      value: repo.syncEnabled ? `Every ${repo.pollIntervalSeconds}s` : "Off",
      tone: repo.syncEnabled ? "ok" : "muted",
      testId: "health-sync",
    },
    { label: "Provider", value: repo.provider, tone: "muted", testId: "health-provider" },
  ];

  return (
    <div className="card settings__card settings__health" data-testid="settings-health-rail">
      <h2 className="settings__card-title">Connection health</h2>
      <dl className="settings__health-list">
        {rows.map((r) => (
          <div key={r.testId} className="settings__health-row">
            <dt>{r.label}</dt>
            <dd className={`settings__health-value settings__health-value--${r.tone}`} data-testid={r.testId}>
              <span className={`settings__dot settings__dot--${r.tone}`} aria-hidden="true" />
              {r.value}
            </dd>
          </div>
        ))}
      </dl>
    </div>
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
