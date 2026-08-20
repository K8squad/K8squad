// Empty stub for the `server-only` package (lib/bff.ts imports it to enforce the BFF boundary in
// the Next runtime). Unit tests import route handlers directly — outside React's server/client
// compilation context — where the real package would throw. The alias in vitest.config.ts maps
// the import here so BFF route modules are testable at the proxy boundary.
export {};
