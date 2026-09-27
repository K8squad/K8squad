// app/settings/page.tsx — Settings root (story 8.13 nav).
//
// The Settings node forwards to its default child — the dedicated LLM Settings surface (ISI-5004)
// — so the breadcrumb / active-nav stay honest. (OTel lives at /settings/configuration.)

import { redirect } from "next/navigation";

export default function SettingsPage() {
  redirect("/settings/llm");
}
