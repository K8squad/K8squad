"use client";

// components/SkillsList.tsx — the Skills surface body (ISI-3962 S2, follow-up to ISI-3961 S1).
//
// The Compose left pane already lists skills inline (ISI-3964 wired useOrgList's "skills" branch to
// GET /api/squad/skills), but there was no place to INSPECT a skill's capability envelope outside the
// authoring form. This is that surface: the fleet-aware list the GET /api/squad/skills route answers,
// proxied by the BFF (app/api/squad/skills), with a row-expands-to-detail view backed by
// GET /api/squad/skills/{name} (app/api/squad/skills/[name]).
//
// Scoping is AUTHORITATIVE in the apiserver — admin ⇒ fleet-wide (ADR-0010), tenant ⇒ their Team
// namespace — so this screen never asks for or receives a Team selector; cross-Team data is absent by
// construction, not filtered client-side (AC5). The list row carries only the fields SkillListEntry
// projects (name, namespace, sourceType); the OWNING-TEAM deep link (/agents?team={teamUid}, ISI-3943
// AC2 idiom) lives on the expanded SkillView, which is the projection that carries teamUid/teamName.
// Every terminal HTTP state the BFF relays gets a distinct honest rendering, mirroring ProjectsList.

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { EmptyState } from "@/components/forms/EmptyState";
import { classifyOverviewStatus } from "@/components/SquadOverview";

/** GET /api/squad/skills row (apiserver SkillListEntry, fleetlist.go). `skills` arrives non-null on
 * the wire (initialized to an empty slice), but we treat it defensively as `| null` so an older
 * apiserver that marshals nil-as-null renders the empty state rather than crashing. */
export interface SkillsListData {
  skills:
    | {
        name: string;
        namespace: string;
        uid?: string;
        sourceType?: string;
        permissions?: string[];
      }[]
    | null;
  fleet?: boolean;
}

/** GET /api/squad/skills/{name} detail (apiserver SkillView, fleetlist.go). Slice fields arrive
 * non-null on the wire ([] never null); we still default them defensively when rendering. */
export interface SkillViewData {
  name: string;
  namespace: string;
  uid?: string;
  teamUid?: string;
  teamName?: string;
  sourceType?: string;
  repoRef?: string;
  ref?: string;
  path?: string;
  mcpToolRefs?: string[];
  permissions?: string[];
  toolchains?: string[];
  sidecars?: string[];
}

type LoadState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "no-team" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: SkillsListData };

type DetailState =
  | { kind: "idle" }
  | { kind: "loading"; name: string }
  | { kind: "error"; name: string; status: number }
  | { kind: "ready"; name: string; data: SkillViewData };

export function SkillsList() {
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const [detail, setDetail] = useState<DetailState>({ kind: "idle" });

  useEffect(() => {
    let alive = true;
    fetch("/api/squad/skills", { headers: { accept: "application/json" } })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          setState(classifyOverviewStatus(res.status) as LoadState);
          return;
        }
        setState({ kind: "ready", data: (await res.json()) as SkillsListData });
      })
      .catch(() => {
        if (alive) setState({ kind: "error", status: 0 });
      });
    return () => {
      alive = false;
    };
  }, []);

  // Toggle a row: collapse if already open, else fetch its single-skill view (AC4). The name is the
  // list key the {name} route resolves; on a name collision the apiserver picks deterministically
  // (ns order), so admin fleet browsing stays stable.
  const openDetail = useCallback(
    (name: string) => {
      if (detail.kind !== "idle" && detail.name === name) {
        setDetail({ kind: "idle" });
        return;
      }
      setDetail({ kind: "loading", name });
      let alive = true;
      fetch(`/api/squad/skills/${encodeURIComponent(name)}`, {
        headers: { accept: "application/json" },
      })
        .then(async (res) => {
          if (!alive) return;
          if (!res.ok) {
            setDetail({ kind: "error", name, status: res.status });
            return;
          }
          setDetail({ kind: "ready", name, data: (await res.json()) as SkillViewData });
        })
        .catch(() => {
          if (alive) setDetail({ kind: "error", name, status: 0 });
        });
    },
    [detail],
  );

  if (state.kind === "loading") {
    return (
      <div className="card" data-testid="skills-loading">
        Loading skills…
      </div>
    );
  }
  if (state.kind === "unauthenticated") {
    return (
      <div className="card" data-testid="skills-unauthenticated">
        <h2 style={{ marginTop: 0 }}>Sign in required</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          The Skills list is scoped to your session. Authenticate through the console sign-in flow
          and reload.
        </p>
      </div>
    );
  }
  if (state.kind === "no-team") {
    return (
      <div className="card" data-testid="skills-no-team">
        <h2 style={{ marginTop: 0 }}>No squad for your Team yet</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          Your session resolves to a Team with no projection — the Team may be newly created (or
          deleted) and the cache has not observed it yet.
        </p>
      </div>
    );
  }
  if (state.kind === "not-wired") {
    return (
      <div className="card" data-testid="skills-not-wired">
        <h2 style={{ marginTop: 0 }}>Skills list not wired</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          This apiserver runs without the skills read model (dev / cluster-less run) and answers its
          documented 501.
        </p>
      </div>
    );
  }
  if (state.kind === "error") {
    return (
      <div className="card" data-testid="skills-error">
        <h2 style={{ marginTop: 0 }}>Skills unavailable</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          The read model could not be reached (HTTP {state.status || "network error"}). Retry
          shortly.
        </p>
      </div>
    );
  }

  const skills = state.data.skills ?? [];
  return (
    <div data-testid="skills-ready">
      <header className="card">
        <h1 style={{ margin: 0 }}>Skills</h1>
        <p className="muted" style={{ margin: "6px 0 0" }}>
          {state.data.fleet
            ? "Fleet-wide — every squad's registered skills across the cluster."
            : "Your squad's registered skills. Open one to inspect its capability envelope."}
        </p>
      </header>

      {skills.length === 0 ? (
        <EmptyState
          testId="skills-empty"
          title="No Skills yet"
          why="No Skills in your Team's namespace yet."
          ctaLabel="Compose a skill"
          onCta={() => (window.location.href = "/compose?kind=skill")}
        />
      ) : (
        skills.map((s) => {
          const open = detail.kind !== "idle" && detail.name === s.name;
          return (
            <section
              className="card"
              key={`${s.namespace}/${s.name}`}
              data-testid="skills-row"
              data-namespace={s.namespace}
            >
              <button
                type="button"
                className="skills-row__toggle"
                aria-expanded={open}
                data-testid="skills-row-toggle"
                onClick={() => openDetail(s.name)}
                style={{
                  background: "none",
                  border: "none",
                  padding: 0,
                  cursor: "pointer",
                  textAlign: "left",
                  width: "100%",
                }}
              >
                <h2 style={{ margin: "0 0 4px" }}>{s.name}</h2>
                <p className="muted" style={{ margin: 0, fontSize: 13 }}>
                  {s.sourceType ? (
                    <>
                      <code>{s.sourceType}</code>
                      {" · "}
                    </>
                  ) : null}
                  {state.data.fleet ? (
                    <>
                      Squad ns <code>{s.namespace}</code>
                    </>
                  ) : (
                    <code>{s.namespace}</code>
                  )}
                </p>
              </button>

              {open ? <SkillDetail detail={detail} fleet={!!state.data.fleet} /> : null}
            </section>
          );
        })
      )}
    </div>
  );
}

/** The expanded single-skill view (AC4): source provenance + capability envelope. For an admin
 * (fleet), the owning Team is surfaced with the ISI-3943 AC2 deep link into that squad's agents. */
function SkillDetail({ detail, fleet }: { detail: DetailState; fleet: boolean }) {
  if (detail.kind === "loading") {
    return (
      <p className="muted" data-testid="skills-detail-loading" style={{ marginTop: 8 }}>
        Loading skill…
      </p>
    );
  }
  if (detail.kind === "error") {
    return (
      <p className="muted" data-testid="skills-detail-error" style={{ marginTop: 8 }}>
        Could not load this skill (HTTP {detail.status || "network error"}).
      </p>
    );
  }
  if (detail.kind !== "ready") return null;

  const d = detail.data;
  const mcp = d.mcpToolRefs ?? [];
  const perms = d.permissions ?? [];
  const toolchains = d.toolchains ?? [];
  const sidecars = d.sidecars ?? [];

  return (
    <div data-testid="skills-detail" style={{ marginTop: 8, borderTop: "1px solid var(--border, #333)", paddingTop: 8 }}>
      <dl className="skills-detail__meta" style={{ margin: 0 }}>
        <Field label="Source">
          <code>{d.sourceType || "—"}</code>
          {d.repoRef ? (
            <>
              {" · "}
              <code data-testid="skills-detail-repo">
                {d.repoRef}
                {d.ref ? `@${d.ref}` : ""}
                {d.path ? `:${d.path}` : ""}
              </code>
            </>
          ) : null}
        </Field>
        {fleet && d.teamName ? (
          <Field label="Owning squad">
            <code>{d.teamName}</code>
            {d.teamUid ? (
              <>
                {" · "}
                <Link
                  href={`/agents?team=${encodeURIComponent(d.teamUid)}`}
                  className="muted"
                  data-testid="skills-detail-team-link"
                >
                  View squad agents →
                </Link>
              </>
            ) : null}
          </Field>
        ) : null}
        <Field label="MCP tools">
          <ChipList items={mcp} testId="skills-detail-mcp" empty="none granted" />
        </Field>
        <Field label="Permissions">
          <ChipList items={perms} testId="skills-detail-perms" empty="none" />
        </Field>
        <Field label="Toolchains">
          <ChipList items={toolchains} testId="skills-detail-toolchains" empty="none" />
        </Field>
        <Field label="Sidecars">
          <ChipList items={sidecars} testId="skills-detail-sidecars" empty="none" />
        </Field>
      </dl>
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div style={{ display: "flex", gap: 8, margin: "4px 0", fontSize: 13 }}>
      <dt className="muted" style={{ minWidth: 110, flexShrink: 0 }}>
        {label}
      </dt>
      <dd style={{ margin: 0 }}>{children}</dd>
    </div>
  );
}

function ChipList({ items, testId, empty }: { items: string[]; testId: string; empty: string }) {
  if (items.length === 0) return <span className="muted">{empty}</span>;
  return (
    <span style={{ display: "inline-flex", gap: 6, flexWrap: "wrap" }} data-testid={testId}>
      {items.map((it) => (
        <code key={it}>{it}</code>
      ))}
    </span>
  );
}
