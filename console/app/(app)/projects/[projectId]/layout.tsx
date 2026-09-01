// app/projects/[projectId]/layout.tsx — the Project-scoped sub-nav (story 8.13).
//
// Every screen under /projects/{id} renders with the project sub-nav tab strip
// (Build · Tickets · Runs · Discussion), all scoped to the selected Project. This is routing
// ONLY (scope guard R6): the existing discussion screen (10.3) re-parents here unchanged.

import type { ReactNode } from "react";
import Link from "next/link";
import { projectSubnav } from "@/lib/nav";

export default async function ProjectLayout({
  children,
  params,
}: {
  children: ReactNode;
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  const decoded = decodeURIComponent(projectId);
  const subnav = projectSubnav(decoded);
  return (
    <div className="project">
      <nav className="subnav" aria-label="Project sections">
        {subnav.map((n) => (
          <Link key={n.id} href={n.href} className="subnav__tab">
            {n.label}
          </Link>
        ))}
      </nav>
      {children}
    </div>
  );
}
