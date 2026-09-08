"use client";

// components/compose/ComposeScreen.tsx — the Compose surface (E5-S1 re-shell, ISI-3685).
//
// Three-pane expert authoring surface: tab bar (Teams/Projects/Agents/Roles/Skills) replaces the
// kind <select> (AC1/FR-7.1); object list left, form center, live CRD YAML preview right (AC2/FR-7.3);
// guidance strip derives next-missing from the onboarding progress read-model (AC3/FR-7.2/AD-2);
// 403/409/422/501 surfaced VERBATIM with recovery CTA (AC4/FR-7.4/NFR-5).

import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "next/navigation";
import { Field } from "./fields";
import {
  COMPOSE_KINDS,
  KIND_LABEL,
  RUNTIME_CLASS_HINTS,
  emptyForm,
  isValid,
  parseComposeParams,
  toWire,
  validate,
  type ComposeForm,
  type ComposeKind,
  type ComposeMode,
  type ComposeResult,
  type FieldErrors,
} from "@/lib/compose";
import { TeamForm } from "./TeamForm";
import { ProjectForm } from "./ProjectForm";
import { AgentForm } from "./AgentForm";

type Mode = ComposeMode;

type SubmitState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "ok"; result: ComposeResult }
  | { kind: "error"; status: number; message: string; fields: FieldErrors };

/** Parse the apiserver error body into a message + any field-level errors (invariant 1 → 422). */
function parseError(status: number, body: string): { message: string; fields: FieldErrors } {
  const fields: FieldErrors = {};
  let message = `Apply failed (status ${status}).`;
  try {
    const j = JSON.parse(body) as {
      error?: string;
      fields?: Array<{ field: string; message: string }>;
    };
    if (j.error) message = j.error;
    if (Array.isArray(j.fields)) for (const f of j.fields) fields[f.field] = f.message;
  } catch {
    /* non-JSON (e.g. a bare 501 text) — keep the status message */
  }
  if (status === 401) message = "You must sign in to compose.";
  else if (status === 403) message = message || "You don't have write access here.";
  else if (status === 404) message = "No such project / team scope for this caller.";
  else if (status === 409) message = message || "That name already exists in this team.";
  else if (status === 501) message = "Compose is not available on this deployment yet.";
  return { message, fields };
}

/** Minimal YAML serializer for CRD wire objects (F-UI-1 — no new dep, handles the toWire shape). */
function yamlStringify(obj: unknown, indent = 0): string {
  const pad = "  ".repeat(indent);
  if (obj === null || obj === undefined) return "null";
  if (typeof obj === "boolean") return obj ? "true" : "false";
  if (typeof obj === "number") return String(obj);
  if (typeof obj === "string") {
    if (obj === "") return '""';
    if (/[\n:]/.test(obj) || obj.startsWith(" ") || obj.endsWith(" ")) {
      return `"${obj.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/\n/g, "\\n")}"`;
    }
    return obj;
  }
  if (Array.isArray(obj)) {
    if (obj.length === 0) return "[]";
    return obj.map((v) => `${pad}- ${yamlStringify(v, indent + 1).trimStart()}`).join("\n");
  }
  if (typeof obj === "object") {
    const entries = Object.entries(obj as Record<string, unknown>).filter(
      ([, v]) => v !== undefined && v !== null && v !== "",
    );
    if (entries.length === 0) return "{}";
    return entries
      .map(([k, v]) => {
        const val = yamlStringify(v, indent + 1);
        if (typeof v === "object" && v !== null && !Array.isArray(v)) {
          return `${pad}${k}:\n${val}`;
        }
        if (Array.isArray(v) && (v as unknown[]).length > 0) {
          return `${pad}${k}:\n${val}`;
        }
        return `${pad}${k}: ${val}`;
      })
      .join("\n");
  }
  return String(obj);
}

/** Onboarding progress shape from GET /api/onboarding/progress (E1-S1/AD-2). */
interface OnboardingProgress {
  step: number;
  done: number;
  total: number;
  nextMilestone: "team" | "agents" | "credentials" | "project" | "done";
}

/** Derive a guidance message from the progress read-model. */
function deriveGuidance(progress: OnboardingProgress | null): string | null {
  if (!progress) return null;
  if (progress.done >= progress.total) return null;
  switch (progress.nextMilestone) {
    case "team":
      return "No Team yet — create a Team first to namespace all other objects.";
    case "agents":
      return "Team exists but no agents yet — add a Boss, Implementer, and Manager agent.";
    case "credentials":
      return "Agents exist but no credentials — connect a model credential on each agent.";
    case "project":
      return "Almost ready — create a Project with a repo to complete setup.";
    default:
      return null;
  }
}

/** Derive a dependency nudge for the selected kind (AC3). */
function deriveNudge(kind: ComposeKind, progress: OnboardingProgress | null): string | null {
  if (!progress) return null;
  if (kind === "agents" && progress.nextMilestone === "team") {
    return "Create a Team first — agents require a team namespace.";
  }
  if (kind === "projects" && progress.done < 2) {
    return "Set up your agents and credentials before adding a Project.";
  }
  if ((kind === "roles" || kind === "skills") && progress.nextMilestone === "team") {
    return "Create a Team before adding Roles or Skills.";
  }
  return null;
}

// ── Left-pane list entries ────────────────────────────────────────────────────

interface ListEntry {
  name: string;
  subtitle?: string;
}

function useOrgList(kind: ComposeKind): { entries: ListEntry[]; loading: boolean } {
  const [entries, setEntries] = useState<ListEntry[]>([]);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    setEntries([]);
    setLoading(true);
    let cancelled = false;

    (async () => {
      try {
        if (kind === "agents") {
          // Fleet-aware Agents list (ISI-3985): admin ⇒ every squad, tenant ⇒ own Team. The old
          // `/api/teams/current/org` path passed the literal string "current" as the teamId — no
          // endpoint resolves it (teamScope: a tenant's UID never equals "current"; an admin has no
          // home team), so the list never populated and the pane sat empty ("skills not loading",
          // ISI-3985). This mirrors the teams/skills/roles kinds already migrated to /api/squad/*
          // (ISI-3941/3963/3964); agents was the one kind left behind.
          const res = await fetch("/api/squad/agents", { cache: "no-store" });
          if (res.ok) {
            const data = (await res.json()) as {
              agents?: Array<{ name: string; runtime?: string; skillCount?: number }> | null;
            };
            if (!cancelled) {
              setEntries(
                (data.agents ?? []).map((a) => ({
                  name: a.name,
                  subtitle:
                    typeof a.skillCount === "number"
                      ? `${a.skillCount} skill${a.skillCount === 1 ? "" : "s"}`
                      : a.runtime,
                })),
              );
            }
          }
        } else if (kind === "projects") {
          const res = await fetch("/api/projects", { cache: "no-store" });
          if (res.ok) {
            const data = (await res.json()) as Array<{ name?: string; metadata?: { name?: string } }>;
            if (!cancelled) {
              setEntries(
                (Array.isArray(data) ? data : []).map((p) => ({
                  name: p.name ?? p.metadata?.name ?? "—",
                })),
              );
            }
          }
        } else if (kind === "teams") {
          // Fleet-aware Teams list (ISI-3964): admin ⇒ every squad, tenant ⇒ own Team.
          const res = await fetch("/api/squad/teams", { cache: "no-store" });
          if (res.ok) {
            const data = (await res.json()) as {
              teams?: Array<{ name: string; namespace?: string }> | null;
            };
            if (!cancelled) {
              setEntries(
                (data.teams ?? []).map((t) => ({ name: t.name, subtitle: t.namespace })),
              );
            }
          }
        } else if (kind === "skills") {
          const res = await fetch("/api/squad/skills", { cache: "no-store" });
          if (res.ok) {
            const data = (await res.json()) as {
              skills?: Array<{ name: string; namespace?: string; sourceType?: string }> | null;
            };
            if (!cancelled) {
              setEntries(
                (data.skills ?? []).map((s) => ({
                  name: s.name,
                  subtitle: s.sourceType ?? s.namespace,
                })),
              );
            }
          }
        } else if (kind === "roles") {
          const res = await fetch("/api/squad/roles", { cache: "no-store" });
          if (res.ok) {
            const data = (await res.json()) as {
              roles?: Array<{ name: string; namespace?: string; prompt?: string }> | null;
            };
            if (!cancelled) {
              setEntries(
                (data.roles ?? []).map((r) => ({
                  name: r.name,
                  subtitle: r.prompt ?? r.namespace,
                })),
              );
            }
          }
        }
      } catch {
        /* silently ignore — left pane is informational */
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [kind]);

  return { entries, loading };
}

// ── Caller team scope (ISI-3964 compose gate) ─────────────────────────────────
//
// Compose create/edit is team-scoped server-side from the caller's AuthorContext. A caller with no
// resolvable OWN Team — a global admin (whose team_id backs no Team CR by design, ISI-3921) or a
// tenant whose Team is dangling — would submit a form that 404s. This hook reads the fleet-aware
// GET /api/squad/teams once and reports whether the caller has an own writable Team, so the form can
// be gated behind a "create your team first" prompt instead. (Full admin-write-into-any-team is
// Phase 3 / ISI-3955.) A tenant WITH a Team (fleet:false + ≥1 row) is the only "has own team" case;
// an admin response carries fleet:true (no home tenancy). Fails OPEN (unknown ⇒ show the form) so a
// transient error or an un-wired dev deployment never blocks composing.
type CallerScope =
  | { kind: "loading" }
  | { kind: "unknown" }
  | { kind: "resolved"; hasOwnTeam: boolean; fleet: boolean };

function useCallerScope(): CallerScope {
  const [scope, setScope] = useState<CallerScope>({ kind: "loading" });

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await fetch("/api/squad/teams", { cache: "no-store" });
        if (!res.ok) {
          if (!cancelled) setScope({ kind: "unknown" });
          return;
        }
        const data = (await res.json()) as {
          teams?: Array<unknown> | null;
          fleet?: boolean;
        };
        const fleet = data.fleet ?? false;
        const count = (data.teams ?? []).length;
        if (!cancelled) {
          setScope({ kind: "resolved", fleet, hasOwnTeam: !fleet && count >= 1 });
        }
      } catch {
        if (!cancelled) setScope({ kind: "unknown" });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  return scope;
}

// ── Tab bar ──────────────────────────────────────────────────────────────────

const ADVANCED_KINDS: ComposeKind[] = ["roles", "skills"];

function ComposeTabs({
  active,
  onSelect,
}: {
  active: ComposeKind;
  onSelect: (k: ComposeKind) => void;
}) {
  return (
    <div className="compose__tabs" role="tablist" aria-label="Compose kind">
      {COMPOSE_KINDS.map((k) => {
        const isAdv = ADVANCED_KINDS.includes(k);
        return (
          <button
            key={k}
            role="tab"
            type="button"
            aria-selected={k === active}
            className={`compose__tab${k === active ? " compose__tab--active" : ""}${isAdv ? " compose__tab--adv" : ""}`}
            onClick={() => onSelect(k)}
          >
            {KIND_LABEL[k]}
            {isAdv && <span className="compose__tab-badge">adv</span>}
          </button>
        );
      })}
    </div>
  );
}

// ── Guidance strip ───────────────────────────────────────────────────────────

function GuidanceStrip({
  progress,
  kind,
}: {
  progress: OnboardingProgress | null;
  kind: ComposeKind;
}) {
  const global = deriveGuidance(progress);
  const nudge = deriveNudge(kind, progress);
  const msg = nudge ?? global;
  if (!msg) return null;
  return (
    <div className="compose__guidance" role="note" aria-label="Setup guidance">
      <span className="compose__guidance-icon" aria-hidden>ℹ</span>
      <span>{msg}</span>
    </div>
  );
}

// ── Verbatim error with recovery CTA (AC4/NFR-5) ────────────────────────────

function ErrorBanner({
  status,
  message,
  onDismiss,
  onRetry,
}: {
  status: number;
  message: string;
  onDismiss: () => void;
  onRetry: () => void;
}) {
  const cta =
    status === 409
      ? "Switch to Edit mode to update an existing object."
      : status === 403
        ? "Contact your team admin for write access."
        : status === 501
          ? "This deployment is not yet connected to a cluster."
          : null;

  return (
    <div className="compose__error-banner" role="alert">
      <span className="compose__error-status">HTTP {status}</span>
      <span className="compose__error-msg">{message}</span>
      {cta && <span className="compose__error-cta">{cta}</span>}
      <div className="compose__error-actions">
        <button type="button" className="btn btn--ghost" onClick={onRetry}>
          Try again
        </button>
        <button type="button" className="btn" onClick={onDismiss}>
          Dismiss
        </button>
      </div>
    </div>
  );
}

// ── Main component ────────────────────────────────────────────────────────────

export function ComposeScreen() {
  const params = useSearchParams();
  const seed = useMemo(
    () =>
      parseComposeParams({
        kind: params.get("kind"),
        mode: params.get("mode"),
        name: params.get("name"),
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );

  const [kind, setKind] = useState<ComposeKind>(seed.kind);
  const [mode, setMode] = useState<Mode>(seed.mode);
  const [cf, setCf] = useState<ComposeForm>(() => {
    const base = emptyForm(seed.kind);
    return seed.name
      ? ({ kind: base.kind, form: { ...base.form, name: seed.name } } as ComposeForm)
      : base;
  });
  const [submit, setSubmit] = useState<SubmitState>({ kind: "idle" });
  const [progress, setProgress] = useState<OnboardingProgress | null>(null);

  // Fetch onboarding progress for guidance strip (soft dep — graceful on 501).
  useEffect(() => {
    fetch("/api/onboarding/progress", { cache: "no-store" })
      .then((r) => (r.ok ? (r.json() as Promise<OnboardingProgress>) : null))
      .then((p) => { if (p) setProgress(p); })
      .catch(() => {});
  }, []);

  const clientErrors = useMemo(() => validate(cf), [cf]);
  const serverErrors = submit.kind === "error" ? submit.fields : {};
  const errors: FieldErrors = { ...clientErrors, ...serverErrors };
  const valid = isValid(cf);

  // Live YAML preview (AC2/F-UI-1) — updates on every keystroke.
  const yamlPreview = useMemo(() => {
    try {
      return yamlStringify(toWire(cf));
    } catch {
      return "";
    }
  }, [cf]);

  const { entries: listEntries, loading: listLoading } = useOrgList(kind);
  const callerScope = useCallerScope();

  // Gate every team-scoped kind (agents/projects/roles/skills) behind a resolvable OWN team: a
  // global admin (no home tenancy, ISI-3921) or a dangling-team tenant would otherwise submit a form
  // the apiserver 404s. Teams themselves are the escape hatch — never gated. Fails open (loading /
  // unknown ⇒ form) so a transient error never blocks composing (ISI-3964).
  const composeGated =
    kind !== "teams" &&
    callerScope.kind === "resolved" &&
    !callerScope.hasOwnTeam;

  function selectKind(next: ComposeKind) {
    setKind(next);
    setCf(emptyForm(next));
    setSubmit({ kind: "idle" });
  }

  function selectMode(next: Mode) {
    setMode(next);
    setSubmit({ kind: "idle" });
  }

  // Clicking a left-pane object opens it in Edit mode with its name pre-filled (ISI-3985 — "if I
  // click on an agent I should see his skills"). Full-spec field hydration (populating the skill
  // refs / role / model from the object's CRD) is tracked separately; here we at least switch into
  // the correct edit context instead of a dead, unclickable list.
  function selectEntry(name: string) {
    setMode("edit");
    setCf(() => {
      const base = emptyForm(kind);
      return { kind: base.kind, form: { ...base.form, name } } as ComposeForm;
    });
    setSubmit({ kind: "idle" });
  }

  function patch(p: Record<string, unknown>) {
    setCf((prev) => ({ kind: prev.kind, form: { ...prev.form, ...p } }) as ComposeForm);
    setSubmit({ kind: "idle" });
  }

  async function apply() {
    setSubmit({ kind: "saving" });
    const name = cf.form.name.trim();
    const url =
      mode === "edit"
        ? `/api/compose/${kind}/${encodeURIComponent(name)}`
        : `/api/compose/${kind}`;
    const res = await fetch(url, {
      method: mode === "edit" ? "PUT" : "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(toWire(cf)),
    });
    if (res.ok) {
      setSubmit({ kind: "ok", result: (await res.json()) as ComposeResult });
    } else {
      const { message, fields } = parseError(res.status, await res.text());
      setSubmit({ kind: "error", status: res.status, message, fields });
    }
  }

  return (
    <div className="compose">
      <h1>Compose</h1>
      <p className="muted">
        Author a Team, Project, Agent, Role, or Skill — no raw YAML. Each apply is validated
        server-side; an edit creates a new revision. Team creation is admin-only.
      </p>

      {/* AC1: Tab bar replaces kind <select> */}
      <ComposeTabs active={kind} onSelect={selectKind} />

      {/* AC3: Guidance strip */}
      <GuidanceStrip progress={progress} kind={kind} />

      {/* Mode toggle */}
      <div className="compose__mode" role="radiogroup" aria-label="Compose mode">
        <button
          type="button"
          className={`btn ${mode === "create" ? "btn--primary" : ""}`}
          aria-pressed={mode === "create"}
          onClick={() => selectMode("create")}
        >
          Create
        </button>
        <button
          type="button"
          className={`btn ${mode === "edit" ? "btn--primary" : ""}`}
          aria-pressed={mode === "edit"}
          onClick={() => selectMode("edit")}
        >
          Edit by name
        </button>
      </div>

      {/* AC2: Three-pane layout */}
      <div className="compose__panes">
        {/* Left pane: object list from org read-model */}
        <aside className="compose__list-pane" aria-label={`Existing ${KIND_LABEL[kind]}s`}>
          <h2 className="compose__pane-heading">
            {KIND_LABEL[kind]}s
          </h2>
          {listLoading ? (
            <p className="muted">Loading…</p>
          ) : listEntries.length === 0 ? (
            <p className="muted">None yet.</p>
          ) : (
            <ul className="compose__list">
              {listEntries.map((e) => (
                <li key={e.name} className="compose__list-item">
                  <button
                    type="button"
                    className="compose__list-select"
                    onClick={() => selectEntry(e.name)}
                    title={`Edit ${e.name}`}
                  >
                    <span className="compose__list-name">{e.name}</span>
                    {e.subtitle && (
                      <span className="compose__list-sub muted">{e.subtitle}</span>
                    )}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </aside>

        {/* Center pane: form (or the no-own-team gate, ISI-3964) */}
        <main className="compose__form-pane">
          {composeGated ? (
            <div className="card compose__gate" data-testid="compose-no-team-gate">
              <h2>Create your team first</h2>
              <p className="muted">
                {callerScope.kind === "resolved" && callerScope.fleet
                  ? `You’re signed in as a fleet admin with no home team, so there’s nowhere to place this ${KIND_LABEL[kind]} yet. Create a team to author into, or pick an existing squad from the Agents view. (Authoring directly into another squad is coming in a later phase.)`
                  : `You need a team before you can add a ${KIND_LABEL[kind]} — everything you compose is scoped to a team.`}
              </p>
              <button
                type="button"
                className="btn btn--primary"
                onClick={() => selectKind("teams")}
                data-testid="compose-gate-create-team"
              >
                Create a Team
              </button>
            </div>
          ) : (
          <form
            className="card compose__form"
            onSubmit={(e) => {
              e.preventDefault();
              if (valid && submit.kind !== "saving") void apply();
            }}
          >
            <KindFields cf={cf} errors={errors} patch={patch} />

            <div className="compose__actions">
              <button
                type="submit"
                className="btn btn--primary"
                disabled={!valid || submit.kind === "saving"}
              >
                {submit.kind === "saving"
                  ? "Applying…"
                  : mode === "edit"
                    ? `Save ${KIND_LABEL[kind]} (new revision)`
                    : `Create ${KIND_LABEL[kind]}`}
              </button>

              {submit.kind === "ok" && (
                <span className="state state--ok" role="status">
                  {submit.result.operation === "created" ? "Created" : "Updated"}{" "}
                  {submit.result.kind} <strong>{submit.result.name}</strong> — revision{" "}
                  {submit.result.revision} in {submit.result.namespace}.
                </span>
              )}
            </div>
          </form>
          )}

          {/* AC4: Verbatim error with recovery CTA */}
          {!composeGated && submit.kind === "error" && (
            <ErrorBanner
              status={submit.status}
              message={submit.message}
              onDismiss={() => setSubmit({ kind: "idle" })}
              onRetry={() => { void apply(); }}
            />
          )}
        </main>

        {/* Right pane: live CRD YAML preview */}
        <aside className="compose__yaml-pane" aria-label="Live CRD YAML preview">
          <h2 className="compose__pane-heading">
            YAML preview
            <span className="compose__pane-note muted"> (live)</span>
          </h2>
          <pre className="compose__yaml"><code>{yamlPreview}</code></pre>
        </aside>
      </div>
    </div>
  );
}

// ── Per-kind field sets ───────────────────────────────────────────────────────

function KindFields({
  cf,
  errors,
  patch,
}: {
  cf: ComposeForm;
  errors: FieldErrors;
  patch: (p: Record<string, unknown>) => void;
}) {
  switch (cf.kind) {
    case "teams":
      return <TeamForm cf={cf} errors={errors} patch={patch} />;
    case "projects":
      return <ProjectForm cf={cf} errors={errors} patch={patch} />;
    case "agents":
      return <AgentForm cf={cf} errors={errors} patch={patch} />;
    case "roles": {
      const f = cf.form;
      return (
        <div className="compose__grid">
          <Field label="Project" error={errors["project"]}>
            <input
              value={f.project}
              onChange={(e) => patch({ project: e.target.value })}
              aria-invalid={!!errors["project"]}
            />
          </Field>
          <Field label="Name" hint="DNS-1123 label (lowercase, digits, '-')" error={errors["name"]}>
            <input
              value={f.name}
              onChange={(e) => patch({ name: e.target.value })}
              aria-invalid={!!errors["name"]}
              placeholder="my-resource"
            />
          </Field>
          <Field label="Prompt ref" error={errors["promptRef.name"]}>
            <input
              value={f.promptRef}
              onChange={(e) => patch({ promptRef: e.target.value })}
              aria-invalid={!!errors["promptRef.name"]}
              placeholder="name or namespace/name"
            />
          </Field>
          <Field label="Runtime class hint" hint="optional" error={errors["runtimeClassHint"]}>
            <select
              value={f.runtimeClassHint}
              onChange={(e) => patch({ runtimeClassHint: e.target.value })}
            >
              {RUNTIME_CLASS_HINTS.map((h) => (
                <option key={h || "default"} value={h}>
                  {h || "— default —"}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Default skills" hint="optional — one name (or namespace/name) per line">
            <textarea
              rows={3}
              value={f.defaultSkills}
              onChange={(e) => patch({ defaultSkills: e.target.value })}
            />
          </Field>
        </div>
      );
    }
    case "skills": {
      const f = cf.form;
      return (
        <div className="compose__grid">
          <Field label="Project" error={errors["project"]}>
            <input
              value={f.project}
              onChange={(e) => patch({ project: e.target.value })}
              aria-invalid={!!errors["project"]}
            />
          </Field>
          <Field label="Name" hint="DNS-1123 label (lowercase, digits, '-')" error={errors["name"]}>
            <input
              value={f.name}
              onChange={(e) => patch({ name: e.target.value })}
              aria-invalid={!!errors["name"]}
              placeholder="my-resource"
            />
          </Field>
          <Field label="Source type" error={errors["source.type"]}>
            <select
              value={f.sourceType}
              onChange={(e) => patch({ sourceType: e.target.value })}
            >
              <option value="inline">inline</option>
              <option value="git">git</option>
            </select>
          </Field>
          {f.sourceType === "inline" ? (
            <Field label="Inline source" error={errors["source.inline"]}>
              <textarea
                rows={5}
                value={f.inline}
                onChange={(e) => patch({ inline: e.target.value })}
                aria-invalid={!!errors["source.inline"]}
                placeholder="# skill definition"
              />
            </Field>
          ) : (
            <>
              <Field label="Git repo ref" error={errors["source.git.repoRef"]}>
                <input
                  value={f.gitRepoRef}
                  onChange={(e) => patch({ gitRepoRef: e.target.value })}
                  aria-invalid={!!errors["source.git.repoRef"]}
                />
              </Field>
              <Field label="Git ref" hint="branch / tag / SHA" error={errors["source.git.ref"]}>
                <input
                  value={f.gitRef}
                  onChange={(e) => patch({ gitRef: e.target.value })}
                  aria-invalid={!!errors["source.git.ref"]}
                />
              </Field>
              <Field label="Git path" hint="optional — path within the repo">
                <input value={f.gitPath} onChange={(e) => patch({ gitPath: e.target.value })} />
              </Field>
            </>
          )}
          <Field label="Permissions" hint="optional — one permission per line">
            <textarea
              rows={3}
              value={f.permissions}
              onChange={(e) => patch({ permissions: e.target.value })}
            />
          </Field>
        </div>
      );
    }
  }
}
