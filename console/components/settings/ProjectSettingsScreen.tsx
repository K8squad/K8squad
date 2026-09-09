"use client";

// components/settings/ProjectSettingsScreen.tsx — the project Settings tab (ISI-4000 / S2).
//
// Renders S1's read projection (GET /api/projects/{id}/settings) and, for an authorized operator
// (canEdit), lets them (a) view/set the repo URL + tracked ref, (b) set/replace the SCM PAT, and
// (c) test the connection — all by composing EXISTING endpoints (see lib/projectSettings.ts). The
// token is WRITE-ONLY: it is POSTed once, never read back, and the field clears on submit. Every
// non-happy read state gets an HONEST frame (loading / unauthenticated / not-found / not-wired
// 501 / error / no-repo) — never a blank page, never fabricated values. canEdit gates the UI as
// defense-in-depth; the apiserver's write-tier gate stays the real wall.

import { useCallback, useEffect, useState } from "react";
import { EmptyState } from "@/components/forms/EmptyState";
import { Field } from "@/components/compose/fields";
import {
  credentialStatusLabel,
  fetchProjectAuthoring,
  fetchProjectSettings,
  mergeProjectWrite,
  postScmCredential,
  putProjectCompose,
  scmSecretName,
  SCM_PAT_SECRET_KEY,
  testRepoAuth,
  type ProjectSettings,
  type RepoAuthTestResult,
  type SettingsState,
} from "@/lib/projectSettings";

/** A base36 timestamp suffix for a fresh SCM Secret name (create-only write ⇒ unique per set). */
function freshSuffix(): string {
  return Date.now().toString(36);
}

export function ProjectSettingsScreen({ projectId }: { projectId: string }) {
  const [state, setState] = useState<SettingsState>({ kind: "loading" });

  // Repo form (seeded from the projection once it is ready; canEdit gates rendering).
  const [repoUrl, setRepoUrl] = useState("");
  const [repoRef, setRepoRef] = useState("");
  const [seeded, setSeeded] = useState(false);

  // Write-only PAT field.
  const [pat, setPat] = useState("");

  // Per-action status.
  const [savingRepo, setSavingRepo] = useState(false);
  const [savingPat, setSavingPat] = useState(false);
  const [testing, setTesting] = useState(false);
  const [repoErrors, setRepoErrors] = useState<Record<string, string>>({});
  const [notice, setNotice] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<RepoAuthTestResult | null>(null);

  const load = useCallback(async () => {
    setState({ kind: "loading" });
    try {
      setState(await fetchProjectSettings(projectId));
    } catch {
      setState({ kind: "error", status: 0 });
    }
  }, [projectId]);

  useEffect(() => {
    void load();
  }, [load]);

  // Seed the editable inputs from the projection exactly once (never re-clobber operator edits).
  useEffect(() => {
    if (!seeded && state.kind === "ready") {
      setRepoUrl(state.data.repo.url);
      setRepoRef(state.data.repo.ref);
      setSeeded(true);
    }
  }, [seeded, state]);

  const reload = useCallback(async () => {
    // Re-read after a mutating success WITHOUT re-seeding the form (the operator's edits stand).
    try {
      setState(await fetchProjectSettings(projectId));
    } catch {
      setState({ kind: "error", status: 0 });
    }
  }, [projectId]);

  const saveRepo = useCallback(async () => {
    setSavingRepo(true);
    setRepoErrors({});
    setNotice(null);
    try {
      const base = await fetchProjectAuthoring(projectId);
      if (!base) {
        setNotice("Couldn’t read the current project spec to save safely — try again.");
        return;
      }
      const body = mergeProjectWrite(base, { url: repoUrl, ref: repoRef });
      const out = await putProjectCompose(projectId, body);
      if (out.ok) {
        setNotice("Repository updated.");
        await reload();
      } else if (out.fields) {
        setRepoErrors(out.fields);
      } else if (out.status === 409) {
        setNotice("The project changed while you were editing — reload and retry.");
      } else {
        setNotice(`Save failed (HTTP ${out.status}).`);
      }
    } catch {
      setNotice("Save failed — the console couldn’t reach the server.");
    } finally {
      setSavingRepo(false);
    }
  }, [projectId, repoUrl, repoRef, reload]);

  const savePat = useCallback(async () => {
    if (!pat.trim()) return;
    setSavingPat(true);
    setRepoErrors({});
    setNotice(null);
    try {
      const name = scmSecretName(projectId, freshSuffix());
      const cred = await postScmCredential(name, pat);
      if (!cred.ok) {
        if (cred.fields?.["value"] || cred.fields?.["name"]) {
          setNotice(cred.fields.value ?? cred.fields.name);
        } else {
          setNotice(`Couldn’t store the token (HTTP ${cred.status}).`);
        }
        return;
      }
      // Repoint spec.repo.auth at the just-written Secret (key contract: apiKey), preserving the
      // rest of the authoring spec.
      const base = await fetchProjectAuthoring(projectId);
      if (!base) {
        setNotice("Token stored, but the project spec couldn’t be read to link it — try again.");
        return;
      }
      const body = mergeProjectWrite(base, {
        credentialSecretRef: { name: cred.secretName ?? name, key: SCM_PAT_SECRET_KEY },
      });
      const out = await putProjectCompose(projectId, body);
      if (out.ok) {
        setPat(""); // write-only: clear the field, never echo it back
        setNotice("Token saved and linked to the repository.");
        await reload();
      } else {
        setNotice(`Token stored, but linking it failed (HTTP ${out.status}).`);
      }
    } catch {
      setNotice("Couldn’t save the token — the console couldn’t reach the server.");
    } finally {
      setSavingPat(false);
    }
  }, [projectId, pat, reload]);

  const runTest = useCallback(
    async (settings: ProjectSettings) => {
      setTesting(true);
      setTestResult(null);
      try {
        const res = await testRepoAuth(settings.repo.url, {
          name: settings.auth.credentialSecretRefName,
          key: SCM_PAT_SECRET_KEY,
        });
        setTestResult(res);
      } catch {
        setTestResult({ status: 0 });
      } finally {
        setTesting(false);
      }
    },
    [],
  );

  // ── Honest non-ready frames (AC7) ──────────────────────────────────────────
  if (state.kind === "loading") {
    return (
      <section className="settings" data-testid="settings-screen" data-state="loading">
        <p className="muted" data-testid="settings-loading">Loading settings…</p>
      </section>
    );
  }
  if (state.kind === "unauthenticated") {
    return (
      <FrameError testId="settings-unauthenticated" title="Sign in to view settings"
        why="Your session isn’t authenticated. Sign in and reopen this tab." onRetry={load} />
    );
  }
  if (state.kind === "not-found") {
    return (
      <FrameError testId="settings-not-found" title="Settings unavailable"
        why="This project isn’t visible to you, or it doesn’t exist." onRetry={load} />
    );
  }
  if (state.kind === "not-wired") {
    return (
      <FrameError testId="settings-not-wired" title="Settings not available yet"
        why="The project settings read model isn’t wired in this deployment." onRetry={load} />
    );
  }
  if (state.kind === "error") {
    return (
      <FrameError testId="settings-error" title="Couldn’t load settings"
        why={`The server returned an error (HTTP ${state.status || "network"}).`} onRetry={load} />
    );
  }

  // ── Ready (AC1/2/3/4/5/6) ───────────────────────────────────────────────────
  const { repo, auth, canEdit } = state.data;
  const status = credentialStatusLabel(auth.connected, auth.lastTest);
  const hasRepo = repo.url.trim().length > 0;

  return (
    <section className="settings" data-testid="settings-screen" data-state="ready">
      <header className="settings__head">
        <h1>Settings</h1>
        <p className="muted">Repository &amp; source-control credential for this project.</p>
      </header>

      {notice && (
        <p className="settings__notice" role="status" data-testid="settings-notice">{notice}</p>
      )}

      {/* Repository panel (AC1 / AC3 / AC6). */}
      <div className="card settings__card" data-testid="settings-repo">
        <h2>Repository</h2>
        {!hasRepo && (
          <p className="muted" data-testid="settings-no-repo">
            No repository connected yet{canEdit ? " — set one below." : "."}
          </p>
        )}

        {canEdit ? (
          <div className="settings__form" data-testid="settings-repo-form">
            <Field label="Repository URL" hint="A github.com repository URL (v1)."
              error={repoErrors["repo.url"]}>
              <input
                type="url"
                value={repoUrl}
                onChange={(e) => setRepoUrl(e.target.value)}
                aria-invalid={!!repoErrors["repo.url"]}
                data-testid="settings-repo-url"
                placeholder="https://github.com/owner/repo"
              />
            </Field>
            <Field label="Tracked ref" hint="Branch or tag; leave empty for the default branch."
              error={repoErrors["repo.ref"]}>
              <input
                type="text"
                value={repoRef}
                onChange={(e) => setRepoRef(e.target.value)}
                data-testid="settings-repo-ref"
                placeholder="default branch"
              />
            </Field>
            <button
              type="button"
              className="btn btn--primary"
              onClick={saveRepo}
              disabled={savingRepo}
              data-testid="settings-repo-save"
            >
              {savingRepo ? "Saving…" : "Save repository"}
            </button>
          </div>
        ) : (
          <dl className="settings__facts" data-testid="settings-repo-readonly">
            <dt>URL</dt>
            <dd>{hasRepo ? <code>{repo.url}</code> : <span className="muted">—</span>}</dd>
            <dt>Tracked ref</dt>
            <dd>{repo.ref ? <code>{repo.ref}</code> : <span className="muted">default branch</span>}</dd>
            <dt>Provider</dt>
            <dd>{repo.provider}</dd>
          </dl>
        )}
      </div>

      {/* SCM credential panel (AC2 / AC4). */}
      <div className="card settings__card" data-testid="settings-credential">
        <h2>Source-control credential</h2>
        <p className="settings__status" data-testid="settings-cred-status" data-tone={status.tone}>
          <span className={`settings__badge settings__badge--${status.tone}`}>{status.label}</span>
        </p>

        {canEdit && (
          <div className="settings__form" data-testid="settings-pat-form">
            <Field
              label={auth.connected ? "Replace personal access token" : "Personal access token"}
              hint="Pasted once and stored write-only — it is never shown again."
            >
              <input
                type="password"
                value={pat}
                onChange={(e) => setPat(e.target.value)}
                autoComplete="off"
                data-testid="settings-pat-input"
                placeholder="ghp_…"
              />
            </Field>
            <button
              type="button"
              className="btn btn--primary"
              onClick={savePat}
              disabled={savingPat || !pat.trim()}
              data-testid="settings-pat-save"
            >
              {savingPat ? "Saving…" : auth.connected ? "Replace token" : "Save token"}
            </button>
          </div>
        )}
      </div>

      {/* Test connection (AC5). Available whenever a repo + credential exist. */}
      {hasRepo && auth.connected && (
        <div className="card settings__card" data-testid="settings-test">
          <h2>Connection</h2>
          <button
            type="button"
            className="btn"
            onClick={() => runTest(state.data)}
            disabled={testing}
            data-testid="settings-test-btn"
          >
            {testing ? "Testing…" : "Test connection"}
          </button>
          {testResult && (
            testResult.status === 200 ? (
              <p
                className={`settings__test settings__test--${testResult.ok ? "ok" : "bad"}`}
                role="status"
                data-testid="settings-test-result"
                data-ok={testResult.ok ? "true" : "false"}
              >
                {/* Verbatim {ok, detail} — an honest red is a valid outcome, never rewritten. */}
                {testResult.detail ?? (testResult.ok ? "Connection OK." : "Connection failed.")}
              </p>
            ) : (
              <p className="settings__test settings__test--bad" role="status"
                data-testid="settings-test-error">
                Couldn’t run the test (HTTP {testResult.status || "network"}).
              </p>
            )
          )}
        </div>
      )}

      {/* Sync is read-only in v1 (a later slice owns editing). Shown so the operator sees the truth. */}
      <div className="card settings__card" data-testid="settings-sync">
        <h2>Sync</h2>
        <dl className="settings__facts">
          <dt>Enabled</dt>
          <dd>{repo.syncEnabled ? "Yes" : "No"}</dd>
          {repo.syncEnabled && (
            <>
              <dt>Poll interval</dt>
              <dd>{repo.pollIntervalSeconds ? `${repo.pollIntervalSeconds}s` : "—"}</dd>
              <dt>Reflect outbound</dt>
              <dd>{repo.reflectOutbound ? "Yes" : "No"}</dd>
            </>
          )}
        </dl>
        <p className="muted settings__hint">Sync configuration is managed elsewhere in v1.</p>
      </div>

      {!canEdit && (
        <p className="muted" data-testid="settings-readonly-note">
          You don’t have permission to change these settings.
        </p>
      )}
    </section>
  );
}

/** A honest error/empty frame with a retry affordance (reuses the shared EmptyState, ISI-3686). */
function FrameError({
  testId,
  title,
  why,
  onRetry,
}: {
  testId: string;
  title: string;
  why: string;
  onRetry: () => void;
}) {
  return (
    <section className="settings" data-testid="settings-screen">
      <EmptyState title={title} why={why} ctaLabel="Retry" onCta={onRetry} testId={testId} />
    </section>
  );
}
