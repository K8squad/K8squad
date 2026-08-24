import { describe, it, expect } from "vitest";
import {
  bannerHold,
  classifyCredentialsStatus,
  expiryLabel,
  expiringSoon,
  formatDuration,
  healthBadge,
  tokenTypeLabel,
  type AgentCredentialRow,
} from "@/lib/credentials";

// The pure derivations behind the 8.6 screen: health badges, expiry honesty
// (unknown is "—", never a fabricated horizon), banner selection, and the
// outcome classification (deny collapse + documented 501).

const NOW = new Date("2026-08-20T13:00:00Z");

function row(over: Partial<AgentCredentialRow> = {}): AgentCredentialRow {
  return {
    agent: "fixer-hermes",
    namespace: "squad-a",
    runtime: "hermes",
    credentialRef: "squad-a/sam/hermes-oauth",
    expiresKnown: false,
    health: "connected",
    ...over,
  };
}

describe("tokenTypeLabel — credential-class vocabulary", () => {
  it("maps the pinned classes", () => {
    expect(tokenTypeLabel(row({ credentialClass: "claude_oauth" }))).toBe("OAuth");
    expect(tokenTypeLabel(row({ credentialClass: "api_key" }))).toBe("API key");
    expect(tokenTypeLabel(row({ credentialClass: "byo_endpoint" }))).toBe("BYO endpoint");
    expect(tokenTypeLabel(row({ credentialClass: "human-seat" }))).toBe("OAuth · seat");
  });
  it("unknown class renders the honest dash", () => {
    expect(tokenTypeLabel(row())).toBe("—");
  });
});

describe("expiryLabel — honesty rules", () => {
  it("unknown horizon renders — (never fabricated)", () => {
    expect(expiryLabel(row(), NOW)).toBe("—");
    expect(expiryLabel(row({ expiresKnown: true, expiresAt: undefined }), NOW)).toBe("—");
  });
  it("static classes render — (static)", () => {
    expect(expiryLabel(row({ credentialClass: "api_key" }), NOW)).toBe("— (static)");
    expect(expiryLabel(row({ credentialClass: "byo_endpoint" }), NOW)).toBe("— (static)");
  });
  it("future horizon renders the mock-05 idiom", () => {
    const at = new Date(NOW.getTime() + 41 * 60000);
    expect(expiryLabel(row({ expiresKnown: true, expiresAt: at.toISOString() }), NOW)).toBe("in 41m");
  });
  it("past horizon renders expired Nm ago", () => {
    const at = new Date(NOW.getTime() - 6 * 60000);
    expect(expiryLabel(row({ expiresKnown: true, expiresAt: at.toISOString() }), NOW)).toBe("expired 6m ago");
  });
});

describe("expiringSoon", () => {
  it("flags a horizon inside the window", () => {
    const at = new Date(NOW.getTime() + 30 * 60000);
    expect(expiringSoon(row({ expiresKnown: true, expiresAt: at.toISOString() }), NOW)).toBe(true);
  });
  it("unknown or far horizons are never soon", () => {
    const far = new Date(NOW.getTime() + 5 * 3600000);
    expect(expiringSoon(row(), NOW)).toBe(false);
    expect(expiringSoon(row({ expiresKnown: true, expiresAt: far.toISOString() }), NOW)).toBe(false);
  });
});

describe("healthBadge — the closed set + overlays", () => {
  it("paused hold wins: Expired · paused", () => {
    const badge = healthBadge(
      row({ health: "expired", pausedRuns: [{ name: "run-139", reason: "credential_expired" }] }),
      NOW,
    );
    expect(badge).toEqual({ label: "Expired · paused", tone: "bad" });
  });
  it("connected + soon ⇒ Expiring soon (warn)", () => {
    const at = new Date(NOW.getTime() + 30 * 60000);
    expect(healthBadge(row({ expiresKnown: true, expiresAt: at.toISOString() }), NOW)).toEqual({
      label: "Expiring soon",
      tone: "warn",
    });
  });
  it("connected + unknown horizon ⇒ Valid (idle-adjacent ok)", () => {
    expect(healthBadge(row(), NOW)).toEqual({ label: "Valid", tone: "ok" });
  });
  it("refreshing ⇒ warn; anything else maps to Unknown, never a guess", () => {
    expect(healthBadge(row({ health: "refreshing" }), NOW).label).toBe("Refreshing");
    expect(healthBadge(row({ health: "garbage" }), NOW)).toEqual({ label: "Unknown", tone: "idle" });
  });
});

describe("bannerHold — the clearest signal wins", () => {
  it("null with no holds", () => {
    expect(bannerHold([row(), row({ agent: "b" })])).toBeNull();
  });
  it("picks the most recent hold across rows", () => {
    const older = new Date("2026-08-20T12:00:41Z").toISOString();
    const newer = new Date("2026-08-20T12:30:00Z").toISOString();
    const hold = bannerHold([
      row({ agent: "a", pausedRuns: [{ name: "run-139", reason: "credential_expired", since: older }] }),
      row({ agent: "b", pausedRuns: [{ name: "run-142", reason: "credential_expired", since: newer }] }),
    ]);
    expect(hold).toEqual({ agent: "b", run: { name: "run-142", reason: "credential_expired", since: newer } });
  });
  it("sub-second (RFC3339Nano) timestamps compare as instants, not strings", () => {
    // Go emits "...:00.5Z" for sub-second precision; '.' (0x2E) sorts before 'Z' (0x5A), so a
    // string compare would wrongly crown the older whole-second hold (PR #87 review).
    const wholeSecond = new Date("2026-08-20T12:00:00Z").toISOString();
    const halfSecondLater = new Date("2026-08-20T12:00:00.500Z").toISOString();
    const hold = bannerHold([
      row({ agent: "a", pausedRuns: [{ name: "run-whole", reason: "credential_expired", since: wholeSecond }] }),
      row({ agent: "b", pausedRuns: [{ name: "run-half", reason: "credential_expired", since: halfSecondLater }] }),
    ]);
    expect(hold?.run.name).toBe("run-half");
  });
  it("missing since keeps first-wins (NaN comparisons are false)", () => {
    const hold = bannerHold([
      row({ agent: "a", pausedRuns: [{ name: "run-a", reason: "credential_expired" }] }),
      row({ agent: "b", pausedRuns: [{ name: "run-b", reason: "credential_expired" }] }),
    ]);
    expect(hold?.run.name).toBe("run-a");
  });
});

describe("formatDuration — mock-05 idiom", () => {
  it("compacts minutes/hours/days", () => {
    expect(formatDuration(41 * 60000)).toBe("41m");
    expect(formatDuration((5 * 60 + 12) * 60000)).toBe("5h 12m");
    expect(formatDuration(2 * 3600000)).toBe("2h");
    expect(formatDuration(9 * 86400000)).toBe("9d");
  });
});

describe("classifyCredentialsStatus — deny collapse + documented 501", () => {
  it("401/403/404 collapse to not-found (existence-hiding)", () => {
    for (const s of [401, 403, 404]) expect(classifyCredentialsStatus(s)).toBe("not-found");
  });
  it("501 is the unconfigured contract, distinct from error", () => {
    expect(classifyCredentialsStatus(501)).toBe("unconfigured");
  });
  it("5xx/other ⇒ error", () => {
    expect(classifyCredentialsStatus(500)).toBe("error");
    expect(classifyCredentialsStatus(503)).toBe("error");
  });
});
