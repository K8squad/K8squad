// Project-scoped Discussion route (AC1). Reachable from the Project nav
// (Issues · Discussion · Project Board · File Explorer, per console_kit_ia).
//
// This is the Next.js App Router entry. It is intentionally thin: it resolves
// the Project's default room via the BFF (which enforces the shared authz choke
// point — deny renders 404-not-403, AC4) and mounts the client-side room view.
//
// NOTE: the surrounding Epic 8 shell (nav rail, auth session, BFF middleware)
// is provided by Epic 8; this file wires only the Discussion slice into it. It
// avoids importing from `next/*` so the slice type-checks and tests in
// isolation until the shell lands.

import { DiscussionRoomClient } from "./RoomClient";

export const dynamic = "force-dynamic";

export default function DiscussionPage({
  params,
}: {
  params: { projectId: string };
}) {
  return (
    <main className="ksq-discussion-page">
      <header className="ksq-discussion-page__head">
        <h1>Discussion</h1>
      </header>
      <DiscussionRoomClient projectId={params.projectId} />
    </main>
  );
}
