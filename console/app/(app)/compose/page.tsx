// app/compose/page.tsx — the Compose nav node (story 8.5 / UX screen 04-compose-crd, ISI-2901).
//
// The thin App Router entry for the CRD authoring surface. The screen itself (kind selector +
// typed create/edit forms for Team/Project/Agent/Role/Skill) is a client component that writes
// through the BFF (/api/compose → apiserver CRD-apply write surface, ISI-3198). RBAC is enforced
// upstream by the apiserver's write-tier membership gate (§13 choke point): a viewer's apply is a
// 403 and Team creation is admin-only — the form surfaces the server's verdict verbatim rather
// than pre-authorizing in the browser.

import { Suspense } from "react";

import { ComposeScreen } from "@/components/compose/ComposeScreen";

export const dynamic = "force-dynamic";

export default function ComposePage() {
  // ComposeScreen reads deep-link params via useSearchParams (ISI-3554) — Next requires a Suspense
  // boundary around a client component that reads the query string.
  return (
    <Suspense fallback={<div className="compose" aria-busy="true" />}>
      <ComposeScreen />
    </Suspense>
  );
}
