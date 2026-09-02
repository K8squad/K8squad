"use client";

// components/compose/ComposeScreen.tsx — the Compose surface (story 8.5 / UX screen 04-compose-crd).
//
// A typed, guided authoring surface for the five ksquad.io compose CRDs (Team / Project / Agent /
// Role / Skill). Pick a kind, fill the typed form, and Create (POST) or Edit-by-name (PUT) through
// the BFF — never raw YAML (scope guard R6). The apiserver (ISI-3198) is the authority: it
// re-validates (field-level 422), enforces the write-tier membership gate (viewer → 403; Team is
// admin-only), team-scopes the apply, and records provenance. This screen mirrors the server's
// field checks at the edge (lib/compose) for fast feedback, merges any server 422 field errors back
// onto the same fields, and surfaces every non-2xx status VERBATIM (403/404/409/501) rather than
// guessing. Revision + operation from a successful apply are shown so the author sees "created
// revision N" / "updated to revision N".

import { useMemo, useState } from "react";
import { useSearchParams } from "next/navigation";
import { Field } from "./fields";
import { ModelSelector } from "./ModelSelector";
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

export function ComposeScreen() {
  // Deep-link seeding (ISI-3554 Story A). The compose surface is reachable directly (/compose) or
  // via a discoverable entry point that pre-selects the form — "+ New Agent" on /agents links to
  // ?kind=agents, and "Edit" on an agent detail links to ?kind=agents&mode=edit&name=<agentName>.
  // Params seed the INITIAL state only; manual kind/mode switching afterward still works because the
  // selectKind/selectMode handlers own the state from then on. Absent/invalid params fall back to
  // today's defaults (parseComposeParams → projects/create), so a bare /compose is unchanged (AC4).
  const params = useSearchParams();
  const seed = useMemo(
    () =>
      parseComposeParams({
        kind: params.get("kind"),
        mode: params.get("mode"),
        name: params.get("name"),
      }),
    // Seed once from the entry URL; later manual edits are owned by component state, not the URL.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );

  const [kind, setKind] = useState<ComposeKind>(seed.kind);
  const [mode, setMode] = useState<Mode>(seed.mode);
  const [cf, setCf] = useState<ComposeForm>(() => {
    const base = emptyForm(seed.kind);
    // Pre-fill the name so an edit deep-link targets the right object (PUT is name-addressed).
    return seed.name
      ? ({ kind: base.kind, form: { ...base.form, name: seed.name } } as ComposeForm)
      : base;
  });
  const [submit, setSubmit] = useState<SubmitState>({ kind: "idle" });

  const clientErrors = useMemo(() => validate(cf), [cf]);
  // Server 422 field errors overlay client errors until the field changes again.
  const serverErrors = submit.kind === "error" ? submit.fields : {};
  const errors: FieldErrors = { ...clientErrors, ...serverErrors };
  const valid = isValid(cf);

  function selectKind(next: ComposeKind) {
    setKind(next);
    setCf(emptyForm(next));
    setSubmit({ kind: "idle" });
  }

  function selectMode(next: Mode) {
    setMode(next);
    setSubmit({ kind: "idle" });
  }

  // Narrowed setter: patch the active kind's form (the union guarantees form/kind agree).
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
        Author a Team, Project, Agent, Role, or Skill from a typed form — no raw YAML. Each apply is
        validated and recorded server-side; an edit creates a new revision. Team creation is admin-only.
      </p>

      <div className="compose__controls">
        <label>
          <span>Kind</span>
          <select
            value={kind}
            onChange={(e) => selectKind(e.target.value as ComposeKind)}
            aria-label="Compose kind"
          >
            {COMPOSE_KINDS.map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL[k]}
              </option>
            ))}
          </select>
        </label>

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
      </div>

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
              {submit.result.operation === "created" ? "Created" : "Updated"} {submit.result.kind}{" "}
              <strong>{submit.result.name}</strong> — revision {submit.result.revision} in{" "}
              {submit.result.namespace}.
            </span>
          )}
          {submit.kind === "error" && (
            <span className="state state--error" role="alert">
              {submit.message}
            </span>
          )}
        </div>
      </form>
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
  const nameField = (
    <Field label="Name" hint="DNS-1123 label (lowercase, digits, '-')" error={errors["name"]}>
      <input
        value={cf.form.name}
        onChange={(e) => patch({ name: e.target.value })}
        aria-invalid={!!errors["name"]}
        placeholder="my-resource"
      />
    </Field>
  );

  switch (cf.kind) {
    case "teams": {
      const f = cf.form;
      return (
        <div className="compose__grid">
          {nameField}
          <Field label="Namespace strategy" hint="optional — defaults to perTeam">
            <input
              value={f.namespaceStrategy}
              onChange={(e) => patch({ namespaceStrategy: e.target.value })}
              placeholder="perTeam"
            />
          </Field>
        </div>
      );
    }
    case "projects": {
      const f = cf.form;
      return (
        <div className="compose__grid">
          {nameField}
          <Field label="Repo URL" error={errors["repo.url"]}>
            <input
              value={f.repoUrl}
              onChange={(e) => patch({ repoUrl: e.target.value })}
              aria-invalid={!!errors["repo.url"]}
              placeholder="https://github.com/org/repo"
            />
          </Field>
          <Field label="Repo ref" hint="optional — branch / tag / SHA">
            <input
              value={f.repoRef}
              onChange={(e) => patch({ repoRef: e.target.value })}
              placeholder="main"
            />
          </Field>
          <Field label="Egress policy ref" hint="optional — name or namespace/name">
            <input
              value={f.egressPolicyRef}
              onChange={(e) => patch({ egressPolicyRef: e.target.value })}
            />
          </Field>
          <Field label="Goals" hint="one goal per line">
            <textarea
              rows={3}
              value={f.goals}
              onChange={(e) => patch({ goals: e.target.value })}
              placeholder={"Ship the checkout flow\nHarden the payment path"}
            />
          </Field>
        </div>
      );
    }
    case "agents": {
      const f = cf.form;
      return (
        <div className="compose__grid">
          <Field label="Project" hint="the squad this Agent composes within" error={errors["project"]}>
            <input
              value={f.project}
              onChange={(e) => patch({ project: e.target.value })}
              aria-invalid={!!errors["project"]}
            />
          </Field>
          {nameField}
          <Field label="Runtime ref" error={errors["runtimeRef.name"]}>
            <input
              value={f.runtimeRef}
              onChange={(e) => patch({ runtimeRef: e.target.value })}
              aria-invalid={!!errors["runtimeRef.name"]}
              placeholder="name or namespace/name"
            />
          </Field>
          <Field label="Role ref" error={errors["roleRef.name"]}>
            <input
              value={f.roleRef}
              onChange={(e) => patch({ roleRef: e.target.value })}
              aria-invalid={!!errors["roleRef.name"]}
            />
          </Field>
          <ModelSelector
            model={f.model}
            modelEndpointRef={f.modelEndpointRef}
            byoEnabled={f.byoEnabled}
            errors={errors}
            patch={patch}
          />
          <Field label="Credential Secret ref" hint="name or name/key" error={errors["credentialSecretRef.name"]}>
            <input
              value={f.credentialSecretRef}
              onChange={(e) => patch({ credentialSecretRef: e.target.value })}
              aria-invalid={!!errors["credentialSecretRef.name"]}
            />
          </Field>
          <Field label="Skill refs" hint="optional — one name (or namespace/name) per line">
            <textarea
              rows={3}
              value={f.skillRefs}
              onChange={(e) => patch({ skillRefs: e.target.value })}
            />
          </Field>
        </div>
      );
    }
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
          {nameField}
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
          {nameField}
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
