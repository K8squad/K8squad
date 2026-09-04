import { describe, it, expect } from "vitest";
import { sanitizeNext } from "@/app/login/safeNext";

// Copilot review of PR #215: the post-login `?next=` redirect guard let a backslash bypass slip
// through — `?next=/\evil.example` passed the "starts with / but not //" check, yet the browser
// resolves `/\evil.example` as `https://evil.example/` (an open redirect). sanitizeNext must keep
// the redirect on-origin. Origin is injected so this is a pure, deterministic unit test.
const ORIGIN = "https://console.local";

describe("sanitizeNext — post-login open-redirect guard", () => {
  it("keeps a plain root-relative path", () => {
    expect(sanitizeNext("/overview", ORIGIN)).toBe("/overview");
    expect(sanitizeNext("/projects/p1?tab=runs#top", ORIGIN)).toBe("/projects/p1?tab=runs#top");
  });

  it("defaults to / when next is absent", () => {
    expect(sanitizeNext(null, ORIGIN)).toBe("/");
    expect(sanitizeNext("", ORIGIN)).toBe("/");
  });

  it("rejects the backslash bypass (Copilot regression)", () => {
    expect(sanitizeNext("/\\evil.example", ORIGIN)).toBe("/");
    expect(sanitizeNext("/%5Cevil.example", ORIGIN)).toBe("/%5Cevil.example"); // literal %5C stays on-path (browser does not decode it here)
    expect(sanitizeNext("/\\/evil.example", ORIGIN)).toBe("/");
  });

  it("rejects protocol-relative and absolute URLs", () => {
    expect(sanitizeNext("//evil.example", ORIGIN)).toBe("/");
    expect(sanitizeNext("https://evil.example", ORIGIN)).toBe("/");
    expect(sanitizeNext("http://evil.example/path", ORIGIN)).toBe("/");
    expect(sanitizeNext("javascript:alert(1)", ORIGIN)).toBe("/");
  });
});
