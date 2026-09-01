// app/(app)/layout.tsx — the AUTHENTICATED app layout: the ONE adaptive nav shell (ISI-3522).
//
// This layout wraps every authenticated route (the `(app)` route group — `/`, `/overview`,
// `/agents`, … — route groups do NOT change the URL). It is the shell that USED to live in the root
// layout; moving it here is the ISI-3520 fix: pre-auth pages (`(auth)/login`, root `not-found.tsx`)
// render on the bare root layout WITHOUT this rail, so an unauthenticated user never sees the
// operator nav (the "shell leak").
//
// It stays a server component and mounts ConsoleShell (a client component reading the pathname) so
// the project-rooted, responsive nav wraps every authenticated page: desktop/tablet rail, mobile
// bottom nav + drawer, Project selector, and breadcrumb.

import type { ReactNode } from "react";
import { ConsoleShell } from "@/components/nav/ConsoleShell";
import { viewerAccess } from "@/lib/session";

export default async function AppLayout({ children }: { children: ReactNode }) {
  // Story 8.16: resolve the viewer's global role server-side so the shell renders the role-adapted
  // nav (admin-only nodes present only for admins). Fails closed to "user" (lib/session.ts).
  const access = await viewerAccess();
  return <ConsoleShell access={access}>{children}</ConsoleShell>;
}
