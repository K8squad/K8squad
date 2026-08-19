// app/layout.tsx — the console shell (App Router root layout).
//
// The whole-shell theming contract (story 8.9) mounts here: <html data-theme="dark"> is the
// default canvas, and the ThemeProvider swaps the token role attribute on toggle. Every screen
// mounts inside this shell (nav rail + 8-Crest lockup + theme toggle), so theming is shell-wide,
// never a per-screen paint job.

import type { Metadata } from "next";
import type { ReactNode } from "react";
import "./globals.css";
import { ThemeProvider } from "@/components/ThemeProvider";
import { ThemeToggle } from "@/components/ThemeToggle";
import { Logo } from "@/components/Logo";
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
          <div className="shell">
            <header className="shell__topbar">
              <Logo />
              <ThemeToggle />
            </header>
            <main className="shell__main">{children}</main>
          </div>
        </ThemeProvider>
      </body>
    </html>
  );
}
