// lib/teams.ts — client types + status classifier for the Teams LIST surface
// (ISI-3953, gap G4 of the ISI-3949 fleet-admin audit).
//
// Mirrors the Go apiserver DTOs (internal/apiserver/teams.go): each row is a
// TeamRef {name, namespace, uid}, and the payload carries a Fleet flag set only
// for a global-admin caller (the list then spans every squad; a tenant sees
// exactly their own Team). The uid is the object UID the console feeds straight
// back into GET /api/teams/{uid}/org.

/** One Team row — mirrors internal/apiserver.TeamRef. */
export interface TeamSummary {
  name: string;
  namespace: string;
  uid: string;
}

/** GET /api/teams payload — mirrors internal/apiserver.TeamsList. */
export interface TeamsList {
  teams: TeamSummary[];
  fleet?: boolean;
}

/** The screen's load outcome kinds, mirroring the Credentials-screen state machine. */
export type TeamsOutcomeKind = "ok" | "empty" | "unconfigured" | "not-found" | "error";

/**
 * classifyTeamsStatus maps an HTTP status from the BFF (which surfaces the
 * apiserver's status verbatim) to a non-ok outcome kind. 501 ⇒ unconfigured
 * (read model not wired), 401/403/404 ⇒ not-found (deny/absence collapsed,
 * existence-hiding), anything else ⇒ error. A 2xx is handled by the caller
 * (which then distinguishes ok from empty by the row count), so it is a
 * programming error to classify one here.
 */
export function classifyTeamsStatus(status: number): Exclude<TeamsOutcomeKind, "ok" | "empty"> {
  if (status >= 200 && status < 300) throw new Error("ok is not a failure kind");
  if (status === 401 || status === 403 || status === 404) return "not-found";
  if (status === 501) return "unconfigured";
  return "error";
}
