// lib/theme.ts — the DARK/LIGHT design-token maps (story 8.9).
//
// This mirrors the locked visual system in docs/bmad/ux/console_kit.py (UX §0 / ISI-2150,
// ISI-2324). The toggle is a v1 TOKEN SWAP, not a redesign: light mode is the dark shell with
// its token ROLES luminance-inverted. Two invariants are load-bearing (theme-light-parity-check):
//   - the azure accent is THEME-INVARIANT (identical in both themes) — one brand hue, no re-tint;
//   - the reserved STATUS hues are vivid and identical across both themes (only the surrounding
//     surface tint changes on toggle) and never collapse onto the accent.
// AC6 role-inversion: the dark canvas navy #0B1220 reappears in light mode in a TEXT role.

export type ThemeName = 'dark' | 'light';

/** Brand accent — a single azure, invariant across themes (AC3). */
export const ACCENT = '#3D7DFF';

/** Reserved status dot hues — vivid and identical in both themes (AC7). */
export const STATUS = {
  running: '#34D399', // green
  paused: '#FBBF24', // amber
  blocked: '#FB7185', // rose (also failed)
  idle: '#64748B', // slate
} as const;

export type ThemeTokens = {
  canvas: string;
  surface: string;
  border: string;
  text1: string;
  text2: string;
  accent: string;
};

// Dark: navy canvas, light text.
export const DARK: ThemeTokens = {
  canvas: '#0B1220',
  surface: '#111A2E',
  border: '#22304D',
  text1: '#E6EDF7',
  text2: '#9FB0C9',
  accent: ACCENT,
};

// Light: the SAME token roles, luminance-inverted. The dark canvas navy #0B1220 returns as the
// primary TEXT role (AC6); the accent and status hues are unchanged (AC3/AC7).
export const LIGHT: ThemeTokens = {
  canvas: '#F6F8FC',
  surface: '#FFFFFF',
  border: '#D6DEEC',
  text1: '#0B1220',
  text2: '#4B5B76',
  accent: ACCENT,
};

export const THEMES: Record<ThemeName, ThemeTokens> = { dark: DARK, light: LIGHT };

export const DEFAULT_THEME: ThemeName = 'dark';
