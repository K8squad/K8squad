// components/Logo.tsx — the v2 8-Crest rail lockup (story 8.9 AC2).
//
// Simplified in-shell rendition of the official gradient-ring 8-Crest mark
// (docs/bmad/branding/assets/mark-8crest-on-dark.svg, ISI-2137). The ring gradient is the brand
// signature; the accent stop is the theme-invariant azure. This is the placeholder-free mark the
// theming contract requires on every screen — no flat-stroke ring rect (AC2).

import { ACCENT } from '@/lib/theme';

export function Logo({ size = 28, withWordmark = true }: { size?: number; withWordmark?: boolean }) {
  return (
    <span className="brand-lockup" style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}>
      <svg
        width={size}
        height={size}
        viewBox="0 0 32 32"
        role="img"
        aria-label="KSquad 8-Crest logo"
        fill="none"
      >
        <defs>
          <linearGradient id="ringTop" x1="0" y1="0" x2="1" y2="1">
            <stop offset="0" stopColor={ACCENT} />
            <stop offset="1" stopColor="#7AA7FF" />
          </linearGradient>
          <linearGradient id="ringBot" x1="1" y1="1" x2="0" y2="0">
            <stop offset="0" stopColor={ACCENT} />
            <stop offset="1" stopColor="#2A5BD7" />
          </linearGradient>
        </defs>
        <circle cx="16" cy="16" r="11" stroke="url(#ringTop)" strokeWidth="3" strokeDasharray="34 18" />
        <circle cx="16" cy="16" r="11" stroke="url(#ringBot)" strokeWidth="3" strokeDasharray="34 18" strokeDashoffset="34" opacity="0.85" />
        <circle cx="16" cy="16" r="3.2" fill={ACCENT} />
      </svg>
      {withWordmark && <strong className="brand-wordmark">KSquad</strong>}
    </span>
  );
}
