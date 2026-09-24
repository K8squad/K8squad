// components/compose/ProvenanceChip.tsx — the model-priority provenance atom (ISI-4893 / S4,
// epic ISI-4822). The one net-new visual of the model-config UX: a tinted pill + dot painted in
// the *winning* tier's locked hue (DESIGN-SPEC-ISI-4823 §2/§5.2), answering "which tier did this
// model come from?" at a glance. Pure presentational — the consumer (S3 EffectiveModelReadout,
// and the S1/S2 inheritance chips) owns resolution; this atom only paints the tier it is told.
//
// Locked colour language, zero new hex — each tier keys a globals.css token:
//   default → --accent #3d7dff · role → --ksq-run #a78bfa · agent → --status-running #34d399

export type ProvenanceTier = "default" | "role" | "agent";

// Exported so S3's read-out can label its "runs with … <chip>" sentence without duplicating
// the string logic. Role name is fail-soft: a missing name degrades to plain "from Role".
export function provenanceLabel(tier: ProvenanceTier, roleName?: string): string {
  switch (tier) {
    case "default":
      return "from Default";
    case "role":
      return roleName ? `from Role: ${roleName}` : "from Role";
    case "agent":
      return "Agent override";
  }
}

export function ProvenanceChip({
  tier,
  roleName,
}: {
  tier: ProvenanceTier;
  roleName?: string;
}) {
  const label = provenanceLabel(tier, roleName);
  return (
    <span
      className={`provenance-chip provenance-chip--${tier}`}
      data-tier={tier}
      title={label}
    >
      <span className="provenance-chip__dot" aria-hidden="true" />
      {label}
    </span>
  );
}
