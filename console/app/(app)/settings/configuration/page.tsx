// app/settings/configuration/page.tsx — Settings → Configuration (story 8.12 / ADR-029).
//
// Mounts the OTLP exporter compose surface (per-signal traces/metrics/logs routing over the
// OTelConfig CRD, saved through the BFF at /api/otelconfig). The screen itself is a client
// component; this route is the nav destination for the Settings → Configuration node.

import { OtlpConfigScreen } from "@/components/settings/OtlpConfigScreen";

export default function SettingsConfigurationPage() {
  return <OtlpConfigScreen />;
}
