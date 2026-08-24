"use client";

// components/nav/Breadcrumb.tsx — the always-visible `Project › section` trail (story 8.13).
// Pure derivation from the pathname via lib/nav.breadcrumbFor; the last crumb is the current
// location and renders as plain text (no self-link).

import Link from "next/link";
import { breadcrumbFor } from "@/lib/nav";

export function Breadcrumb({ pathname }: { pathname: string }) {
  const crumbs = breadcrumbFor(pathname);
  return (
    <nav className="crumbs" aria-label="Breadcrumb">
      <ol>
        {crumbs.map((c, i) => (
          <li key={`${c.label}-${i}`} aria-current={i === crumbs.length - 1 ? "page" : undefined}>
            {c.href !== null && i < crumbs.length - 1 ? (
              <Link href={c.href}>{c.label}</Link>
            ) : (
              <span>{c.label}</span>
            )}
          </li>
        ))}
      </ol>
    </nav>
  );
}
