"use client";

// components/compose/EffectiveModelReadout.tsx — the read-only "which model will
// this agent run, and why?" projection on the Agent form (ISI-4892 / S3, epic
// ISI-4822 Flow C). It slots beneath <ModelSelector> and answers the precedence
// question (Agent override → Role → Org default) at a glance, so no one has to
// read three CRDs by hand to know what an inherited agent actually runs.
//
// It is a PURE PROJECTION: the winning tier comes from the server-side resolver
// (via useEffectiveModel → the BFF read route), and provenance is painted by the
// shared <ProvenanceChip> atom (S4 / ISI-4893). This component re-derives nothing
// and writes nothing — read-only in v1 (OQ3). Editing the model stays in
// <ModelSelector>; this widget is additive and never mutates form state.

import { useEffectiveModel } from "@/lib/useEffectiveModel";
import { ProvenanceChip } from "./ProvenanceChip";

interface EffectiveModelReadoutProps {
  // The PERSISTED agent name to resolve against. Absent (create mode / onboarding)
  // ⇒ the read-out hides: there is no saved agent for the resolver to key on.
  agentName?: string;
  // Admin cross-squad selector (the ?team= act-as-team seam); tenants omit it.
  team?: string;
}

export function EffectiveModelReadout({ agentName, team }: EffectiveModelReadoutProps) {
  const { data, loading, notFound, error } = useEffectiveModel(agentName, team);

  // Create mode (no persisted agent) or existence-hiding 404: nothing to resolve —
  // render nothing rather than an empty or misleading box.
  if (!agentName?.trim() || notFound) return null;

  if (loading) {
    return (
      <div className="effective-model effective-model--loading" aria-busy="true">
        <span className="effective-model__label">Effective model</span>
        <span className="effective-model__hint">Resolving…</span>
      </div>
    );
  }

  if (error || !data) {
    // A transient read failure is non-blocking: the form still submits (the server
    // is the fail-closed authority). Surface it terse, never as a crash.
    return (
      <div className="effective-model" role="status">
        <span className="effective-model__label">Effective model</span>
        <span className="effective-model__hint">Couldn’t resolve the effective model right now.</span>
      </div>
    );
  }

  // AC4 — fail-closed / unresolved guardrail. No invented fallback (ISI-4430 D3):
  // the model is genuinely unset at every tier until an admin sets the org default.
  if (data.unresolved) {
    return (
      <div className="effective-model effective-model--unresolved" role="status">
        <span className="effective-model__label">Effective model</span>
        <p className="effective-model__warn">
          No effective model — the org default is unset. The platform can’t resolve a model until an
          admin sets it (Settings → Configuration).
        </p>
      </div>
    );
  }

  // AC2 / AC3 — resolved. The chip tier IS the provenance the server returned; the
  // helper only appears when the model is inherited (tier ≠ agent), nudging toward
  // an Agent-tier override without implying the read-out is editable.
  const inherited = data.tier !== "agent";
  return (
    <div className="effective-model" role="status">
      <div className="effective-model__row">
        <span className="effective-model__label">Effective model</span>
        <span className="effective-model__model">{data.model}</span>
        <ProvenanceChip tier={data.tier} roleName={data.roleName} />
      </div>
      {inherited && (
        <span className="effective-model__hint">
          Read-only in v1. Set a model above to override at the Agent tier.
        </span>
      )}
    </div>
  );
}
