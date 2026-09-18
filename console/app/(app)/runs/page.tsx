// app/runs/page.tsx — the cross-project (fleet) runs list (ISI-4575), replacing the ISI-3725
// placeholder index now that the ISI-4571 global run-list read model exists. Renders the shared
// RunsList body with no project scope; rows deep-link into the run detail at /runs/{runId}.

import { RunsList } from "@/components/runs/RunsList";

export const metadata = {
  title: "Runs — K8squad Console",
};

export default function RunsPage() {
  return <RunsList />;
}
