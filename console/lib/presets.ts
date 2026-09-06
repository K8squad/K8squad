// lib/presets.ts — Role-preset → default-model table (AD-3, FR-6.1, ISI-3676).
//
// The Role CRD has NO model field (F-CRD-1): a Role carries only
// { promptRef, defaultSkills, runtimeClassHint } (api/v1alpha1/role_types.go).
// The default model is an Agent property (Agent.spec.model), so the preset
// model default lives here, console-side. The template-materialize flow
// (POST /api/compose/squad, E2-S2) reads this table when it builds each
// Agent from a seeded Role. Never put a model on the Role CR.
//
// The three seeded Role CRs ship in the public repo under config/roles/.

/** Stable id of a seeded Role preset — matches the Role CR metadata.name. */
export type RolePresetId = "role-boss" | "role-implementer" | "role-manager";

export type RolePreset = {
  /** metadata.name of the seeded Role CR in config/roles/. */
  roleRef: RolePresetId;
  /** Human-facing preset name (FR-6.1 pick-a-card tiles). */
  label: string;
  /** One-line preset summary for the template gallery. */
  summary: string;
  /** Default Agent.spec.model for this preset (FR-6.1). */
  defaultModel: string;
  /** Skills the Role presets via spec.defaultSkills (unioned, ADR-044). */
  defaultSkills: string[];
};

/**
 * The three seeded Role presets (FR-6.1):
 * Boss → Opus-5 (planning+board), Implementer → Sonnet-5 (code+test),
 * Manager → Sonnet-5 (review+board).
 */
export const ROLE_PRESETS: Readonly<Record<RolePresetId, RolePreset>> = {
  "role-boss": {
    roleRef: "role-boss",
    label: "Boss",
    summary: "Plans, decomposes and delegates.",
    defaultModel: "claude-opus-5",
    defaultSkills: ["planning", "board"],
  },
  "role-implementer": {
    roleRef: "role-implementer",
    label: "Implementer",
    summary: "Writes, tests and ships code.",
    defaultModel: "claude-sonnet-5",
    defaultSkills: ["code", "test"],
  },
  "role-manager": {
    roleRef: "role-manager",
    label: "Manager",
    summary: "Grooms, reviews and unblocks.",
    defaultModel: "claude-sonnet-5",
    defaultSkills: ["review", "board"],
  },
};

/**
 * Default model for a seeded Role ref, or undefined when the ref is not one
 * of the three presets (advanced users may point at their own Role CRs —
 * those carry no console default).
 */
export function defaultModelForRole(roleRef: string): string | undefined {
  return (ROLE_PRESETS as Record<string, RolePreset>)[roleRef]?.defaultModel;
}
