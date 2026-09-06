"use client";

// components/onboarding/TemplateGallery.tsx — the frame-03 starter-squad gallery
// (E2-S3, ISI-3678; FR-2.1/2.2/2.3, AD-3; mock frame 03).
//
// Mounted by the Launchpad's Agents milestone when Nadia picked the ⚡ starter-squad
// on-ramp. Three template cards preview the EXACT agents the materialize endpoint
// would create (name → seeded Role preset → default model from lib/presets.ts);
// "Start blank" stays available and drops to the E0 shared AgentForm (FR-8 — no
// bespoke inputs here either). Apply is ONE call: POST /api/compose/squad
// (E2-S2 BFF → apiserver) carrying the template id + the preset-keyed default
// models; the server does the planning, provenance and per-object applies.
//
// Honesty contract (AC3, NFR-5): every status reaches the screen VERBATIM.
// 201 → onApplied (the hub re-reads the server projection; remaining milestones
// reduce to review & connect). 207 → a per-object result panel: created objects
// with their operation, failed objects with kind/name/status/error exactly as the
// server reported them, plus a real recovery CTA ("Finish by hand" → the shared
// form). 401/403/404/422/501 → the server's message and field errors, never a
// masked fake-green.
//
// The catalog below MIRRORS the apiserver template set (squadTemplates,
// internal/apiserver/composecrd.go) — template ids must stay in lockstep; the
// wire contract test in test/onboarding/TemplateGallery.test.tsx pins the ids.

import { useMemo, useState } from "react";
import { ROLE_PRESETS, type RolePresetId } from "@/lib/presets";

/** The preset keys the apiserver's squad endpoint understands (boss/implementer/
 * manager — squadRequest.Models is keyed by these, NOT by the role-boss Role name). */
export type SquadPresetKey = "boss" | "implementer" | "manager";

/** One agent a template materializes: CR name + the seeded Role preset it references. */
export type SquadTemplateAgent = {
  name: string;
  preset: SquadPresetKey;
  /** Optional persona note for BMAD's named personas (e.g. sam = CEO). */
  persona?: string;
};

export type SquadTemplate = {
  /** Wire id — MUST match squadTemplates keys in composecrd.go. */
  id: "minimal-trio" | "solo" | "bmad";
  /** Card title (mock frame 03). */
  title: string;
  /** ★ the D1 recommended default. */
  recommended?: boolean;
  blurb: string;
  agents: ReadonlyArray<SquadTemplateAgent>;
};

/** The template catalog (AC1) — mirrors squadTemplates (composecrd.go:836). */
export const SQUAD_TEMPLATES: ReadonlyArray<SquadTemplate> = [
  {
    id: "minimal-trio",
    title: "Minimal Trio",
    recommended: true,
    blurb: "Boss + Implementer + Manager — the smallest squad that plans, ships and reviews.",
    agents: [
      { name: "boss", preset: "boss" },
      { name: "implementer", preset: "implementer" },
      { name: "manager", preset: "manager" },
    ],
  },
  {
    id: "bmad",
    title: "BMAD Squad",
    blurb: "The full examples/bmad-team persona set mapped onto the seeded Role presets.",
    agents: [
      { name: "sam", preset: "boss", persona: "CEO" },
      { name: "john", preset: "manager", persona: "Product Manager" },
      { name: "winston", preset: "boss", persona: "Architect" },
      { name: "uma", preset: "implementer", persona: "UX Designer" },
      { name: "mary", preset: "manager", persona: "Brainstormer" },
      { name: "cade", preset: "manager", persona: "Challenger" },
      { name: "quill", preset: "implementer", persona: "Writer" },
      { name: "amelia", preset: "manager", persona: "Code Reviewer" },
      { name: "tess", preset: "implementer", persona: "Tester" },
      { name: "ada", preset: "implementer", persona: "Coder" },
    ],
  },
  {
    id: "solo",
    title: "Solo",
    blurb: "Boss + Implementer — just enough to delegate and ship on your own.",
    agents: [
      { name: "boss", preset: "boss" },
      { name: "implementer", preset: "implementer" },
    ],
  },
];

/** Role CR name for a preset key (squadRolePrefix + key, matching the server). */
export function roleRefForPreset(preset: SquadPresetKey): RolePresetId {
  return `role-${preset}` as RolePresetId;
}

/** The preset-keyed default-model map the apply call sends (AC2: "preset models
 * from presets.ts" — FR-6.1; absent keys fall back to the server-side defaults). */
export function presetModels(): Record<SquadPresetKey, string> {
  return {
    boss: ROLE_PRESETS[roleRefForPreset("boss")].defaultModel,
    implementer: ROLE_PRESETS[roleRefForPreset("implementer")].defaultModel,
    manager: ROLE_PRESETS[roleRefForPreset("manager")].defaultModel,
  };
}

// ── Wire result shapes (squadResponse, composecrd.go:810) ─────────────────────

type ComposeResult = {
  kind?: string;
  name: string;
  operation?: string;
};

type SquadObjectError = {
  kind: string;
  name: string;
  status: number;
  error: string;
};

type SquadResponse = {
  team?: ComposeResult | null;
  agents?: ComposeResult[];
  errors?: SquadObjectError[];
};

type FieldErrors = Record<string, string>;

/** Parse a non-2xx squad response body (verbatim honesty, mirroring the shared
 * ComposeScreen/Launchpad contract): server message + field errors as sent. */
function parseError(status: number, body: string): { message: string; fields: FieldErrors } {
  const fields: FieldErrors = {};
  let message = `Materialize failed (status ${status}).`;
  try {
    const j = JSON.parse(body) as { error?: string; fields?: Array<{ field: string; message: string }> };
    if (j.error) message = j.error;
    if (Array.isArray(j.fields)) for (const f of j.fields) fields[f.field] = f.message;
  } catch {
    /* non-JSON — keep the status message */
  }
  if (status === 401) message = "You must sign in to compose.";
  else if (status === 404) message = message || "No team scope for this caller.";
  else if (status === 501) message = "Compose is not available on this deployment yet.";
  return { message, fields };
}

type ApplyState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "error"; message: string; fields: FieldErrors }
  | { kind: "partial"; created: SquadResponse };

export function TemplateGallery({
  onApplied,
  onStartBlank,
}: {
  /** Full materialize (201): the hub re-reads the projection and advances. */
  onApplied: (note: string) => void;
  /** "Start blank" — FR-2.2: the manual path must remain available. */
  onStartBlank: () => void;
}) {
  const [selected, setSelected] = useState<SquadTemplate>(
    () => SQUAD_TEMPLATES[0] as SquadTemplate,
  );
  const [project, setProject] = useState("");
  const [apply, setApply] = useState<ApplyState>({ kind: "idle" });

  const projectError =
    apply.kind === "error" && apply.fields["project"]
      ? apply.fields["project"]
      : project.trim().length > 0
        ? undefined
        : "required — the write-tier scope for the squad's agents";

  async function materialize() {
    setApply({ kind: "saving" });
    try {
      const res = await fetch("/api/compose/squad", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          template: selected.id,
          project: project.trim(),
          models: presetModels(),
        }),
      });
      if (res.status === 201) {
        const body = (await res.json()) as SquadResponse;
        const n = body.agents?.length ?? 0;
        const teamNote = body.team ? `team "${body.team.name}"` + (body.team.operation === "existing" ? " (existing)" : "") + ", " : "";
        onApplied(`Squad materialized — ${teamNote}${n} agent${n === 1 ? "" : "s"} created`);
        return;
      }
      if (res.status === 207) {
        setApply({ kind: "partial", created: (await res.json()) as SquadResponse });
        return;
      }
      const { message, fields } = parseError(res.status, await res.text());
      setApply({ kind: "error", message, fields });
    } catch {
      setApply({ kind: "error", message: "Network error — the squad was not sent.", fields: {} });
    }
  }

  const busy = apply.kind === "saving";

  return (
    <div className="launchpad-gallery" data-testid="template-gallery">
      <ul className="launchpad-gallery__cards" aria-label="Starter squad templates">
        {SQUAD_TEMPLATES.map((t) => {
          const active = t.id === selected.id;
          return (
            <li key={t.id}>
              <button
                type="button"
                className={`launchpad-gallery__card${active ? " launchpad-gallery__card--selected" : ""}`}
                aria-pressed={active}
                onClick={() => {
                  setSelected(t);
                  setApply({ kind: "idle" });
                }}
              >
                <span className="launchpad-gallery__card-title">
                  {t.title}
                  {t.recommended && (
                    <span className="launchpad-gallery__badge">
                      ★<span className="sr-only"> recommended</span>
                    </span>
                  )}
                </span>
                <span className="launchpad-gallery__blurb">{t.blurb}</span>
                <span className="launchpad-gallery__preview">
                  {t.agents.slice(0, 4).map((a) => (
                    <span key={a.name} className="launchpad-gallery__agent">
                      <strong>{a.name}</strong>
                      <span className="muted">
                        {ROLE_PRESETS[roleRefForPreset(a.preset)].label} ·{" "}
                        {ROLE_PRESETS[roleRefForPreset(a.preset)].defaultModel}
                      </span>
                    </span>
                  ))}
                  {t.agents.length > 4 && (
                    <span className="muted">+{t.agents.length - 4} more</span>
                  )}
                </span>
                <span className="launchpad-gallery__count muted">
                  {t.agents.length} agent{t.agents.length === 1 ? "" : "s"}
                </span>
              </button>
            </li>
          );
        })}
      </ul>

      {apply.kind === "partial" && <PartialResultPanel result={apply.created} onDismiss={() => setApply({ kind: "idle" })} onStartBlank={onStartBlank} />}

      <div className="launchpad-gallery__apply">
        <label className="launchpad-gallery__project">
          <span>
            Project <span className="muted">— the squad these agents compose within</span>
          </span>
          <input
            value={project}
            onChange={(e) => {
              setProject(e.target.value);
              if (apply.kind === "error") setApply({ kind: "idle" });
            }}
            aria-invalid={!!projectError}
            aria-label="Project scope for the starter squad"
            placeholder="my-project"
          />
          {projectError && (
            <span className="launchpad-gallery__field-error" role="alert">
              {projectError}
            </span>
          )}
        </label>
        <div className="launchpad-gallery__actions">
          <button
            type="button"
            className="btn btn--primary"
            disabled={busy || project.trim().length === 0}
            onClick={() => void materialize()}
          >
            {busy ? "Materializing…" : `Use ${selected.title} →`}
          </button>
          <button type="button" className="btn" onClick={onStartBlank}>
            Start blank
          </button>
        </div>
        {apply.kind === "error" && (
          <span className="state state--error" role="alert">
            {apply.message}
          </span>
        )}
      </div>
    </div>
  );
}

// ── 207 partial result (AC3 — verbatim, with a recovery CTA) ──────────────────

function PartialResultPanel({
  result,
  onDismiss,
  onStartBlank,
}: {
  result: SquadResponse;
  onDismiss: () => void;
  onStartBlank: () => void;
}) {
  const agents = result.agents ?? [];
  const errors = result.errors ?? [];
  return (
    <div className="launchpad-gallery__partial" role="alert" data-testid="squad-partial-result">
      <p>
        <strong>Partial materialize (207)</strong> — the server reported some objects failed.
        Nothing is masked:
      </p>
      <ul className="launchpad-gallery__outcomes">
        {result.team && (
          <li className="launchpad-gallery__ok">
            ✓ Team <code>{result.team.name}</code> — {result.team.operation ?? "created"}
          </li>
        )}
        {agents.map((a) => (
          <li key={a.name} className="launchpad-gallery__ok">
            ✓ Agent <code>{a.name}</code> — {a.operation ?? "created"}
          </li>
        ))}
        {errors.map((e) => (
          <li key={`${e.kind}-${e.name}`} className="launchpad-gallery__fail">
            ✗ {e.kind} <code>{e.name}</code> — {e.status} {e.error}
          </li>
        ))}
      </ul>
      <div className="launchpad-gallery__actions">
        <button type="button" className="btn btn--primary" onClick={onStartBlank}>
          Finish by hand →
        </button>
        <button type="button" className="btn" onClick={onDismiss}>
          Back to gallery
        </button>
      </div>
      <p className="muted">
        Created objects count immediately — finish the failed ones with the shared form (retries
        of the whole template would conflict on the names that already exist).
      </p>
    </div>
  );
}
