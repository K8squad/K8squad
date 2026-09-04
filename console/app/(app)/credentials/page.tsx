// app/credentials/page.tsx — the Settings → Credentials route (story 8.6 / mock 05).
//
// Mounted inside the Epic 8 shell (app/layout.tsx — theming, topbar). Nav-rail parenting under
// Settings lands with the 8.13 nav-IA shell (ISI-2908); this route is the surface itself.
// Thin by design: the client screen owns fetch/state so every render path (ok / unconfigured 501 /
// deny-collapsed not-found / error) is testable at the component boundary.

import { CredentialsScreen } from "@/components/credentials/CredentialsScreen";

export const metadata = {
  title: "Credentials & auth state — K8squad Console",
};

export default function CredentialsPage() {
  return <CredentialsScreen />;
}
