// app/not-found.tsx — the root 404 (ISI-3522, fixes the ISI-3520 "404 inside the shell" bug).
//
// A `not-found.tsx` at the app root renders on the BARE root layout (app/layout.tsx), which no longer
// mounts ConsoleShell. So an unknown route — or a `notFound()` that bubbles past the (app) group —
// renders this clean page instead of the operator nav rail wrapping a "404: This page could not be
// found." title (the exact ISI-3520 leak). It links back to the app root; the middleware will bounce
// an anonymous visitor from there to `/login`.

import Link from "next/link";
import { Logo } from "@/components/Logo";

export default function NotFound() {
  return (
    <main
      style={{
        minHeight: "100vh",
        display: "grid",
        placeItems: "center",
        background: "var(--canvas)",
        padding: "24px",
      }}
    >
      <div style={{ textAlign: "center", display: "flex", flexDirection: "column", alignItems: "center", gap: 12 }}>
        <Logo size={40} />
        <h1 style={{ margin: 0, fontSize: 20, color: "var(--text-1)" }}>Page not found</h1>
        <p style={{ margin: 0, color: "var(--text-2)", fontSize: 14 }}>
          The page you’re looking for doesn’t exist or has moved.
        </p>
        <Link
          href="/"
          style={{
            marginTop: 8,
            padding: "10px 16px",
            borderRadius: "var(--radius)",
            border: "1px solid var(--accent)",
            color: "var(--accent)",
            textDecoration: "none",
            fontWeight: 600,
          }}
        >
          Back to console
        </Link>
      </div>
    </main>
  );
}
