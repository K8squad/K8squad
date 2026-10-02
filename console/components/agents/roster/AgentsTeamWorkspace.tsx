"use client";

// components/agents/roster/AgentsTeamWorkspace.tsx — the client orchestrator for the Agents & Team
// redesign (ISI-5362 / S5; mockups ISI-5306). Loads the two-axis roster, owns the selected-node
// state, and lays out the rail + detail two-pane master-detail.
//
// Reads compose the shipped fleet LIST reads (lib/agents/roster.loadRoster) + the per-agent
// effective-model read; the inline editors (Model / Skills tabs, Frame 02/03) write through the
// field-scoped merge PUT (ISI-5359) and call back `onRosterChanged` so the rail's counts, role
// chips and readouts re-derive from a fresh server read after every save.

import { useCallback, useEffect, useState } from "react";

import { loadRoster, type Roster, type RosterSelection } from "@/lib/agents/roster";
import { NodeDetail } from "./NodeDetail";
import { RosterRail } from "./RosterRail";
import {
  DetailPlaceholder,
  RosterEmpty,
  RosterError,
  RosterLoading,
} from "./StateViews";

type LoadState =
  | { kind: "loading" }
  | { kind: "error" }
  | { kind: "ok"; roster: Roster };

function isEmpty(roster: Roster): boolean {
  return (
    roster.teams.length === 0 && roster.roles.length === 0 && roster.skills.length === 0
  );
}

/** `team` is the admin cross-squad selector (the ?team= act-as-team seam); tenants omit it. */
export function AgentsTeamWorkspace({ team }: { team?: string }) {
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const [selection, setSelection] = useState<RosterSelection>(null);
  // `nonce` forces a reload on Retry — and after any inline-editor save — without threading the
  // abort controller through state.
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    loadRoster(controller.signal, team)
      .then((roster) => setState({ kind: "ok", roster }))
      .catch((e: unknown) => {
        if (controller.signal.aborted) return;
        setState({ kind: "error" });
        // Keep the surface honest but quiet; the retry affordance is the recovery path.
        console.error("roster load failed", e);
      });
    return () => controller.abort();
  }, [team, nonce]);

  const retry = useCallback(() => setNonce((n) => n + 1), []);

  if (state.kind === "loading") return <RosterLoading />;
  if (state.kind === "error") return <RosterError onRetry={retry} />;
  if (isEmpty(state.roster)) return <RosterEmpty />;

  return (
    <div className="roster-workspace">
      <RosterRail
        roster={state.roster}
        selection={selection}
        onSelect={setSelection}
      />
      <div className="roster-workspace__detail">
        {selection ? (
          <NodeDetail
            roster={state.roster}
            selection={selection}
            team={team}
            onRosterChanged={retry}
          />
        ) : (
          <DetailPlaceholder />
        )}
      </div>
    </div>
  );
}
