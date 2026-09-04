// app/teams/page.tsx — Teams nav destination (ISI-3725 rail realignment to the ISI-3641 mock).
//
// The ISI-3641 rail promotes Teams to a top-level item. A dedicated Teams listing surface is not yet
// built (Team data lives server-side as SquadOverview/teamOrg and is rendered today inside the Agents
// org diagram). This is the honest landing so the rail link resolves instead of 404-ing; the real
// listing is a follow-up story. ponytail: placeholder route, no data fetch — upgrade to a Team list
// when the Teams surface lands.

import Link from "next/link";

export const metadata = {
  title: "Teams — K8squad Console",
};

export default function TeamsPage() {
  return (
    <section className="stub">
      <h1>Teams</h1>
      <p>
        A dedicated Teams surface is coming. Today, a session resolves to one Team and its org chart
        renders under <Link href="/agents">Agents</Link>; the fleet view lives on{" "}
        <Link href="/overview">Overview</Link>.
      </p>
    </section>
  );
}
