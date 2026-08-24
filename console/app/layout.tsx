// app/layout.tsx — the console shell (App Router root layout, stories 8.13 + 8.20).
//
// The root layout stays a server component and mounts the ONE adaptive nav shell (ConsoleShell,
// a client component that reads the pathname) so the project-rooted, responsive nav wraps every
// page: desktop/tablet rail, mobile bottom nav + drawer, Project selector, and breadcrumb.

import type { Metadata } from "next";
import type { ReactNode } from "react";
import "./globals.css";
import { ThemeProvider } from "@/components/ThemeProvider";
import { ConsoleShell } from "@/components/nav/ConsoleShell";
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
        <ThemeProvider>
          <ConsoleShell>{children}</ConsoleShell>
        </ThemeProvider>
      </body>
    </html>
  );
}
