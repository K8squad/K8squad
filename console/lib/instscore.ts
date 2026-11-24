// lib/instscore.ts — the Instrumentation Score surface's client vocabulary
// (ISI-7750, phase 4 of the ISI-7741 plan).
//
// Types mirror the apiserver's GET /api/v1/instscore/services{,/{name}} JSON
// verbatim (snake_case over the wire). The screen consumes them through the
// BFF proxy routes (app/api/instscore/**); these helpers stay pure so the
// vitest boundary can exercise them without a server.

/** One services-list row. `score` is absent while a service sits mid-throttle
 *  (evidence arrived, the 10-minute re-evaluation window has not fired). */
export interface InstScoreServiceSummary {
  service: string;
  score?: number;
  category?: string;
  implemented: number;
  total: number;
  evaluated_at?: string;
  pass: number;
  fail: number;
  not_evaluated: number;
  spans: number;
  logs: number;
  metrics: number;
  resources: number;
  first_batch_at?: string;
  last_batch_at?: string;
}

/** GET /api/v1/instscore/services payload. */
export interface InstScoreServicesResponse {
  services: InstScoreServiceSummary[];
  rules_total: number;
}

/** One rule verdict, enriched with the vendored-standard context. */
export interface InstScoreVerdict {
  rule: string;
  target?: string[];
  impact: string;
  status: "pass" | "fail" | "not_evaluated";
  evidence: string;
  description?: string;
  guidance?: string;
  evaluated_at: string;
}

/** One hourly score sample, oldest first. */
export interface InstScoreHistoryPoint {
  bucket_hour: string;
  score: number;
}

/** GET /api/v1/instscore/services/{name} payload. */
export interface InstScoreServiceDetail extends InstScoreServiceSummary {
  verdicts: InstScoreVerdict[];
  history: InstScoreHistoryPoint[];
}

/** The screen's load states — the audit vocabulary: honest states render as
 *  themselves, never a fabricated fallback. */
export type InstScoreLoadState =
  | "loading"
  | "ok"
  | "empty"
  | "denied"
  | "unconfigured"
  | "error";

/** Map an HTTP status to the screen's load state (401/403+404 deny-collapse,
 *  501 unconfigured, anything else error — the house classify pattern). */
export function classifyInstScoreStatus(status: number): InstScoreLoadState {
  if (status >= 200 && status < 300) return "ok";
  if (status === 401 || status === 403 || status === 404) return "denied";
  if (status === 501) return "unconfigured";
  return "error";
}

/** Spec categories in display order with their tone class (shared palette
 *  vocabulary with the console's status badges). */
export const INSTSCORE_CATEGORIES = [
  "Excellent",
  "Good",
  "Needs Improvement",
  "Poor",
] as const;

/** Tone class for a category / score badge. */
export function categoryTone(category?: string, score?: number): string {
  if (category === undefined && score === undefined) return "pending";
  if (category === "Excellent") return "excellent";
  if (category === "Good") return "good";
  if (category === "Needs Improvement") return "needs-improvement";
  if (category === "Poor") return "poor";
  return "pending";
}

/** Display label for a service name, including the empty-string bucket
 *  (telemetry that arrived with a missing service.name — kept visible because
 *  RES-005 scores it as a violation). */
export function serviceLabel(service: string): string {
  return service === "" ? "(missing service.name)" : service;
}

/** The empty-name bucket has no detail route: {name} needs a path segment. */
export function serviceNavigable(service: string): boolean {
  return service !== "";
}

/** Score badge text: the number, or the honest pending marker. */
export function scoreLabel(summary: InstScoreServiceSummary): string {
  return summary.score === undefined ? "pending" : summary.score.toFixed(1);
}

/** Impact weight order for sorting/legend (Critical heaviest). */
export const IMPACT_ORDER: Record<string, number> = {
  Critical: 0,
  Important: 1,
  Normal: 2,
  Low: 3,
};

/** Compact "when" label for timestamps (UTC, date + time). */
export function whenLabel(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}
