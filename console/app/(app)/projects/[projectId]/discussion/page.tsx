// Project-scoped Discussion route (AC1). Reachable from the Project nav
// (Issues · Discussion · Project Board · File Explorer, per console_kit_ia).
//
// This is the Next.js App Router entry, mounted inside the Epic 8 shell
// (app/layout.tsx — nav topbar, theming, BFF). It is intentionally thin: it
// resolves the Project's default room via the BFF (which enforces the shared
// authz choke point — deny renders 404-not-403, AC4) and mounts the client-side
// room view.

import { DiscussionRoomClient } from "./RoomClient";

// Next.js 15: dynamic params arrive as a Promise; the route is per-request
// (never statically cached) because the room is a live, authz-scoped surface.
export const dynamic = "force-dynamic";

export default async function DiscussionPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  return (
    <main className="ksq-discussion-page">
      <header className="ksq-discussion-page__head">
        <h1>Discussion</h1>
        <p className="muted">
          Project collaboration room — agents and humans, threaded. Provenance
          is server-stamped; this surface carries no coordination control.
        </p>
      </header>
      <DiscussionRoomClient projectId={projectId} />
    </main>
  );
}
