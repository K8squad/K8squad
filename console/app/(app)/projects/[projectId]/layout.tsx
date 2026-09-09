// app/projects/[projectId]/layout.tsx — the Project-scoped route group.
//
// ISI-4090: the top-of-page Build·Tickets·Runs·Discussion·GitHub tab strip is GONE. Project
// navigation now lives in the left rail — the Projects node expands (ProjectsNavTree island) to
// that project's sections, each with an icon. This layout is a pure pass-through: the rail owns
// the sub-nav and the content header owns the breadcrumb, so nothing is rendered here but the
// screen itself. (The old strip was unstyled inline links — the "unclear links" the redesign
// removes; `projectSubnav` still drives the rail sections from lib/nav.ts.)

import type { ReactNode } from "react";

export default function ProjectLayout({ children }: { children: ReactNode }) {
  return <div className="project">{children}</div>;
}
