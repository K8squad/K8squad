// components/overview/AgentRunCard.tsx — the Paperclip-style agent card that replaces the
// opaque `intake-<uuid>` run list on the overview surfaces (ISI-5707 WS-2, parent plan
// ISI-5697 rev2, approved design ISI-5699 mock B).
//
// Pure presentational: it renders ONE agent's current-or-last run from the WS-1a read
// model (lib/overview/agentActivity) — avatar + name/role, a semantic phase chip, the
// CLASSIFIED last-thinking line (never the raw [run]/[tool] stream — ISI-5695/5696), a
// work-item/discussion deep-link chip, and the relative last-activity time. The run id is
// still reachable (the chip jumps to the work it did) but demoted out of the primary label.
//
// Five states, all first-class (mock B bottom row), so a card never renders blank
// (degrade-don't-blank, ISI-4229) and never fabricates (FR-I3):
//   running — live phase chip + current thinking
//   idle    — the agent's LAST run (thinking + ticket + time)
//   blocked — red chip; the blocking reason rides the thinking line
//   empty   — agent never ran → avatar outline + "No run yet — idle since onboarding"
//   loading — skeleton (avatar circle + shimmer bars), never a spinner-blank

import {
  type AgentActivity,
  type AgentCardState,
  agentInitials,
  avatarColor,
  relativeActivityShort,
  resolveCardState,
  resolveDeepLink,
  thinkingLine,
} from "@/lib/overview/agentActivity";
import { toneClass, type Tone } from "./tone";
import "./overview.css";

/** Map a card state to the semantic tone that paints its chip (reuses the shipped status hues). */
function stateTone(state: AgentCardState): Tone {
  switch (state) {
    case "running":
      return "running";
    case "blocked":
      return "blocked";
    default:
      return "idle";
  }
}

const EMPTY_LABEL = "No run yet — idle since onboarding";

export function AgentRunCard({
  agent,
  loading = false,
  /** Injected clock (ms) for the relative-time line — keeps render pure/testable & SSR-stable. */
  now,
}: {
  agent?: AgentActivity | null;
  loading?: boolean;
  now?: number;
}) {
  const state = resolveCardState(agent, loading);

  if (state === "loading") {
    return (
      <div className="ov-agentcard ov-agentcard--loading" aria-hidden="true">
        <div className="ov-agentcard__head">
          <span className="ov-skel ov-agentcard__avatar-skel" />
          <div className="ov-agentcard__id">
            <span className="ov-skel ov-skel--value" />
            <span className="ov-skel ov-skel--sub" />
          </div>
        </div>
        <div className="ov-agentcard__think">
          <span className="ov-skel ov-agentcard__think-skel" />
          <span className="ov-skel ov-agentcard__think-skel" />
        </div>
      </div>
    );
  }

  // Past loading, `agent` is present (resolveCardState returns "loading" otherwise).
  const a = agent as AgentActivity;
  const run = a.currentOrLastRun ?? null;
  const tone = stateTone(state);
  const isEmpty = state === "empty";
  const link = resolveDeepLink(run);
  const nowMs = now ?? Date.now();
  const relTime = relativeActivityShort(run?.lastActivityAt, nowMs);

  return (
    <div
      className={`ov-agentcard ov-agentcard--${state}`}
      role="group"
      aria-label={`${a.name}${a.role ? ` · ${a.role}` : ""} — ${state}`}
      data-testid="agent-run-card"
      data-state={state}
    >
      <div className="ov-agentcard__head">
        <span
          className={`ov-agentcard__avatar${isEmpty ? " ov-agentcard__avatar--empty" : ""}`}
          style={isEmpty ? undefined : { background: avatarColor(a.agentId || a.name) }}
          aria-hidden="true"
        >
          {isEmpty ? "" : agentInitials(a.name)}
        </span>
        <div className="ov-agentcard__id">
          <span className="ov-agentcard__name">{a.name}</span>
          {a.role ? (
            <span className="ov-agentcard__role" data-testid="agent-role" title={a.role}>
              <svg
                className="ov-agentcard__role-glyph"
                viewBox="0 0 16 16"
                width="11"
                height="11"
                aria-hidden="true"
                focusable="false"
              >
                <path
                  fill="currentColor"
                  d="M8 8a3 3 0 1 0 0-6 3 3 0 0 0 0 6Zm0 1.5c-2.9 0-5.5 1.5-5.5 3.4V14h11v-1.1c0-1.9-2.6-3.4-5.5-3.4Z"
                />
              </svg>
              <span className="ov-agentcard__role-text">{a.role}</span>
            </span>
          ) : null}
        </div>
        {!isEmpty && (
          <span className={`ov-agentcard__chip ${toneClass(tone)}`} data-testid="agent-phase-chip">
            <span className="ov-agentcard__chip-dot" aria-hidden="true" />
            {state}
          </span>
        )}
      </div>

      {isEmpty ? (
        <p className="ov-agentcard__empty" data-testid="agent-empty">
          {EMPTY_LABEL}
        </p>
      ) : (
        <>
          <div className="ov-agentcard__think" data-testid="agent-thinking">
            <span className="ov-agentcard__think-label">Last thinking</span>
            <p className="ov-agentcard__think-text" title={thinkingLine(state, run)}>
              {thinkingLine(state, run)}
            </p>
          </div>
          <div className="ov-agentcard__foot">
            {link ? (
              <a
                className="ov-agentcard__workchip"
                href={link.href}
                data-testid="agent-workchip"
                data-kind={link.kind}
              >
                {link.label} <span aria-hidden="true">↗</span>
              </a>
            ) : (
              <span className="ov-agentcard__workchip ov-agentcard__workchip--none">—</span>
            )}
            <time className="ov-agentcard__time" data-testid="agent-time">
              {relTime}
            </time>
          </div>
        </>
      )}
    </div>
  );
}

/** Responsive 3-up grid wrapper for the Current-runs agent-card panel (mock A ④ / mock D ④). */
export function AgentRunCardGrid({ children }: { children: React.ReactNode }) {
  return <div className="ov-agentcard-grid">{children}</div>;
}
