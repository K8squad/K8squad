// components/Logo.tsx — the official v2 8-Crest lockup (ISI-3529 branding fix; story 8.9 AC2).
//
// Faithful in-shell rendition of the canonical 8-Crest mark
// (assets/logo/svg/mark-8crest-on-dark.svg, ISI-2137): the shared `8` of `K8s`/`K8squad` drawn as
// two stacked rounded-square squad-containers pinched at a bright coordinator node. This geometry —
// NOT a dashed circular ring — is the brand signature the theming contract requires on every screen.
//
// Geometry + palette are copied verbatim from the canonical SVG (100-unit design space, minus the
// dark app-icon background tile so the mark sits inline on any surface). The azure stops use the
// theme-invariant ACCENT (#3D7DFF, == the canonical Squad Azure); the lead tint #93B7FF and recede
// depth #2E4E8C are literal per the azure-mono palette (assets/logo/README.md). Gradient ids are
// per-instance (useId) so multiple <Logo>s on one page never collide on url(#id).

import { useId } from "react";
import { ACCENT } from "@/lib/theme";

// Palette — azure-mono family, brand-locked (never re-tints to status hues). See logo README.
const LEAD = "#93B7FF"; // lead / highlight tint
const RECEDE = "#2E4E8C"; // recede depth mid

export function Logo({
  size = 28,
  withWordmark = true,
}: {
  size?: number;
  withWordmark?: boolean;
}) {
  const uid = useId();
  const ringTop = `ringTop-${uid}`;
  const ringBot = `ringBot-${uid}`;
  return (
    <span
      className="brand-lockup"
      style={{ display: "inline-flex", alignItems: "center", gap: 10 }}
    >
      <svg
        width={size}
        height={size}
        viewBox="0 0 100 100"
        role="img"
        aria-label="K8squad 8-Crest logo"
        fill="none"
      >
        <defs>
          <linearGradient id={ringTop} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" stopColor={LEAD} />
            <stop offset="1" stopColor={ACCENT} />
          </linearGradient>
          <linearGradient id={ringBot} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" stopColor={ACCENT} />
            <stop offset="1" stopColor={RECEDE} />
          </linearGradient>
        </defs>
        {/* lower crest (recedes) */}
        <rect
          x="29"
          y="45"
          width="42"
          height="42"
          rx="13"
          fill="none"
          stroke={`url(#${ringBot})`}
          strokeWidth="9"
        />
        {/* upper crest (leads) */}
        <rect
          x="29"
          y="13"
          width="42"
          height="42"
          rx="13"
          fill="none"
          stroke={`url(#${ringTop})`}
          strokeWidth="9"
        />
        {/* coordinator node — the bright pinch that fuses the two crests */}
        <rect x="41" y="41" width="18" height="18" rx="5" fill={LEAD} />
        {/* squad member nodes */}
        <rect x="44.5" y="9.5" width="11" height="11" rx="3" fill={LEAD} />
        <rect x="44.5" y="79.5" width="11" height="11" rx="3" fill={RECEDE} />
      </svg>
      {withWordmark && (
        <strong className="brand-wordmark">
          K<span style={{ color: ACCENT }}>8</span>squad
        </strong>
      )}
    </span>
  );
}
