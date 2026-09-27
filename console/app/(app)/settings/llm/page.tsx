// app/settings/llm/page.tsx — Settings → LLM Settings (ISI-5004).
//
// The dedicated LLM model surface, split off /settings/configuration so the OTLP screen stays
// OTLP-only. This route is a server component so it can read the session; the section itself is
// a client component (ModelPrioritySection, unchanged from its previous home on the configuration
// page). Admin gating is resolved server-side here (viewer() → globalRole) and passed down; the
// apiserver's adminOnly compose scope stays authoritative.

import { viewer } from "@/lib/session";
import { ModelPrioritySection } from "@/components/settings/ModelPrioritySection";

export default async function SettingsLlmPage() {
  const v = await viewer();
  return <ModelPrioritySection isAdmin={v.access === "admin"} />;
}
