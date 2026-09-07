// app/teams/page.tsx — the Teams nav destination (ISI-3725 rail item), now a
// real listing surface (ISI-3953, gap G4 of the ISI-3949 fleet-admin audit).
//
// Replaces the former non-fetching stub. Thin by design: the client screen owns
// fetch/state so every render path (ok / empty / unconfigured 501 / deny-collapsed
// not-found / error) is testable at the component boundary. Admin ⇒ fleet-wide
// Teams; tenant ⇒ own Team only (the apiserver enforces the scoping). This is the
// enumeration source the fleet Team picker (ISI-3950, gap G1) consumes.

import { TeamsScreen } from "@/components/teams/TeamsScreen";

export const metadata = {
  title: "Teams — K8squad Console",
};

export default function TeamsPage() {
  return <TeamsScreen />;
}
