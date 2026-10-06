// app/(app)/inbox/page.tsx — ISI-5535 (E1 of ISI-5531): the "Needs Human Decision" Inbox page.
// Follows the runs/ route pattern: thin page that mounts the shared InboxList component body.

import { InboxList } from "@/components/inbox/InboxList";

export const metadata = {
  title: "Inbox — K8squad Console",
};

export default function InboxPage() {
  return (
    <div className="inbox">
      <h1 className="inbox__heading">Inbox</h1>
      <p className="inbox__subhead">Decisions waiting for you across all projects.</p>
      <InboxList />
    </div>
  );
}
