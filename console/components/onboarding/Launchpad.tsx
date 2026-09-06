"use client";

// components/onboarding/Launchpad.tsx — the Launchpad hub (E1-S2, ISI-3674; mock frames
// 01/06/07; FR-1.1/1.2/1.5, AC1–AC4).
//
// First-run Nadia's home: a 4-milestone spine (① Team · ② Agents · ③ Models & credentials ·
// ④ Project), a top progress meter, a card per milestone (state + one-line why + primary CTA),
// and two on-ramps (⚡ starter squad = default/recommended, 🧭 step by step). Returning Raj
// gets the resume state (frame 06): done cards carry a Review link, the first incomplete card
// is actionable, later ones are Locked. At 4/4 (in-session) the hub celebrates with "Your
// squad is ready" (frame 07) and yields to the real Overview.
//
// Single-source constraint (FR-8, NFR-6): milestone step panels mount the E0 SHARED forms
// (TeamForm / AgentForm / ProjectForm) over the SAME lib/compose validate/toWire + the SAME
// POST /api/compose/{kind} BFF write path ComposeScreen uses — no bespoke onboarding inputs,
// no forked write path. Models & credentials intentionally mounts NO form: that surface is
// E3-S4's credential sheet (ISI-3682); until then the milestone routes to /credentials.
//
// Server-truth resume (NFR-3): progress comes from GET /api/onboarding/progress (E1-S1), never
// a client flag; the panel re-reads it after every successful create, so a second device sees
// the same journey position.

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import {
  ONBOARDING_MILESTONES,
  SUGGESTED_AGENT_ROLES,
  heroCopy,
  isJourneyComplete,
  isOnboardingProgress,
  milestoneStates,
  nextMilestoneId,
  resumeLabel,
  type MilestoneState,
  type OnboardingMilestone,
  type OnboardingMilestoneId,
  type OnboardingProgress,
} from "@/lib/onboarding";
import {
  emptyForm,
  isValid,
  toWire,
  validate,
  type ComposeForm,
  type ComposeResult,
  type FieldErrors,
} from "@/lib/compose";
import { TeamForm } from "@/components/compose/TeamForm";
import { AgentForm } from "@/components/compose/AgentForm";
import { ProjectForm } from "@/components/compose/ProjectForm";

/** Which on-ramp Nadia picked (FR-1.2). "template" carries the starter-squad guidance into the
 * Agents step; "manual" is the plain step-by-step path. */
type Ramp = "template" | "manual";

type SubmitState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "error"; status: number; message: string; fields: FieldErrors };

/** Parse the apiserver error body into a message + field errors — the same verbatim-honesty
 * contract ComposeScreen uses (NFR-5): server truth (401/403/404/409/422/501), never masked. */
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
    /* non-JSON — keep the status message */
  }
  if (status === 401) message = "You must sign in to compose.";
  else if (status === 403) message = message || "You don't have write access here.";
  else if (status === 404) message = "No such project / team scope for this caller.";
  else if (status === 409) message = message || "That name already exists in this team.";
  else if (status === 501) message = "Compose is not available on this deployment yet.";
  return { message, fields };
}

export function Launchpad({
  initialProgress,
  onDismiss,
  onYield,
}: {
  /** The projection the gate fetched; the hub re-reads it after every successful create. */
  initialProgress: OnboardingProgress;
  /** "Skip for now" (FR-1.3) — the gate persists dismissal and swaps to the Overview. */
  onDismiss: () => void;
  /** Frame-07 exit — the gate swaps to the normal Overview (D5 yield). */
  onYield: () => void;
}) {
  const [progress, setProgress] = useState(initialProgress);
  const [openStep, setOpenStep] = useState<OnboardingMilestoneId | null>(null);
  const [ramp, setRamp] = useState<Ramp | null>(null);
  const [lastCreated, setLastCreated] = useState<string | null>(null);

  const complete = isJourneyComplete(progress);
  const states = milestoneStates(progress);
  const hero = heroCopy(progress);
  const next = nextMilestoneId(progress);

  async function refreshProgress(): Promise<OnboardingProgress | null> {
    try {
      const res = await fetch("/api/onboarding/progress", { cache: "no-store" });
      if (!res.ok) return null;
      const p: unknown = await res.json();
      return isOnboardingProgress(p) ? p : null;
    } catch {
      return null;
    }
  }

  /** A milestone form created its object: re-read the server projection and advance the flow
   * to the new next milestone (the wizard walk). At 4/4 the hub flips to the ready state. */
  async function onCreated(result: ComposeResult) {
    setLastCreated(`${result.kind.replace(/s$/, "")} "${result.name}" created`);
    const p = await refreshProgress();
    if (!p) {
      // The CREATE succeeded but the re-read failed — keep the panel open with the honest ack
      // (the object exists server-side); the spine refreshes on the next load. Never fake a
      // done state (NFR-5).
      return;
    }
    setProgress(p);
    if (isJourneyComplete(p)) {
      setOpenStep(null);
      return;
    }
    setOpenStep(nextMilestoneId(p));
  }

  /** Manual re-check (models milestone): re-read the server projection in place — the
   * projection is server-derived, so this is the honest refresh (no client-side guessing). */
  async function recheck() {
    const p = await refreshProgress();
    if (!p) return;
    setProgress(p);
    if (isJourneyComplete(p)) setOpenStep(null);
  }

  function startFlow(which: Ramp) {
    setRamp(which);
    setLastCreated(null);
    setOpenStep(next);
  }

  if (complete) {
    return <ReadyState progress={progress} onYield={onYield} />;
  }

  const nextMilestone = ONBOARDING_MILESTONES.find((m) => m.id === next) ?? null;

  return (
    <section className="launchpad" aria-labelledby="launchpad-title">
      <header className="launchpad__header">
        <div>
          <h1 id="launchpad-title">{hero.title}</h1>
          <p className="muted">{hero.sub}</p>
        </div>
        <button type="button" className="launchpad__skip" onClick={onDismiss}>
          Skip for now
        </button>
      </header>

      <div
        className="launchpad__meter"
        role="progressbar"
        aria-valuenow={progress.done}
        aria-valuemin={0}
        aria-valuemax={progress.total}
        aria-label={`Setup progress: ${progress.done} of ${progress.total} complete`}
      >
        <div className="launchpad__meter-labels">
          <span className="launchpad__kicker">Setup progress</span>
          <span className="muted" aria-live="polite">
            {progress.done} of {progress.total} complete
          </span>
        </div>
        <div className="launchpad__meter-track">
          <div
            className="launchpad__meter-fill"
            style={{ width: `${(progress.done / progress.total) * 100}%` }}
          />
        </div>
      </div>

      <ol className="launchpad__spine" aria-label="Setup milestones">
        {ONBOARDING_MILESTONES.map((m, i) => {
          const state = states[m.id];
          return (
            <li
              key={m.id}
              className={`launchpad__spine-node launchpad__spine-node--${state}`}
              aria-current={state === "next" ? "step" : undefined}
            >
              <span className="launchpad__spine-dot" aria-hidden="true">
                {state === "done" ? "✓" : m.step}
              </span>
              <span className="launchpad__spine-label">{m.spine}</span>
              {i < ONBOARDING_MILESTONES.length - 1 && (
                <span
                  className={`launchpad__spine-link${state === "done" ? " launchpad__spine-link--done" : ""}`}
                  aria-hidden="true"
                />
              )}
            </li>
          );
        })}
      </ol>

      <ul className="launchpad__cards">
        {ONBOARDING_MILESTONES.map((m) => (
          <MilestoneCard
            key={m.id}
            milestone={m}
            state={states[m.id]}
            onStart={() => {
              setRamp((r) => r ?? "manual");
              setLastCreated(null);
              setOpenStep(m.id);
            }}
          />
        ))}
      </ul>

      {!openStep && (
        <div className="launchpad__hero">
          {progress.done === 0 ? (
            <>
              <div className="launchpad__onramps">
                <button
                  type="button"
                  className="launchpad__onramp launchpad__onramp--primary"
                  onClick={() => startFlow("template")}
                >
                  <span className="launchpad__onramp-title">⚡ Start from a starter squad</span>
                  <span className="launchpad__onramp-sub">Default · recommended</span>
                </button>
                <button
                  type="button"
                  className="launchpad__onramp"
                  onClick={() => startFlow("manual")}
                >
                  <span className="launchpad__onramp-title">🧭 Build it step by step</span>
                  <span className="launchpad__onramp-sub">Author each object yourself</span>
                </button>
              </div>
              <p className="muted launchpad__reassure">
                ≈ 3 minutes · you can pause and resume anytime
              </p>
            </>
          ) : (
            <>
              <button
                type="button"
                className="btn btn--primary launchpad__resume"
                onClick={() => startFlow(ramp ?? "manual")}
              >
                {resumeLabel(progress)} →
              </button>
              <p className="muted launchpad__reassure">
                Progress saved server-side · step {progress.step} of {progress.total}
              </p>
            </>
          )}
        </div>
      )}

      {openStep && nextMilestone && (
        <StepPanel
          key={`${openStep}-${lastCreated ?? "fresh"}`}
          milestone={ONBOARDING_MILESTONES.find((m) => m.id === openStep) ?? nextMilestone}
          ramp={ramp}
          createdNote={lastCreated}
          onClose={() => setOpenStep(null)}
          onCreated={onCreated}
          onRecheck={recheck}
        />
      )}
    </section>
  );
}

// ── Milestone card (frame 01/06) ─────────────────────────────────────────────

const STATE_LABEL: Record<MilestoneState, string> = {
  done: "Done",
  next: "Up next",
  locked: "Locked",
  todo: "To do",
};

function MilestoneCard({
  milestone,
  state,
  onStart,
}: {
  milestone: OnboardingMilestone;
  state: MilestoneState;
  onStart: () => void;
}) {
  return (
    <li
      className={`launchpad-card launchpad-card--${state}`}
      aria-disabled={state === "locked" || undefined}
    >
      <div className="launchpad-card__top">
        <span className="launchpad-card__num" aria-hidden="true">
          {state === "done" ? "✓" : milestone.step}
        </span>
        <span className={`launchpad-card__state launchpad-card__state--${state}`}>
          {STATE_LABEL[state]}
        </span>
      </div>
      <h2 className="launchpad-card__title">{milestone.title}</h2>
      <p className="launchpad-card__why">{milestone.why}</p>
      <p className="launchpad-card__tag muted">{milestone.tag}</p>
      <div className="launchpad-card__cta">
        {state === "done" && (
          <Link className="launchpad-card__review" href={milestone.reviewHref}>
            Review
          </Link>
        )}
        {state === "next" && (
          <button type="button" className="btn btn--primary" onClick={onStart}>
            Resume →
          </button>
        )}
        {state === "locked" && (
          <span className="muted">
            Locked<span className="sr-only"> — finish the previous milestone first</span>
          </span>
        )}
      </div>
    </li>
  );
}

// ── Step panel: mounts the E0 shared forms (FR-8) ────────────────────────────

type CreateKind = "teams" | "agents" | "projects";

const MILESTONE_FORM_KIND: Partial<Record<OnboardingMilestoneId, CreateKind>> = {
  team: "teams",
  agents: "agents",
  project: "projects",
};

function StepPanel({
  milestone,
  ramp,
  createdNote,
  onClose,
  onCreated,
  onRecheck,
}: {
  milestone: OnboardingMilestone;
  ramp: Ramp | null;
  createdNote: string | null;
  onClose: () => void;
  onCreated: (r: ComposeResult) => void;
  onRecheck: () => void;
}) {
  const kind = MILESTONE_FORM_KIND[milestone.id];
  return (
    <section
      className="launchpad-step"
      aria-label={`${milestone.title} — setup step ${milestone.step} of ${ONBOARDING_MILESTONES.length}`}
    >
      <div className="launchpad-step__head">
        <div>
          <span className="launchpad__kicker">
            Step {milestone.step} of {ONBOARDING_MILESTONES.length}
          </span>
          <h2>{milestone.title}</h2>
        </div>
        <button type="button" className="launchpad__skip" onClick={onClose}>
          Back to hub
        </button>
      </div>

      {createdNote && (
        <p className="state state--ok" role="status">
          {createdNote}
        </p>
      )}

      {milestone.id === "agents" && ramp === "template" && (
        <div className="launchpad-step__guide" role="note">
          <strong>Starter squad (Minimal Trio ★):</strong> create these three agents —{" "}
          {SUGGESTED_AGENT_ROLES.map((r) => (
            <span key={r.roleRef} className="launchpad-step__preset">
              <strong>{r.label}</strong> <code>{r.roleRef}</code> — {r.summary}
            </span>
          ))}
          <span className="muted">
            One-click templates arrive with the template gallery; until then the shared form
            below creates each agent (Role ref = the preset name).
          </span>
        </div>
      )}

      {milestone.id === "agents" && (
        <p className="muted">
          Each agent needs a credential Secret — create one under Settings › Credentials, then
          reference it here.
        </p>
      )}

      {milestone.id === "models" && (
        <div className="launchpad-step__models">
          <p>
            Models &amp; credentials are set per agent: a primary model, a fallback, and the
            credential Secret the agent runs with (shared squad credential by default).
          </p>
          <div className="launchpad-step__models-cta">
            <Link className="btn btn--primary" href="/credentials">
              Open Credentials →
            </Link>
            <button type="button" className="btn" onClick={onRecheck}>
              I&apos;ve wired the models — re-check
            </button>
          </div>
        </div>
      )}

      {milestone.id === "project" && (
        <p className="muted">
          The milestone completes once the project&apos;s repo carries a GitHub credential
          (<code>repo.auth</code>) — the credential-capture fields land with the project-connect
          story; a project created here counts as soon as its repo auth is set.
        </p>
      )}

      {kind && <ComposeCreatePanel kind={kind} onCreated={onCreated} />}
    </section>
  );
}

// ── Create-only host over the E0 shared forms ────────────────────────────────

function ComposeCreatePanel({
  kind,
  onCreated,
}: {
  kind: CreateKind;
  onCreated: (r: ComposeResult) => void;
}) {
  const [cf, setCf] = useState<ComposeForm>(() => emptyForm(kind));
  const [submit, setSubmit] = useState<SubmitState>({ kind: "idle" });

  const clientErrors = useMemo(() => validate(cf), [cf]);
  const serverErrors = submit.kind === "error" ? submit.fields : {};
  const errors: FieldErrors = { ...clientErrors, ...serverErrors };
  const valid = isValid(cf);

  function patch(p: Record<string, unknown>) {
    setCf((prev) => ({ kind: prev.kind, form: { ...prev.form, ...p } }) as ComposeForm);
    setSubmit({ kind: "idle" });
  }

  async function apply() {
    setSubmit({ kind: "saving" });
    const res = await fetch(`/api/compose/${kind}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(toWire(cf)),
    });
    if (res.ok) {
      onCreated((await res.json()) as ComposeResult);
    } else {
      const { message, fields } = parseError(res.status, await res.text());
      setSubmit({ kind: "error", status: res.status, message, fields });
    }
  }

  return (
    <form
      className="card compose__form launchpad-step__form"
      onSubmit={(e) => {
        e.preventDefault();
        if (valid && submit.kind !== "saving") void apply();
      }}
    >
      {cf.kind === "teams" && <TeamForm cf={cf} errors={errors} patch={patch} />}
      {cf.kind === "agents" && <AgentForm cf={cf} errors={errors} patch={patch} />}
      {cf.kind === "projects" && <ProjectForm cf={cf} errors={errors} patch={patch} />}

      <div className="compose__actions">
        <button
          type="submit"
          className="btn btn--primary"
          disabled={!valid || submit.kind === "saving"}
        >
          {submit.kind === "saving" ? "Applying…" : "Create"}
        </button>
        {submit.kind === "error" && (
          <span className="state state--error" role="alert">
            {submit.message}
          </span>
        )}
      </div>
    </form>
  );
}

// ── Ready state (frame 07, AC4 / FR-1.5) ─────────────────────────────────────

/** Best-effort names for the summary cards (squad read-model); the celebration renders fine
 * without them — names are polish, the CTAs are the point. */
type SquadNames = { teamName?: string; projectName?: string };

function ReadyState({
  progress,
  onYield,
}: {
  progress: OnboardingProgress;
  onYield: () => void;
}) {
  const [names, setNames] = useState<SquadNames>({});

  useEffect(() => {
    let cancelled = false;
    fetch("/api/squad/overview", { headers: { accept: "application/json" } })
      .then((res) => (res.ok ? res.json() : null))
      .then((data) => {
        if (cancelled || !data) return;
        const teamName = typeof data.team?.name === "string" ? data.team.name : undefined;
        const first = Array.isArray(data.projects) ? data.projects[0] : null;
        const projectName =
          first && typeof first.name === "string" ? first.name : undefined;
        setNames({ teamName, projectName });
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  const hero = heroCopy(progress);
  const summaryValue: Record<OnboardingMilestoneId, string> = {
    team: names.teamName ?? "Team created",
    agents: "Agents composed",
    models: "Models wired",
    project: names.projectName ?? "Project connected",
  };

  return (
    <section className="launchpad launchpad--ready" aria-labelledby="launchpad-title">
      <header className="launchpad__header">
        <div>
          <h1 id="launchpad-title">{hero.title}</h1>
          <p className="muted">
            {names.teamName
              ? `${names.teamName} is live with its agents, models wired, and a project connected.`
              : hero.sub}
          </p>
        </div>
      </header>

      <ul className="launchpad__cards launchpad__cards--summary">
        {ONBOARDING_MILESTONES.map((m) => (
          <li key={m.id} className="launchpad-card launchpad-card--done">
            <h2 className="launchpad-card__title">{m.spine}</h2>
            <p className="launchpad-card__why">{summaryValue[m.id]}</p>
            <p className="launchpad-card__tag muted">{m.summaryTag}</p>
          </li>
        ))}
      </ul>

      <div className="launchpad__hero">
        <div className="launchpad__onramps">
          <button
            type="button"
            className="launchpad__onramp launchpad__onramp--primary"
            onClick={onYield}
          >
            <span className="launchpad__onramp-title">Go to Overview</span>
            <span className="launchpad__onramp-sub">Your squad at a glance</span>
          </button>
          <Link className="launchpad__onramp" href="/compose">
            <span className="launchpad__onramp-title">Open Compose →</span>
            <span className="launchpad__onramp-sub">Author more objects</span>
          </Link>
        </div>
        <p className="muted launchpad__reassure">
          Tip: start your first Run from the Boss agent, or compose more objects.
        </p>
      </div>
    </section>
  );
}
