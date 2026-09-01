// app/settings/page.tsx — Settings root (story 8.13 nav).
//
// Settings has one surface today: Configuration (the OTLP exporter config, story 8.12). The
// Settings node lands here and forwards to its default child so the breadcrumb / active-nav stay
// honest as more settings surfaces arrive.

import { redirect } from "next/navigation";

export default function SettingsPage() {
  redirect("/settings/configuration");
}
