// app/audit/page.tsx — the Audit trail route (story 2.6 / ISI-2881).
//
// Mounted inside the Epic 8 shell (app/layout.tsx — theming, topbar). Nav-rail parenting lands
// with the 8.13 nav-IA shell (ISI-2908); this route is the surface itself. Thin by design: the
// client screen owns fetch/state so every render path (ok / empty / denied / forbidden-actor /
// unconfigured 501 / error) is testable at the component boundary.

import { AuditTrailScreen } from "@/components/audit/AuditTrailScreen";

export const metadata = {
  title: "Audit trail — K8squad Console",
};

export default function AuditPage() {
  return <AuditTrailScreen />;
}
