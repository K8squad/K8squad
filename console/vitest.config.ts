import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

// Vitest config for the console's unit + component tests (story 10.3 test guidance:
// component / authz / no-coordination / theming). Playwright e2e stays separate (`npm run e2e`).
// The `@/*` alias mirrors tsconfig.json so tests import the same way the app does.
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL(".", import.meta.url)),
      // lib/bff.ts imports `server-only` (correct at Next runtime); unit tests import the BFF
      // route modules directly outside that context, where the real package throws. Map it to an
      // empty stub so the proxy boundary stays testable (see test/server-only-stub.ts).
      "server-only": fileURLToPath(
        new URL("./test/server-only-stub.ts", import.meta.url),
      ),
    },
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./vitest.setup.ts"],
    include: ["test/**/*.test.{ts,tsx}"],
    coverage: {
      // ConsoleFloor (>=70%) enforced here so the L5 `Test with coverage` step fails
      // closed instead of passing vacuously (ISI-7578 review F4). Measured at head:
      // statements 76.0 / branches 84.0 / functions 81.7 / lines 76.0.
      thresholds: {
        statements: 70,
        branches: 70,
        functions: 70,
        lines: 70,
      },
    },
  },
});
