import { describe, it, expect } from "vitest";
import { resolveTeamState } from "@/components/agents/SessionTeamOrg";

// The Agents landing auto-resolves the session's ONE Team (tenancy root) from the squad-overview
// projection — there is no manual selector (ISI-3543). resolveTeamState is the honest status map
// that decides what the landing renders; it must NEVER surface scaffold copy or leak a foreign team.

describe("resolveTeamState — session-team resolution", () => {
  it("200 with a team.uid → ok(teamId)", () => {
    expect(
      resolveTeamState(200, { team: { uid: "uid-abc" } }),
    ).toEqual({ kind: "ok", teamId: "uid-abc" });
  });

  it("401 → unauthenticated", () => {
    expect(resolveTeamState(401, null)).toEqual({ kind: "unauthenticated" });
  });

  it("404 → no-team (existence-hiding, not an error)", () => {
    expect(resolveTeamState(404, null)).toEqual({ kind: "no-team" });
  });

  it("200 but no team.uid on the wire → no-team, not a crash", () => {
    expect(resolveTeamState(200, {})).toEqual({ kind: "no-team" });
    expect(resolveTeamState(200, { team: {} })).toEqual({ kind: "no-team" });
    expect(resolveTeamState(200, null)).toEqual({ kind: "no-team" });
  });

  it("501 (read model not wired) and 5xx → error (retryable)", () => {
    expect(resolveTeamState(501, null)).toEqual({ kind: "error" });
    expect(resolveTeamState(500, null)).toEqual({ kind: "error" });
    expect(resolveTeamState(0, null)).toEqual({ kind: "error" });
  });
});
