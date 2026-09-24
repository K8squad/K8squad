"use client";

// lib/useEffectiveModel.ts — the read-only effective-model client for the Agent
// form's EffectiveModelReadout (ISI-4892 / S3, epic ISI-4822 Flow C).
//
// One GET against the BFF proxy (`/api/squad/agents/{name}/effective-model`),
// which projects the SHIPPED Model-Per-Role resolver (Agent → Role → org-default,
// ISI-4430). The precedence rule lives ONCE, in Go; this hook only renders what
// the route returns — it never re-derives "which tier wins" client-side (Option B,
// story §"Implementation decision").
//
// Read-only: no write, no mutate. Keyed by the PERSISTED agent name — in create
// mode (no persisted agent yet) the caller passes no name and the hook is inert.
// A 404 (name not found / not-yet-persisted / out of scope) is existence-hiding,
// surfaced as `notFound` so the read-out simply hides rather than alarming.

import { useEffect, useState } from "react";

// ProvenanceTier is re-declared here (not imported from ProvenanceChip) so the
// hook has no component dependency; it is the same three-value union the wire and
// the chip agree on.
export type EffectiveTier = "agent" | "role" | "default";

export type EffectiveModel = {
  model: string;
  tier: EffectiveTier;
  roleName?: string;
  fallbackModel?: string;
  unresolved?: boolean;
};

export type UseEffectiveModel = {
  data: EffectiveModel | null;
  loading: boolean;
  /** true when the route answered 404 (unpersisted / out-of-scope) — the read-out hides. */
  notFound: boolean;
  /** a non-404 fetch/transport error — the read-out shows a terse, non-blocking line. */
  error: string | null;
};

const TIERS: ReadonlySet<string> = new Set(["agent", "role", "default"]);

// coerce validates the wire body defensively (the read-out renders only a
// server-stamped tier; an unknown shape degrades to an error, never a crash).
function coerce(raw: unknown): EffectiveModel | null {
  if (typeof raw !== "object" || raw === null) return null;
  const r = raw as Record<string, unknown>;
  const unresolved = r.unresolved === true;
  const roleName = typeof r.roleName === "string" ? r.roleName : undefined;
  const fallbackModel = typeof r.fallbackModel === "string" ? r.fallbackModel : undefined;
  if (unresolved) {
    return { model: "", tier: "default", roleName, fallbackModel, unresolved: true };
  }
  const model = typeof r.model === "string" ? r.model : "";
  const tier = typeof r.tier === "string" && TIERS.has(r.tier) ? (r.tier as EffectiveTier) : null;
  if (!model || !tier) return null;
  return { model, tier, roleName, fallbackModel };
}

/**
 * useEffectiveModel fetches the effective model + provenance tier for one agent.
 * Pass an empty/undefined name (create mode) and the hook stays inert (`data`
 * null, not loading). `team` scopes an admin's cross-squad read (the ?team=
 * selector); tenants omit it.
 */
export function useEffectiveModel(agentName?: string, team?: string): UseEffectiveModel {
  const [state, setState] = useState<UseEffectiveModel>({
    data: null,
    loading: false,
    notFound: false,
    error: null,
  });

  useEffect(() => {
    const name = (agentName ?? "").trim();
    if (!name) {
      setState({ data: null, loading: false, notFound: false, error: null });
      return;
    }
    const controller = new AbortController();
    setState({ data: null, loading: true, notFound: false, error: null });

    const qs = team ? `?team=${encodeURIComponent(team)}` : "";
    fetch(`/api/squad/agents/${encodeURIComponent(name)}/effective-model${qs}`, {
      signal: controller.signal,
      headers: { accept: "application/json" },
    })
      .then(async (res) => {
        if (res.status === 404) {
          setState({ data: null, loading: false, notFound: true, error: null });
          return;
        }
        if (!res.ok) {
          setState({ data: null, loading: false, notFound: false, error: `effective-model unavailable (${res.status})` });
          return;
        }
        const body = coerce(await res.json());
        if (!body) {
          setState({ data: null, loading: false, notFound: false, error: "unexpected effective-model response" });
          return;
        }
        setState({ data: body, loading: false, notFound: false, error: null });
      })
      .catch((e: unknown) => {
        if (controller.signal.aborted) return; // a name change / unmount aborted the in-flight read
        setState({ data: null, loading: false, notFound: false, error: e instanceof Error ? e.message : "network error" });
      });

    return () => controller.abort();
  }, [agentName, team]);

  return state;
}
