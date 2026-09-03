// app/runs/page.tsx — Runs nav destination (ISI-3725 rail realignment to the ISI-3641 mock).
//
// The ISI-3641 rail promotes Runs from a project sub-section to a top-level item. Run DETAIL routes
// (/runs/[runId]/…) already exist; only this cross-project index landing is new. A global run-history
// list is a follow-up — today runs are listed per project under /projects/[projectId]/runs, so this
// honest landing points there instead of 404-ing. ponytail: placeholder index, no data fetch —
// upgrade to a cross-project run list when that surface lands.

import Link from "next/link";

export const metadata = {
  title: "Runs — K8squad Console",
};

export default function RunsPage() {
  return (
    <section className="stub">
      <h1>Runs</h1>
      <p>
        A cross-project run history is coming. Runs are listed per project today — open a project from{" "}
        <Link href="/overview">Overview</Link> to see its runs, or use a run link to jump straight to
        a run&apos;s build and artifacts.
      </p>
    </section>
  );
}
