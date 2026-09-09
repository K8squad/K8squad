// app/projects/[projectId]/layout.tsx — the Project-Detail Workspace shell (ISI-3957 S1).
//
// KEYSTONE of the Project-Detail Workspace epic (ISI-3942): every section (Landing · Issues ·
// Runs · Discussion · File Explorer · GitHub) renders INSIDE this shell, to the right of a
// persistent, expandable LEFT menu. This reshapes the old horizontal tab strip (Build · Tickets ·
// Runs · Discussion · GitHub) into the control-room layout the board's mental model expects
// ("open a project → get a workspace"). S2 fills the Landing frame; S3 owns Issues writes; S6
// wires the deep-links INTO this route — S1 only guarantees the destinations resolve.
//
// Server component: it binds projectId and delegates interactivity (collapse/expand, active-tab
// derivation, localStorage) to the small client child <ProjectWorkspaceMenu/>. The menu is a pure
// derivation of the pathname + project id (nav.ts doctrine — the URL is the state).

import type { ReactNode } from "react";
import { ProjectWorkspaceMenu } from "@/components/projects/ProjectWorkspaceMenu";
import { decodeProjectId } from "@/lib/projectId";

export default async function ProjectLayout({
  children,
  params,
}: {
  children: ReactNode;
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  // TODO(ISI-3941): fleet-wide name collision — `projectId` is the project CR NAME only; the same
  // name can exist in two namespaces for a fleet admin. The namespace/team qualifier in the route
  // is an open question owned by ISI-3941 / the Architect. S1 uses name-only and leaves this seam.
  const decoded = decodeProjectId(projectId);
  return (
    <div className="pworkspace">
      <ProjectWorkspaceMenu projectId={decoded} />
      <div className="pworkspace__content">{children}</div>
    </div>
  );
}
