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

// Canonical 8-Crest geometry (100-unit design space), copied verbatim from
// mark-8crest-on-dark.svg. Centralised as named constants so the two crests / member nodes stay
// consistent and a brand tweak is a one-line edit rather than a hunt through inline literals.
const CREST = { x: 29, size: 42, rx: 13, strokeWidth: 9 } as const; // the two stacked squad-containers
const CREST_Y = { top: 13, bottom: 45 } as const;
const MEMBER = { x: 44.5, size: 11, rx: 3 } as const; // top/bottom squad-member nodes
const MEMBER_Y = { top: 9.5, bottom: 79.5 } as const;
const NODE = { x: 41, y: 41, size: 18, rx: 5 } as const; // bright coordinator pinch fusing the crests

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
    <span className="brand-lockup">
      <svg
        width={size}
        height={size}
        viewBox="0 0 100 100"
        fill="none"
        // When the wordmark is shown it supplies the single accessible name ("K8squad"); the mark
        // is then decorative, so hide it from AT to avoid a duplicate announcement. Standalone, the
        // SVG carries the name itself.
        role={withWordmark ? undefined : "img"}
        aria-hidden={withWordmark || undefined}
        aria-label={withWordmark ? undefined : "K8squad 8-Crest logo"}
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
          x={CREST.x}
          y={CREST_Y.bottom}
          width={CREST.size}
          height={CREST.size}
          rx={CREST.rx}
          fill="none"
          stroke={`url(#${ringBot})`}
          strokeWidth={CREST.strokeWidth}
        />
        {/* upper crest (leads) */}
        <rect
          x={CREST.x}
          y={CREST_Y.top}
          width={CREST.size}
          height={CREST.size}
          rx={CREST.rx}
          fill="none"
          stroke={`url(#${ringTop})`}
          strokeWidth={CREST.strokeWidth}
        />
        {/* coordinator node — the bright pinch that fuses the two crests */}
        <rect x={NODE.x} y={NODE.y} width={NODE.size} height={NODE.size} rx={NODE.rx} fill={LEAD} />
        {/* squad member nodes */}
        <rect x={MEMBER.x} y={MEMBER_Y.top} width={MEMBER.size} height={MEMBER.size} rx={MEMBER.rx} fill={LEAD} />
        <rect x={MEMBER.x} y={MEMBER_Y.bottom} width={MEMBER.size} height={MEMBER.size} rx={MEMBER.rx} fill={RECEDE} />
      </svg>
      {withWordmark && (
        <strong className="brand-wordmark">
          K<span className="brand-wordmark__accent">8</span>squad
        </strong>
      )}
    </span>
  );
}
