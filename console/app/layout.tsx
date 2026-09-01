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
import "./globals.css";
import { ThemeProvider } from "@/components/ThemeProvider";
import { DEFAULT_THEME } from "@/lib/theme";

export const metadata: Metadata = {
  title: "KSquad Console",
  description:
    "Operator console — legibility + composition surface for KSquad squads.",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" data-theme={DEFAULT_THEME} suppressHydrationWarning>
      <body>
        <ThemeProvider>{children}</ThemeProvider>
      </body>
    </html>
  );
}
