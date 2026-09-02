// lib/compose.ts — the Compose (04-compose-crd screen, story 8.5) form model.
//
// A typed, NARROW write model over the five ksquad.io compose CRDs (Team / Project / Agent /
// Role / Skill). It is the client sibling of the apiserver's CRD-apply write surface (ISI-3198):
// one typed request per kind — never arbitrary YAML exec (scope guard R6, parent ISI-3196) — that
// the console POSTs (create) / PUTs (edit) through the BFF (§13 choke point), which server-side
// re-validates, RBAC-gates, team-scopes, and provenances the apply. This module is PURE and
// unit-tested: `toWire` produces exactly the wire contract each apiserver `*Request` struct
// decodes, and `validate` mirrors the server's field-level checks at the form edge so the common
// mistakes surface before a round-trip (the apiserver stays the authority — a server 422 maps back
// onto these same field keys).

/** The five compose CRD kinds and their apiserver route segment (POST /api/{segment}). */
export const COMPOSE_KINDS = [
  "teams",
  "projects",
  "agents",
  "roles",
  "skills",
] as const;

export type ComposeKind = (typeof COMPOSE_KINDS)[number];

export function isComposeKind(v: string): v is ComposeKind {
  return (COMPOSE_KINDS as readonly string[]).includes(v);
}

/** Create (POST) vs edit-by-name (PUT, new revision) — the compose surface's two write modes. */
export type ComposeMode = "create" | "edit";

/**
 * parseComposeParams maps the compose deep-link query contract (ISI-3554 Story A) onto the initial
 * kind/mode/name state. The URL contract is stable across Stories A/B:
 *   ?kind=agents        pre-selects the kind (canonical: the real ComposeKind literal, PLURAL)
 *   &mode=edit          optional; defaults to "create"
 *   &name=<dns1123>     optional edit target, pre-filled into the name field
 *
 * It is deliberately forgiving so a hand-typed or stale link never lands the user on a broken form:
 *   - the singular alias `agent` maps to `agents` (ISI-3546 wrote `kind=agent` as shorthand);
 *   - an absent / unrecognized `kind` falls back to the current default (`projects`), so `/compose`
 *     with no params behaves exactly as it does today (AC4 — no regression);
 *   - `mode` is `edit` only for the exact literal, else `create`.
 * PURE + unit-tested; the component seeds useState from it.
 */
export function parseComposeParams(p: {
  kind?: string | null;
  mode?: string | null;
  name?: string | null;
}): { kind: ComposeKind; mode: ComposeMode; name: string } {
  const raw = (p.kind ?? "").trim();
  const aliased = raw === "agent" ? "agents" : raw; // accept the singular shorthand as an alias
  const kind: ComposeKind = isComposeKind(aliased) ? aliased : "projects";
  const mode: ComposeMode = p.mode === "edit" ? "edit" : "create";
  const name = (p.name ?? "").trim();
  return { kind, mode, name };
}

/** Human labels for the kind selector. */
export const KIND_LABEL: Record<ComposeKind, string> = {
  teams: "Team",
  projects: "Project",
  agents: "Agent",
  roles: "Role",
  skills: "Skill",
};

// ── Field-level validation, mirroring composecrd.go ───────────────────────────

/** DNS-1123 label (the CR name becomes metadata.name) — same regex the apiserver enforces. */
const DNS1123_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

/** Keyed field errors: field path (matching the wire) → message. */
export type FieldErrors = Record<string, string>;

function checkName(field: string, name: string, errs: FieldErrors): void {
  const v = name.trim();
  if (!v) errs[field] = "is required";
  else if (v.length > 253) errs[field] = "must be at most 253 characters";
  else if (!DNS1123_LABEL.test(v))
    errs[field] =
      "must be a valid DNS-1123 label (lowercase alphanumeric or '-', starting/ending alphanumeric)";
}

function checkRequired(field: string, value: string, errs: FieldErrors): void {
  if (!value.trim()) errs[field] = "is required";
}

// ── Per-kind form shapes ──────────────────────────────────────────────────────

export type TeamForm = { name: string; namespaceStrategy: string };

export type ProjectForm = {
  name: string;
  repoUrl: string;
  repoRef: string;
  goals: string; // one goal per line in the form; split on toWire
  egressPolicyRef: string; // "name" or "namespace/name"; omitted when blank
};

export type AgentForm = {
  project: string;
  name: string;
  runtimeRef: string;
  roleRef: string;
  skillRefs: string; // one ref per line
  model: string;
  modelEndpointRef: string; // secret "name" or "name/key"; omitted when blank
  credentialSecretRef: string; // secret "name" or "name/key"
};

export type RoleForm = {
  project: string;
  name: string;
  promptRef: string;
  defaultSkills: string; // one ref per line
  runtimeClassHint: string; // "" | gvisor | kata | runc
};

export type SkillSourceType = "inline" | "git";

export type SkillForm = {
  project: string;
  name: string;
  sourceType: SkillSourceType;
  inline: string;
  gitRepoRef: string;
  gitRef: string;
  gitPath: string;
  permissions: string; // one permission per line
};

export type ComposeForm =
  | { kind: "teams"; form: TeamForm }
  | { kind: "projects"; form: ProjectForm }
  | { kind: "agents"; form: AgentForm }
  | { kind: "roles"; form: RoleForm }
  | { kind: "skills"; form: SkillForm };

export const RUNTIME_CLASS_HINTS = ["", "gvisor", "kata", "runc"] as const;

export function emptyForm(kind: ComposeKind): ComposeForm {
  switch (kind) {
    case "teams":
      return { kind, form: { name: "", namespaceStrategy: "" } };
    case "projects":
      return {
        kind,
        form: { name: "", repoUrl: "", repoRef: "", goals: "", egressPolicyRef: "" },
      };
    case "agents":
      return {
        kind,
        form: {
          project: "",
          name: "",
          runtimeRef: "",
          roleRef: "",
          skillRefs: "",
          model: "",
          modelEndpointRef: "",
          credentialSecretRef: "",
        },
      };
    case "roles":
      return {
        kind,
        form: { project: "", name: "", promptRef: "", defaultSkills: "", runtimeClassHint: "" },
      };
    case "skills":
      return {
        kind,
        form: {
          project: "",
          name: "",
          sourceType: "inline",
          inline: "",
          gitRepoRef: "",
          gitRef: "",
          gitPath: "",
          permissions: "",
        },
      };
  }
}

// ── Helpers: parse the textarea list / ref shapes into wire objects ───────────

/** Split a one-per-line textarea into trimmed, non-empty entries. */
function lines(text: string): string[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l.length > 0);
}

/** Parse "name" or "namespace/name" into an object ref (namespace optional). */
export function parseObjectRef(s: string): { name: string; namespace?: string } {
  const t = s.trim();
  const i = t.indexOf("/");
  if (i > 0) return { name: t.slice(i + 1).trim(), namespace: t.slice(0, i).trim() };
  return { name: t };
}

/** Parse "name" or "name/key" into a secret ref (key optional). */
export function parseSecretRef(s: string): { name: string; key?: string } {
  const t = s.trim();
  const i = t.indexOf("/");
  if (i > 0) return { name: t.slice(0, i).trim(), key: t.slice(i + 1).trim() };
  return { name: t };
}

// ── toWire: produce the exact JSON each apiserver *Request struct decodes ──────

export function toWire(cf: ComposeForm): Record<string, unknown> {
  switch (cf.kind) {
    case "teams": {
      const f = cf.form;
      return {
        name: f.name.trim(),
        ...(f.namespaceStrategy.trim() ? { namespaceStrategy: f.namespaceStrategy.trim() } : {}),
      };
    }
    case "projects": {
      const f = cf.form;
      const goals = lines(f.goals);
      return {
        name: f.name.trim(),
        repo: {
          url: f.repoUrl.trim(),
          ...(f.repoRef.trim() ? { ref: f.repoRef.trim() } : {}),
        },
        ...(goals.length ? { goals } : {}),
        ...(f.egressPolicyRef.trim() ? { egressPolicyRef: parseObjectRef(f.egressPolicyRef) } : {}),
      };
    }
    case "agents": {
      const f = cf.form;
      const skillRefs = lines(f.skillRefs).map(parseObjectRef);
      return {
        project: f.project.trim(),
        name: f.name.trim(),
        runtimeRef: parseObjectRef(f.runtimeRef),
        roleRef: parseObjectRef(f.roleRef),
        ...(skillRefs.length ? { skillRefs } : {}),
        model: f.model.trim(),
        ...(f.modelEndpointRef.trim() ? { modelEndpointRef: parseSecretRef(f.modelEndpointRef) } : {}),
        credentialSecretRef: parseSecretRef(f.credentialSecretRef),
      };
    }
    case "roles": {
      const f = cf.form;
      const defaultSkills = lines(f.defaultSkills).map(parseObjectRef);
      return {
        project: f.project.trim(),
        name: f.name.trim(),
        promptRef: parseObjectRef(f.promptRef),
        ...(defaultSkills.length ? { defaultSkills } : {}),
        ...(f.runtimeClassHint.trim() ? { runtimeClassHint: f.runtimeClassHint.trim() } : {}),
      };
    }
    case "skills": {
      const f = cf.form;
      const permissions = lines(f.permissions);
      const source: Record<string, unknown> = { type: f.sourceType };
      if (f.sourceType === "inline") {
        source.inline = f.inline;
      } else {
        source.git = {
          repoRef: f.gitRepoRef.trim(),
          ref: f.gitRef.trim(),
          ...(f.gitPath.trim() ? { path: f.gitPath.trim() } : {}),
        };
      }
      return {
        project: f.project.trim(),
        name: f.name.trim(),
        source,
        ...(permissions.length ? { permissions } : {}),
      };
    }
  }
}

// ── validate: mirror composecrd.go's plan*() field checks ─────────────────────

export function validate(cf: ComposeForm): FieldErrors {
  const errs: FieldErrors = {};
  switch (cf.kind) {
    case "teams": {
      checkName("name", cf.form.name, errs);
      break;
    }
    case "projects": {
      checkName("name", cf.form.name, errs);
      checkRequired("repo.url", cf.form.repoUrl, errs);
      break;
    }
    case "agents": {
      const f = cf.form;
      // `project` gates the write-tier RBAC scope on the apiserver (a non-admin with no scope is a
      // 400); require it at the form edge so the common case fails fast with a clear field error.
      checkRequired("project", f.project, errs);
      checkName("name", f.name, errs);
      checkRequired("runtimeRef.name", parseObjectRef(f.runtimeRef).name, errs);
      checkRequired("roleRef.name", parseObjectRef(f.roleRef).name, errs);
      checkRequired("credentialSecretRef.name", parseSecretRef(f.credentialSecretRef).name, errs);
      checkRequired("model", f.model, errs);
      break;
    }
    case "roles": {
      const f = cf.form;
      checkRequired("project", f.project, errs);
      checkName("name", f.name, errs);
      checkRequired("promptRef.name", parseObjectRef(f.promptRef).name, errs);
      if (f.runtimeClassHint && !["gvisor", "kata", "runc"].includes(f.runtimeClassHint))
        errs["runtimeClassHint"] = "must be one of gvisor, kata, runc";
      break;
    }
    case "skills": {
      const f = cf.form;
      checkRequired("project", f.project, errs);
      checkName("name", f.name, errs);
      if (f.sourceType === "inline") {
        checkRequired("source.inline", f.inline, errs);
      } else if (f.sourceType === "git") {
        checkRequired("source.git.repoRef", f.gitRepoRef, errs);
        checkRequired("source.git.ref", f.gitRef, errs);
      } else {
        errs["source.type"] = "must be one of inline, git";
      }
      break;
    }
  }
  return errs;
}

export function isValid(cf: ComposeForm): boolean {
  return Object.keys(validate(cf)).length === 0;
}

/** The apiserver apply result (composeResult) surfaced back to the screen. */
export type ComposeResult = {
  kind: string;
  name: string;
  namespace: string;
  revision: number;
  operation: "created" | "updated";
};
