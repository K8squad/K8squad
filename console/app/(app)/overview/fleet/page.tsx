// app/(app)/overview/fleet/page.tsx — Fleet control room (global overview, S2).
//
// Implements the approved design from mock 01-global-overview-fleet.png and
// DESIGN-SPEC-ISI-4505.md §3/§4. Composes S1 primitives to show fleet-wide status.
//
// Route: /overview/fleet (global overview)
// Features:
// - Fleet stat band (active runs · projects · open tickets · agents · tokens/24h)
// - Project cards per project (run-status + mini-metrics + current runs)
// - Recent ticket work panel (deep-links to tickets)
// - Live runs feed (fleet-wide agent runs)
//
// Data source: /api/squad/overview (existing proxy to apiserver)

"use client";

"use client";

import { useState, useEffect } from "react";
import { StatTile } from "@/components/overview/StatTile";
import { PanelCard } from "@/components/overview/PanelCard";
import { RunStatusMixBar } from "@/components/overview/RunStatusMixBar";
import Link from "next/link";

interface ProjectOverview {
  id: string;
  identifier: string;
  name: string;
  status: "active" | "inactive" | "archived";
  metrics: {
    runs: number;
    openTickets: number;
    openPRs: number;
    lastActivity: string;
  };
  currentRuns: Array<{
    id: string;
    name: string;
    status: "running" | "paused" | "blocked" | "idle";
    updatedAt: string;
  }>;
}

interface RecentTicket {
  id: string;
  title: string;
  status: "backlog" | "todo" | "in_progress" | "code_review" | "testing" | "done" | "cancelled";
  updatedAt: string;
}

interface LiveRun {
  id: string;
  agentName: string;
  projectId: string;
  projectName: string;
  status: "running" | "paused" | "blocked" | "idle";
  latestThought?: string;
  updatedAt: string;
}

interface FleetOverviewResponse {
  fleet: {
    activeRuns: number;
    projects: number;
    openTickets: number;
    activeAgents: number;
    tokens24h: number;
  };
  projects: ProjectOverview[];
  recentTickets: RecentTicket[];
  liveRuns: LiveRun[];
}

export default function FleetOverviewPage() {
  const [data, setData] = useState<FleetOverviewResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const loadData = async () => {
      try {
        const response = await fetch("/api/squad/overview", {
          cache: "no-store",
        });
        if (!response.ok) {
          throw new Error(`Failed to load fleet overview: ${response.status}`);
        }
        const result = await response.json();
        setData(result);
      } catch (err) {
        setError(err instanceof Error ? err.message : "Unknown error");
      } finally {
        setLoading(false);
      }
    };

    loadData();
  }, []);

  if (loading) {
    return (
      <div className="ov-container">
        <div className="ov-grid ov-grid--1col">
          {[1, 2, 3, 4, 5].map((i) => (
            <PanelCard key={i} title="Loading..." state="loading" />
          ))}
        </div>
      </div>
    );
  }

  if (error || !data) {
    return (
      <div className="ov-container">
        <PanelCard
          title="Fleet Overview"
          state="error"
          errorLabel="Failed to load fleet data"
          onRetry={() => window.location.reload()}
        >
          <p className="ov-text--muted">Error: {error || "Unknown error"}</p>
        </PanelCard>
      </div>
    );
  }

  return (
    <div className="ov-container">
      {/* Fleet stat band */}
      <div className="ov-stat-band">
        <StatTile
          label="Active Runs"
          value={data.fleet.activeRuns}
          sub="fleet-wide"
          glyph="⚡"
          tone="run"
        />
        <StatTile
          label="Projects"
          value={data.fleet.projects}
          sub="total"
          glyph="📁"
          tone="neutral"
        />
        <StatTile
          label="Open Tickets"
          value={data.fleet.openTickets}
          sub="across projects"
          glyph="🎯"
          tone="neutral"
        />
        <StatTile
          label="Active Agents"
          value={data.fleet.activeAgents}
          sub="running"
          glyph="🤖"
          tone="neutral"
        />
        <StatTile
          label="Tokens/24h"
          value={data.fleet.tokens24h}
          sub="consumed"
          glyph="💰"
          tone="neutral"
        />
      </div>

      {/* Projects grid */}
      <div className="ov-grid ov-grid--2col ov-grid--gap-l ov-mt-xl">
        {data.projects.map((project) => (
          <Link
            key={project.id}
            href={`/projects/${project.id}/overview`}
            className="ov-panel ov-panel--clickable"
          >
            <PanelCard
              title={project.name}
              action={{ label: "View overview →", href: `/projects/${project.id}/overview` }}
              state="ready"
            >
              <div className="ov-panel__content">
                {/* Run status summary */}
                <div className="ov-flex ov-flex--align-center ov-flex--justify-between">
                  <span className="ov-text--muted">Runs:</span>
                  <span className="ov-value">{project.metrics.runs}</span>
                </div>
                <div className="ov-flex ov-flex--align-center ov-flex--justify-between">
                  <span className="ov-text--muted">Tickets:</span>
                  <span className="ov-value">{project.metrics.openTickets}</span>
                </div>
                <div className="ov-flex ov-flex--align-center ov-flex--justify-between">
                  <span className="ov-text--muted">PRs:</span>
                  <span className="ov-value">{project.metrics.openPRs}</span>
                </div>
                <div className="ov-flex ov-flex--align-center ov-flex--justify-between">
                  <span className="ov-text--muted">Last activity:</span>
                  <span className="ov-text--muted ov-text--small">{project.metrics.lastActivity}</span>
                </div>
                <div className="ov-panel__divider">
                  <RunStatusMixBar
                    counts={{
                      running: project.currentRuns.filter(r => r.status === "running").length,
                      paused: project.currentRuns.filter(r => r.status === "paused").length,
                      blocked: project.currentRuns.filter(r => r.status === "blocked").length,
                      idle: project.currentRuns.filter(r => r.status === "idle").length,
                    }}
                  />
                </div>
                {project.currentRuns.length > 0 && (
                  <div className="ov-panel__footer">
                    <p className="ov-text--small ov-text--muted ov-mb-xs">Current runs:</p>
                    <div className="ov-grid ov-grid--1col ov-grid--gap-xs">
                      {project.currentRuns.slice(0, 3).map((run) => (
                        <div key={run.id} className="ov-text--small">
                          <span className={`ov-tone--${run.status} ov-text-mono`}>
                            {run.status}
                          </span>
                          <span className="ov-mx-xs">•</span>
                          <span className="ov-text--muted">{run.name}</span>
                        </div>
                      ))}
                      {project.currentRuns.length > 3 && (
                        <div className="ov-text--small ov-text--muted">
                          +{project.currentRuns.length - 3} more
                        </div>
                      )}
                    </div>
                  </div>
                )}
              </div>
            </PanelCard>
          </Link>
        ))}
      </div>

      {/* Two-column layout for recent tickets and live runs */}
      <div className="ov-grid ov-grid--2col ov-grid--gap-l ov-mt-xl">
        {/* Recent ticket work panel */}
        <PanelCard
          title="Recent Ticket Work"
          action={{ label: "View all tickets →", href: "/tickets" }}
          state="ready"
        >
          <div className="ov-panel__content">
            {data.recentTickets.slice(0, 5).map((ticket) => (
              <Link
                key={ticket.id}
                href={`/tickets/${ticket.id}`}
                className="ov-panel__item ov-text--muted"
              >
                <div className="ov-flex ov-flex--align-center ov-flex--justify-between">
                  <span className="ov-text">{ticket.title}</span>
                  <span className="ov-text--muted ov-text--small">{ticket.status}</span>
                </div>
                <div className="ov-text--muted ov-text--small">
                  Updated {ticket.updatedAt}
                </div>
              </Link>
            ))}
            {data.recentTickets.length === 0 && (
              <p className="ov-text--muted">No recent ticket activity</p>
            )}
          </div>
        </PanelCard>

        {/* Live runs feed */}
        <PanelCard
          title="Live Agent Runs"
          action={{ label: "View all runs →", href: "/runs" }}
          state="ready"
        >
          <div className="ov-panel__content">
            {data.liveRuns.slice(0, 5).map((run) => (
              <div key={run.id} className="ov-panel__item ov-text--muted">
                <div className="ov-flex ov-flex--align-center ov-flex--justify-between ov-mb-xs">
                  <span className="ov-text">{run.agentName}</span>
                  <span className={`ov-tone--${run.status} ov-text-mono`}>
                    {run.status}
                  </span>
                </div>
                <div className="ov-text--muted ov-text--small">
                  {run.projectName} • Updated {run.updatedAt}
                </div>
                {run.latestThought && (
                  <div className="ov-text--small ov-text--muted ov-line-clamp-2">
                    {run.latestThought}
                  </div>
                )}
              </div>
            ))}
            {data.liveRuns.length === 0 && (
              <p className="ov-text--muted">No active agent runs</p>
            )}
          </div>
        </PanelCard>
      </div>
    </div>
  );
}