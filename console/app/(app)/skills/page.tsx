// app/skills/page.tsx — Skills surface (ISI-3962 S2, AC4).
//
// A read-only fleet-aware Skills list + single-skill view, mirroring the Projects surface
// (projects/page.tsx → ProjectsList). S1 (ISI-3961) added the backend read routes
// GET /api/squad/skills (list) + GET /api/squad/skills/{name} (detail); this page renders the
// SkillsList client component, which fetches the BFF proxies. It is reachable from the Compose Skills
// tab ("Browse all skills →") — a deliberate choice NOT to add a top-level rail item, keeping the
// approved ISI-3641 nav mock intact (Skills is an "advanced" compose kind, not a rail destination).

import { SkillsList } from "@/components/SkillsList";

export const metadata = {
  title: "Skills — K8squad Console",
};

export default function SkillsPage() {
  return <SkillsList />;
}
