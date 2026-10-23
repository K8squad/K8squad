"use client";

// components/overview/ProjectOverviewDashboard.tsx — the per-project overview, redesigned to
// Mock D (ISI-5709 WS-4, parent plan ISI-5697 rev2, approved design ISI-5699). Replaces the old
// Frame 02 (ISI-4508) time-series dashboard with a leaner control room: "few high-level details
// per project with deep links" (mock D). It composes the WS-1a/1b/WS-2 contracts that landed
// first:
//   ① a 5-up stat band — Active Runs (GET /api/squad/overview), Open Issues
//      (GET /api/projects/{id}/work-items), the REAL Open PRs count + Red Checks
//      (GET /api/squad/github, this project's repo row — no more hard-coded "—"), and Agents
//      (the size of the agent-card grid);
//   ② a per-project GitHub strip — assigned / open issues / open+reviewed+merged PRs / red
//      checks for this repo, with an "Open on GitHub ↗" deep link;
//   ③ the Paperclip-style agent-card grid — one AgentRunCard (WS-2) per agent working THIS
//      project, folded from the squad-wide GET /api/squad/agents/activity (WS-1a) by deep-link
//      attribution (lib/overview/projectActivity), so the opaque `intake-<uuid>` run list is
//      gone and each card shows the SERVER-classified last-thinking line, never the raw
//      [run]/[tool] stream (ISI-5695/5696);
//   ④ a Jump-into-Issues CTA + a Latest Tickets panel, both deep-linking the ISI-4231 routes.
//
// Every panel owns its async boundary and its loading/empty/error state (PanelCard / StatTile
// skeletons), so a slow or unrolled read degrades ONLY its own tile — never the page
// (degrade-don't-blank, ISI-4229). Fabrication discipline (FR-I3): an UNAVAILABLE source renders
// "—", only an authoritative zero renders 0.

import { useEffect, useMemo, useState } from "react";
import { phaseTone, type SquadOverviewData } from "@/components/SquadOverview";
import {
  selectProject,
  recentTickets,
  ticketStats,
} from "@/components/projects/ProjectLanding";
import { ApiError, listWorkItems } from "@/lib/tickets/api";
import { STATE_LABELS, type WorkItem } from "@/lib/tickets/types";
import { encodeProjectId } from "@/lib/projectId";
import {
  StatBand,
  StatTile,
  PanelCard,
  AgentRunCard,
  AgentRunCardGrid,
} from "@/components/overview";
import {
  selectRepoStat,
  repoCount,
  type GithubSummary,
  type GithubRepoStat,
} from "@/lib/overview/githubSummary";
import { projectAgents } from "@/lib/overview/projectActivity";
import type { AgentActivity } from "@/lib/overview/agentActivity";
import "@/components/overview/overview.css";
import "@/components/overview/dashboard.css";

const TICKETS_LIMIT = 6;

// ---------------------------------------------------------------------------
// Pure derivations (exported for unit tests — no fetch, no DOM).
// ---------------------------------------------------------------------------

/** Live-work runs for this project (phaseTone "running" is the single authority). */
export function activeRunCount(
  project: ReturnType<typeof selectProject>,
): number {
  return (project?.runs ?? []).filter((r) => phaseTone(r.phase) === "running")
    .length;
}

// ---------------------------------------------------------------------------
// Data hooks — each an independent async boundary (S3's degrade-don't-blank discipline).
// ---------------------------------------------------------------------------

type OverviewState =
  | { kind: "loading" }
  | { kind: "error"; status: number }
  | { kind: "not-available" }
  | { kind: "ready"; data: SquadOverviewData };

function useSquadOverview(): OverviewState {
  const [state, setState] = useState<OverviewState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    fetch("/api/squad/overview", { headers: { accept: "application/json" } })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          setState(
            res.status === 501 || res.status === 404
              ? { kind: "not-available" }
              : { kind: "error", status: res.status },
          );
          return;
        }
        setState({
          kind: "ready",
          data: (await res.json()) as SquadOverviewData,
        });
      })
      .catch(() => alive && setState({ kind: "error", status: 0 }));
    return () => {
      alive = false;
    };
  }, []);
  return state;
}

type WorkItemsState =
  | { kind: "loading" }
  | { kind: "not-available" }
  | { kind: "error"; status: number }
  | { kind: "ready"; items: WorkItem[] };

function useWorkItems(projectId: string): WorkItemsState {
  const [state, setState] = useState<WorkItemsState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    listWorkItems(projectId)
      .then((items) => alive && setState({ kind: "ready", items }))
      .catch((err: unknown) => {
        if (!alive) return;
        const status = err instanceof ApiError ? err.status : 0;
        setState(
          status === 501 || status === 404
            ? { kind: "not-available" }
            : { kind: "error", status },
        );
      });
    return () => {
      alive = false;
    };
  }, [projectId]);
  return state;
}

type GithubState =
  | { kind: "loading" }
  | { kind: "not-available" }
  | { kind: "error"; status: number }
  | { kind: "ready"; summary: GithubSummary };

function useGithubSummary(): GithubState {
  const [state, setState] = useState<GithubState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    fetch("/api/squad/github", { headers: { accept: "application/json" } })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          setState(
            res.status === 501 || res.status === 404
              ? { kind: "not-available" }
              : { kind: "error", status: res.status },
          );
          return;
        }
        setState({
          kind: "ready",
          summary: (await res.json()) as GithubSummary,
        });
      })
      .catch(() => alive && setState({ kind: "error", status: 0 }));
    return () => {
      alive = false;
    };
  }, []);
  return state;
}

type ActivityState =
  | { kind: "loading" }
  | { kind: "not-available" }
  | { kind: "error"; status: number }
  | { kind: "ready"; agents: AgentActivity[] };

function useAgentActivity(): ActivityState {
  const [state, setState] = useState<ActivityState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    fetch("/api/squad/agents/activity", {
      headers: { accept: "application/json" },
    })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          setState(
            res.status === 501 || res.status === 404
              ? { kind: "not-available" }
              : { kind: "error", status: res.status },
          );
          return;
        }
        const data = (await res.json()) as { agents?: AgentActivity[] };
        setState({ kind: "ready", agents: data.agents ?? [] });
      })
      .catch(() => alive && setState({ kind: "error", status: 0 }));
    return () => {
      alive = false;
    };
  }, []);
  return state;
}

// ---------------------------------------------------------------------------
// Panels
// ---------------------------------------------------------------------------

/** One GitHub-strip chip: a label + its count, degrading to "—" (FR-I3) via repoCount. */
function GithubChip({
  label,
  stat,
  field,
  tone,
}: {
  label: string;
  stat: GithubRepoStat | null;
  field: keyof GithubRepoStat;
  tone?: "running" | "blocked";
}) {
  const value = repoCount(stat, field);
  return (
    <div className="ov-ghstrip__chip" data-testid={`gh-chip-${String(field)}`}>
      <span className={`ov-ghstrip__value${tone ? ` ov-tone--${tone}` : ""}`}>
        {value}
      </span>
      <span className="ov-ghstrip__label">{label}</span>
    </div>
  );
}

function GithubStrip({
  state,
  repoUrl,
}: {
  state: GithubState;
  repoUrl: string | null;
}) {
  const stat =
    state.kind === "ready" ? selectRepoStat(state.summary, repoUrl) : null;
  return (
    <PanelCard
      title="GitHub"
      action={
        repoUrl ? { label: "Open on GitHub ↗", href: repoUrl } : undefined
      }
      state={
        state.kind === "loading"
          ? "loading"
          : state.kind === "error"
            ? "error"
            : state.kind === "not-available"
              ? "empty"
              : "ready"
      }
      emptyLabel="GitHub summary not available yet on this deployment."
      errorLabel={`GitHub summary unavailable (HTTP ${state.kind === "error" ? state.status || "network error" : ""}).`}
    >
      {state.kind === "ready" ? (
        stat === null ? (
          <p
            className="muted"
            style={{ margin: 0 }}
            data-testid="gh-strip-no-repo"
          >
            No mirrored GitHub repository for this project yet.
          </p>
        ) : (
          <div className="ov-ghstrip" data-testid="gh-strip">
            <GithubChip label="Assigned" stat={stat} field="assigned" />
            <GithubChip label="Open issues" stat={stat} field="open" />
            <GithubChip
              label="Open PRs"
              stat={stat}
              field="prs"
              tone="running"
            />
            <GithubChip label="Reviewed" stat={stat} field="reviewed" />
            <GithubChip
              label="Red checks"
              stat={stat}
              field="checksRed"
              tone="blocked"
            />
          </div>
        )
      ) : null}
    </PanelCard>
  );
}

function AgentGridPanel({
  state,
  projectId,
  now,
}: {
  state: ActivityState;
  projectId: string;
  now?: number;
}) {
  const agents =
    state.kind === "ready" ? projectAgents(state.agents, projectId) : [];
  return (
    <PanelCard
      title="Agents on this project"
      state={
        state.kind === "loading"
          ? "loading"
          : state.kind === "error"
            ? "error"
            : state.kind === "not-available"
              ? "empty"
              : agents.length === 0
                ? "empty"
                : "ready"
      }
      emptyLabel={
        state.kind === "not-available"
          ? "Agent activity read model not available yet on this deployment."
          : "No agents are working on this project right now."
      }
      errorLabel={`Agent activity unavailable (HTTP ${state.kind === "error" ? state.status || "network error" : ""}).`}
    >
      {state.kind === "ready" && agents.length > 0 ? (
        <AgentRunCardGrid>
          {agents.map((a) => (
            <AgentRunCard key={a.agentId || a.name} agent={a} now={now} />
          ))}
        </AgentRunCardGrid>
      ) : null}
    </PanelCard>
  );
}

function LatestTicketsPanel({
  state,
  projectId,
}: {
  state: WorkItemsState;
  projectId: string;
}) {
  const encoded = encodeProjectId(projectId);
  return (
    <PanelCard
      title="Latest Tickets"
      action={{
        label: "Jump into Issues →",
        href: `/projects/${encoded}/issues`,
      }}
      state={
        state.kind === "loading"
          ? "loading"
          : state.kind === "error"
            ? "error"
            : state.kind === "not-available"
              ? "empty"
              : "ready"
      }
      emptyLabel="Ticket read model not available yet on this deployment."
      errorLabel={`Tickets unavailable (HTTP ${state.kind === "error" ? state.status || "network error" : ""}).`}
    >
      {state.kind === "ready"
        ? (() => {
            const latest = recentTickets(state.items, TICKETS_LIMIT);
            if (latest.length === 0)
              return (
                <p className="muted" style={{ margin: 0 }}>
                  No tickets yet.
                </p>
              );
            return (
              <ul className="ov-list" data-testid="project-latest-tickets">
                {latest.map((t) => (
                  <li key={t.id} className="ov-list__row">
                    <a
                      href={`/projects/${encoded}/issues/${encodeURIComponent(t.id)}`}
                      data-testid="project-ticket-link"
                    >
                      {t.title}
                    </a>
                    <span
                      className="phase-chip"
                      data-tone={t.state === "done" ? "idle" : "running"}
                    >
                      {STATE_LABELS[t.state]}
                    </span>
                  </li>
                ))}
              </ul>
            );
          })()
        : null}
    </PanelCard>
  );
}

// ---------------------------------------------------------------------------
// Assembly
// ---------------------------------------------------------------------------

export function ProjectOverviewDashboard({ projectId }: { projectId: string }) {
  const overview = useSquadOverview();
  const workItems = useWorkItems(projectId);
  const github = useGithubSummary();
  const activity = useAgentActivity();
  const encoded = encodeProjectId(projectId);

  const project = useMemo(
    () =>
      overview.kind === "ready"
        ? selectProject(overview.data, projectId)
        : null,
    [overview, projectId],
  );
  const repoUrl = project?.repoUrl ?? null;
  const repoStat =
    github.kind === "ready" ? selectRepoStat(github.summary, repoUrl) : null;

  const openIssues =
    workItems.kind === "ready" ? ticketStats(workItems.items).open : null;
  const agentCount =
    activity.kind === "ready"
      ? projectAgents(activity.agents, projectId).length
      : null;

  return (
    <div className="ov-dashboard" data-testid="project-overview">
      <StatBand>
        <StatTile
          label="Active Runs"
          value={overview.kind === "ready" ? activeRunCount(project) : "—"}
          loading={overview.kind === "loading"}
          tone="running"
          sub="in flight"
          glyph="⚡"
        />
        <StatTile
          label="Open Issues"
          value={openIssues ?? "—"}
          loading={workItems.kind === "loading"}
          sub={openIssues === null ? "read model unavailable" : "on the board"}
        />
        <StatTile
          label="Open PRs"
          value={repoCount(repoStat, "prs")}
          loading={github.kind === "loading"}
          tone="running"
          sub={repoStat ? "awaiting merge" : "no mirrored repo"}
        />
        <StatTile
          label="Agents"
          value={agentCount ?? "—"}
          loading={activity.kind === "loading"}
          sub={agentCount === null ? "unavailable" : "on this project"}
        />
        <StatTile
          label="Red Checks"
          value={repoCount(repoStat, "checksRed")}
          loading={github.kind === "loading"}
          tone="blocked"
          sub={repoStat ? "failing CI" : "no mirrored repo"}
        />
      </StatBand>

      <GithubStrip state={github} repoUrl={repoUrl} />

      <AgentGridPanel state={activity} projectId={projectId} />

      <div className="ov-bottom-row">
        <LatestTicketsPanel state={workItems} projectId={projectId} />
        <PanelCard title="Jump in">
          <div className="ov-cta-row" data-testid="project-cta">
            <a
              className="ov-cta"
              href={`/projects/${encoded}/issues`}
              data-testid="cta-issues"
            >
              Jump into Issues →
            </a>
            <a
              className="ov-cta ov-cta--ghost"
              href={`/projects/${encoded}/runs`}
              data-testid="cta-runs"
            >
              View runs →
            </a>
            {repoUrl ? (
              <a
                className="ov-cta ov-cta--ghost"
                href={repoUrl}
                data-testid="cta-github"
              >
                Open on GitHub ↗
              </a>
            ) : null}
          </div>
        </PanelCard>
      </div>
    </div>
  );
}
