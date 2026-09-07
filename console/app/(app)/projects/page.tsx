// app/projects/page.tsx — Projects nav destination (ISI-3725 rail realignment; list surface ISI-3943).
//
// The ISI-3641 rail flattens the old project-scoped `Project` accordion into a top-level `Projects`
// link. This landing used to be a placeholder that punted to Overview because the apiserver had no
// GET list route for the tab to call (ISI-3941 root cause). ISI-3943 adds the fleet-aware
// GET /api/squad/projects read route; this page now renders that list (ProjectsList, a client
// component that fetches the BFF proxy). The deep project routes (/projects/[projectId]/…) and
// their sub-nav tab bar (ISI-3651 E5) are unchanged.

import { ProjectsList } from "@/components/ProjectsList";

export const metadata = {
  title: "Projects — K8squad Console",
};

export default function ProjectsPage() {
  return <ProjectsList />;
}
