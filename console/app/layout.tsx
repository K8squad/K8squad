// app/layout.tsx — the BARE App Router root layout (ISI-3522).
//
// The root layout is deliberately MINIMAL: html/body + ThemeProvider only. It mounts NO nav shell,
// so pages that must render pre-auth — the `/login` route (auth route group) and the root
// `not-found.tsx` — do NOT leak the operator `<ConsoleShell>` rail (the ISI-3520 bug: a 404 rendered
// inside the shell because the shell used to live here).
//
// The authenticated app now lives under the `(app)` route group, whose own layout
// (app/(app)/layout.tsx) mounts ConsoleShell + resolves the viewer's role. Route groups do not
// affect URLs, so `/overview`, `/agents`, … are unchanged. Pre-auth pages live under `(auth)`.
// ThemeProvider stays at the root so dark/light theming applies to every surface, login included.

import type { Metadata } from "next";
import type { ReactNode } from "react";
import { GeistSans } from "geist/font/sans";
import { GeistMono } from "geist/font/mono";
import "./globals.css";
import { ThemeProvider } from "@/components/ThemeProvider";
import { DEFAULT_THEME } from "@/lib/theme";

export const metadata: Metadata = {
  title: "K8squad Console",
  description:
    "Operator console — legibility + composition surface for K8squad squads.",
};

// Self-hosted Geist Sans / Geist Mono (visual-system §Typography, ISI-3545/3549). Each `.variable`
// className exposes a CSS custom property (--font-geist-sans / --font-geist-mono) that globals.css
// feeds into --font-sans / --font-mono — no build-time Google fetch, so CI builds stay offline-safe.
export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html
      lang="en"
      data-theme={DEFAULT_THEME}
      className={`${GeistSans.variable} ${GeistMono.variable}`}
      suppressHydrationWarning
    >
      <body>
        <ThemeProvider>{children}</ThemeProvider>
      </body>
    </html>
  );
}
