// app/settings/configuration/page.tsx — Settings → Configuration (story 8.12 / ADR-029).
//
// OTLP exporter config — per-signal traces/metrics/logs routing (OTelConfig CRD). This route is
// OTLP-ONLY (ISI-5004): the Model Priority section moved to its dedicated /settings/llm surface.
//
// This route is a server component so it can read the session; the screen itself is a client
// component.

import { OtlpConfigScreen } from "@/components/settings/OtlpConfigScreen";

export default function SettingsConfigurationPage() {
  return <OtlpConfigScreen />;
}
