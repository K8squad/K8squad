// app/settings/configuration/page.tsx — Settings → Configuration (story 8.12 / ADR-029).
//
// Mounts two independent client sections that share no state:
//   1. Model Priority (ISI-4890 S1) — the org-wide DEFAULT model tier (primary +
//      fallback + optional BYO endpoint), the floor of the Model-Per-Role ladder
//      (ISI-4430). Admin-only editor; the admin flag is resolved SERVER-SIDE here
//      (viewer() → globalRole) and passed down (UX gate; the apiserver's adminOnly
//      compose scope is authoritative). Placed FIRST per the 02-distributed mock.
//   2. OTLP exporter config — per-signal traces/metrics/logs routing (OTelConfig CRD).
//
// This route is a server component so it can read the session; the sections
// themselves are client components.

import { viewer } from "@/lib/session";
import { OtlpConfigScreen } from "@/components/settings/OtlpConfigScreen";
import { ModelPrioritySection } from "@/components/settings/ModelPrioritySection";

export default async function SettingsConfigurationPage() {
  const v = await viewer();
  return (
    <>
      <ModelPrioritySection isAdmin={v.access === "admin"} />
      <OtlpConfigScreen />
    </>
  );
}
