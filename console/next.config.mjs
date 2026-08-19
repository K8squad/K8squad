/** @type {import('next').NextConfig} */
const nextConfig = {
  // Distroless runtime image (Dockerfile.console) copies .next/standalone + server.js.
  output: "standalone",
  reactStrictMode: true,
  poweredByHeader: false,
  // The console is a BFF: it proxies the Go apiserver (REST + SSE) server-side and is the
  // single authorization choke point (arch §13 / ADR-013). The browser never learns the
  // apiserver URL, so it is a server-only env var — never NEXT_PUBLIC_*.
  env: {},
};

export default nextConfig;
