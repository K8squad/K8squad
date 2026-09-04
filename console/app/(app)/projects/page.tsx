// app/projects/page.tsx — Projects nav destination (ISI-3725 rail realignment to the ISI-3641 mock).
//
// The ISI-3641 rail flattens the old project-scoped `Project` accordion into a top-level `Projects`
// link. The deep project routes (/projects/[projectId]/…) and their sub-nav tab bar (ISI-3651 E5)
// are unchanged; only this index landing is new. A project LIST surface is a follow-up — for now the
// squad's projects are enumerated on Overview, so this honest landing links there instead of 404-ing.
// ponytail: placeholder index, no data fetch — upgrade to a project list when that surface lands.

import Link from "next/link";

export const metadata = {
  title: "Projects — K8squad Console",
};

export default function ProjectsPage() {
  return (
    <section className="stub">
      <h1>Projects</h1>
      <p>
        A projects list surface is coming. Your squad&apos;s projects are shown on{" "}
        <Link href="/overview">Overview</Link>; open one to reach its build, tickets, runs and
        discussion.
      </p>
    </section>
  );
}
