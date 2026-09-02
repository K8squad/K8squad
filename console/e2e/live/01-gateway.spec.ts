// e2e/live/01-gateway.spec.ts — MATRIX ROW: gateway reachability.
//
// The first thing that has to work before anything else: the gateway in front of the Console must
// answer, and an unauthenticated hit on the app root must bounce to /login (the ISI-3520/3530
// shell-leak + exposure fix). No credentials required — this row always runs.

import { test, expect, BASE_URL, LOGIN_PATH } from "./config";

test.describe("Gateway reachability", () => {
  test(
    "gateway answers and redirects the unauthenticated root to /login",
    { annotation: { type: "feature", description: "Gateway: root → /login" } },
    async ({ request }) => {
      // Raw hit on "/" WITHOUT following redirects — we want to SEE the bounce, not the target.
      const res = await request.get(`${BASE_URL}/`, { maxRedirects: 0 });
      const status = res.status();
      expect(
        status,
        `GET / should redirect (30x) unauthenticated, got ${status}`,
      ).toBeGreaterThanOrEqual(300);
      expect(status).toBeLessThan(400);
      const location = res.headers()["location"] ?? "";
      expect(location, `redirect Location should point at /login, got "${location}"`).toContain(
        "/login",
      );
    },
  );

  test(
    "login route is reachable (HTTP 200)",
    { annotation: { type: "feature", description: "Gateway: /login reachable" } },
    async ({ request }) => {
      const res = await request.get(`${BASE_URL}${LOGIN_PATH}`);
      expect(res.status(), `GET ${LOGIN_PATH} should be 200`).toBe(200);
      const body = await res.text();
      expect(body, "login HTML should render the sign-in shell").toContain("html");
    },
  );
});
