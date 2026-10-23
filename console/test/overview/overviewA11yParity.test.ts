// test/overview/overviewA11yParity.test.ts — WS-5 (ISI-5710) accessibility gate for the
// overview redesign across the WS-3 Fleet recompose (FleetOverview) and the WS-4 per-project
// overview (ProjectOverviewDashboard). Both surfaces render runs/agent state through ONE
// theme-driven color system: the semantic --status-* / --ksq-run / --accent hues (globals.css),
// painted onto the themed --surface / --inset chart canvases via the ov-tone-* / mixbar classes
// (overview.css) and the chart primitives.
//
// This file locks the two things that are measurable about that system and were the open WS-5
// deliverable once WS-3/WS-4 landed:
//
//   (A) WCAG color-channel safety per the `dataviz` skill — graphical-object contrast
//       (WCAG 1.4.11, 3:1) of each status/run/accent FILL against each theme's chart surface,
//       plus colorblind (CVD) + normal-vision separation of the status palette. The sub-3:1
//       "relief" band the skill permits is legal ONLY with mandatory secondary encoding, so the
//       component tests below prove that encoding (a text label / enumerating aria-label) exists.
//   (B) Dark/light PARITY — every surface+ink token role the overview consumes is defined in BOTH
//       [data-theme] blocks, and the reserved status/brand hues are theme-invariant (defined once
//       in :root, never per-theme). This guards the ISI-5701 class of bug where a role present in
//       only one theme bleeds the wrong value when the theme flips.
//
// The color math (sRGB→linear→OKLab, WCAG relative luminance, Machado-Oliveira-Fernandes 2009 CVD
// simulation at severity 1.0) is a faithful, self-contained port of the `dataviz` skill's
// scripts/validate_palette.js so CI needs no external skill checkout. Keep in lockstep with it.

import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { createElement } from "react";
import { RunStatusMixBar } from "@/components/overview/RunStatusMixBar";
import {
  StackedAreaChart,
  type AreaSeries,
} from "@/components/overview/StackedAreaChart";
import { MIX_STATES } from "@/components/overview/tone";

afterEach(cleanup);

// ── color math (ported from dataviz skill validate_palette.js) ──────────────────────────────────
const hex2srgb = (h: string) => {
  const s = h.trim().replace(/^#/, "");
  return [0, 2, 4].map((i) => parseInt(s.slice(i, i + 2), 16) / 255);
};
const s2lin = (c: number) =>
  c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
const lin = (h: string) => hex2srgb(h).map(s2lin);
const relLum = (h: string) => {
  const [r, g, b] = lin(h);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};
/** WCAG contrast ratio between two opaque hexes. */
function contrast(a: string, b: string): number {
  const [hi, lo] = [relLum(a), relLum(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}
function oklabFromLin([r, g, b]: number[]): [number, number, number] {
  const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b);
  const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b);
  const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
  return [
    0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s,
    1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s,
    0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s,
  ];
}
// Machado, Oliveira & Fernandes (2009) CVD transforms at severity 1.0 (linear RGB).
const MACHADO: Record<string, number[][]> = {
  protan: [
    [0.152286, 1.052583, -0.204868],
    [0.114503, 0.786281, 0.099216],
    [-0.003882, -0.048116, 1.051998],
  ],
  deutan: [
    [0.367322, 0.860646, -0.227968],
    [0.280085, 0.672501, 0.047413],
    [-0.01182, 0.04294, 0.968881],
  ],
};
function simulate(h: string, kind: string) {
  const [r, g, b] = lin(h);
  const M = MACHADO[kind];
  const clamp = (c: number) => Math.max(0, Math.min(1, c));
  return [
    clamp(M[0][0] * r + M[0][1] * g + M[0][2] * b),
    clamp(M[1][0] * r + M[1][1] * g + M[1][2] * b),
    clamp(M[2][0] * r + M[2][1] * g + M[2][2] * b),
  ];
}
/** OKLab ΔE ×100. `kind` undefined → unsimulated (normal) vision. */
function deltaE(h1: string, h2: string, kind?: string): number {
  const a = oklabFromLin(kind ? simulate(h1, kind) : lin(h1));
  const b = oklabFromLin(kind ? simulate(h2, kind) : lin(h2));
  return 100 * Math.hypot(a[0] - b[0], a[1] - b[1], a[2] - b[2]);
}

// ── parse the themed token system out of globals.css ─────────────────────────────────────────────
// Vitest runs with cwd at the console/ package root (where vitest.config.ts lives).
const GLOBALS = readFileSync(resolve(process.cwd(), "app/globals.css"), "utf8");

/** Extract the `--name: value;` declarations inside the first `<selector> { ... }` block. */
function blockTokens(css: string, selector: string): Record<string, string> {
  const at = css.indexOf(selector);
  if (at < 0) throw new Error(`selector not found in globals.css: ${selector}`);
  const open = css.indexOf("{", at);
  const close = css.indexOf("}", open);
  const body = css.slice(open + 1, close);
  const out: Record<string, string> = {};
  for (const m of body.matchAll(/(--[a-z0-9-]+)\s*:\s*([^;]+);/gi)) {
    out[m[1]] = m[2].trim();
  }
  return out;
}

const ROOT = blockTokens(GLOBALS, ":root");
const DARK = blockTokens(GLOBALS, '[data-theme="dark"]');
const LIGHT = blockTokens(GLOBALS, '[data-theme="light"]');

/** The reserved status/brand hues — theme-invariant, defined once in :root. */
const STATUS_HEX: Record<(typeof MIX_STATES)[number], string> = {
  running: ROOT["--status-running"],
  paused: ROOT["--status-paused"],
  blocked: ROOT["--status-blocked"],
  idle: ROOT["--status-idle"],
};
const RUN_HEX = ROOT["--ksq-run"];
const ACCENT_HEX = ROOT["--accent"];

/** The chart canvases a fill can land on, per theme. */
const SURFACES = {
  dark: { surface: DARK["--surface"], inset: DARK["--inset"] },
  light: { surface: LIGHT["--surface"], inset: LIGHT["--inset"] },
};

const GRAPHICAL_MIN = 3.0; // WCAG 1.4.11 non-text contrast
const CVD_FLOOR = 6.0; // OKLab ΔE×100, min(protan,deutan), adjacent — skill WARN floor
const NORMAL_FLOOR = 15.0; // OKLab ΔE×100, unsimulated — skill hard gate

const ALL_FILLS: Record<string, string> = {
  ...STATUS_HEX,
  run: RUN_HEX,
  accent: ACCENT_HEX,
};

describe("overview color system — token hygiene", () => {
  it("resolves every status/brand hue from :root as a concrete hex", () => {
    for (const [name, hex] of Object.entries(ALL_FILLS)) {
      expect(hex, `missing token for ${name}`).toMatch(/^#[0-9a-fA-F]{6}$/);
    }
  });

  it("keeps the reserved status/brand hues theme-invariant (never redefined per-theme)", () => {
    // Redefining a status hue inside a [data-theme] block is the bug: green would stop meaning
    // "running" identically across themes (DESIGN-SPEC-ISI-4505 §2) and silently drift on flip.
    for (const token of [
      "--status-running",
      "--status-paused",
      "--status-blocked",
      "--status-idle",
      "--ksq-run",
      "--accent",
    ]) {
      expect(
        DARK[token],
        `${token} must not be redefined in dark`,
      ).toBeUndefined();
      expect(
        LIGHT[token],
        `${token} must not be redefined in light`,
      ).toBeUndefined();
      expect(ROOT[token]).toBeDefined();
    }
  });
});

describe("dark/light parity (ISI-5701 guard)", () => {
  it("defines every surface + ink role the overview consumes in BOTH themes", () => {
    // The overview paints onto these roles (overview.css/dashboard.css). A role present in only
    // one theme block bleeds the other theme's value on flip — the ISI-5701 light→dark reset bug.
    const ROLES = [
      "--canvas",
      "--surface",
      "--inset",
      "--border",
      "--muted",
      "--text",
      "--text-hi",
      "--text-1",
      "--text-2",
    ];
    for (const role of ROLES) {
      expect(DARK[role], `dark missing ${role}`).toBeDefined();
      expect(LIGHT[role], `light missing ${role}`).toBeDefined();
    }
    // Same role set on both sides — no asymmetric key present in one theme only.
    expect(Object.keys(DARK).sort()).toEqual(Object.keys(LIGHT).sort());
  });
});

describe("WCAG color-channel safety per dataviz skill", () => {
  it("dark theme: every status/run/accent fill clears the 3:1 graphical floor on both canvases", () => {
    for (const [name, hex] of Object.entries(ALL_FILLS)) {
      for (const [canvas, surf] of Object.entries(SURFACES.dark)) {
        const r = contrast(hex, surf);
        expect(
          r,
          `${name} vs dark/${canvas} = ${r.toFixed(2)}:1`,
        ).toBeGreaterThanOrEqual(GRAPHICAL_MIN);
      }
    }
  });

  it("light theme: any sub-3:1 fill is in the skill's relief band, which mandates secondary encoding", () => {
    // The bright status hues are < 3:1 as bare fills on the white light surface. The dataviz skill
    // permits this ONLY with mandatory secondary encoding (visible labels / table view). We assert
    // the relief is bounded (fills are still visible, not vanishing into the surface) here, and
    // prove the required label/legend encoding in the component tests below.
    for (const [name, hex] of Object.entries(ALL_FILLS)) {
      const r = contrast(hex, SURFACES.light.surface);
      // Never collapse to invisibility: even a relief-band fill must stay perceptibly off-surface.
      expect(r, `${name} vs light/surface = ${r.toFixed(2)}:1`).toBeGreaterThan(
        1.3,
      );
    }
  });

  it("status palette is colorblind-separable (CVD + normal-vision floors) in MIX render order", () => {
    const pal = MIX_STATES.map((s) => STATUS_HEX[s]);
    for (let i = 0; i < pal.length - 1; i++) {
      const [a, b] = [pal[i], pal[i + 1]];
      const cvd = Math.min(deltaE(a, b, "protan"), deltaE(a, b, "deutan"));
      const normal = deltaE(a, b);
      expect(
        cvd,
        `CVD ΔE ${MIX_STATES[i]}↔${MIX_STATES[i + 1]} = ${cvd.toFixed(1)}`,
      ).toBeGreaterThanOrEqual(CVD_FLOOR);
      expect(
        normal,
        `normal ΔE ${MIX_STATES[i]}↔${MIX_STATES[i + 1]} = ${normal.toFixed(1)}`,
      ).toBeGreaterThanOrEqual(NORMAL_FLOOR);
    }
  });
});

describe("identity is never color-alone (WCAG 1.4.1 — the relief encoding)", () => {
  it("RunStatusMixBar carries an enumerating aria-label for every state, not color only", () => {
    render(
      createElement(RunStatusMixBar, {
        counts: { running: 3, paused: 1, blocked: 0, idle: 2 },
      }),
    );
    const bar = screen.getByRole("img");
    const label = bar.getAttribute("aria-label") ?? "";
    for (const s of MIX_STATES) expect(label).toContain(s);
    expect(label).toBe("3 running, 1 paused, 0 blocked, 2 idle");
  });

  it("RunStatusMixBar degrades to a labelled empty track (no blank/broken bar)", () => {
    render(createElement(RunStatusMixBar, { counts: {} }));
    expect(screen.getByRole("img").getAttribute("aria-label")).toBe("no runs");
  });

  it("StackedAreaChart labels each series in text + describes the plot for SR (not hue-only)", () => {
    const series: AreaSeries[] = [
      {
        key: "running",
        label: "Running",
        color: "var(--status-running)",
        values: [1, 2, 3],
      },
      {
        key: "blocked",
        label: "Blocked",
        color: "var(--status-blocked)",
        values: [0, 1, 0],
      },
    ];
    render(
      createElement(StackedAreaChart, { series, caption: "runs over time" }),
    );
    // Text label beside each swatch — identity does not rely on the swatch color.
    expect(screen.getByText("Running")).toBeInTheDocument();
    expect(screen.getByText("Blocked")).toBeInTheDocument();
    // The plot itself is a labelled role=img so a screen reader gets the data without the legend.
    expect(screen.getByRole("img").getAttribute("aria-label")).toBeTruthy();
  });
});
