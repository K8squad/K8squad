"use client";

// components/ThemeProvider.tsx — whole-shell theming (story 8.9).
//
// The toggle is a v1 TOKEN SWAP: it sets `data-theme` on <html>, and globals.css maps every
// token role from the DARK/LIGHT maps (lib/theme.ts). One accent, invariant across themes;
// reserved status hues invariant.
//
// Persistence (ISI-5701): the choice is durable in localStorage. On mount we re-read the stored
// theme and apply it, so a hard reload or a run-page mount (which re-runs the root layout with its
// DEFAULT_THEME `data-theme`) can no longer snap the shell back to dark. A pre-hydration inline
// script (lib/theme.ts THEME_INIT_SCRIPT, injected in app/layout.tsx) stamps `data-theme` before
// first paint; this effect reconciles React state and covers the no-script / late-hydration case.

import { createContext, useCallback, useContext, useEffect, useState } from "react";
import type { ReactNode } from "react";
import {
  DEFAULT_THEME,
  persistTheme,
  readStoredTheme,
  type ThemeName,
} from "@/lib/theme";

type ThemeContextValue = {
  theme: ThemeName;
  toggle: () => void;
  setTheme: (t: ThemeName) => void;
};

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  // Start from DEFAULT_THEME so SSR and the first client render agree (no hydration mismatch);
  // the effect below immediately reconciles to the persisted choice.
  const [theme, setThemeState] = useState<ThemeName>(DEFAULT_THEME);

  const apply = useCallback((t: ThemeName) => {
    setThemeState(t);
    if (typeof document !== "undefined") {
      document.documentElement.setAttribute("data-theme", t);
    }
    if (typeof window !== "undefined") {
      persistTheme(t, window.localStorage);
    }
  }, []);

  // On mount, restore the persisted theme (the inline script already stamped the attribute pre-paint;
  // this syncs React state so the toggle label and context are correct).
  useEffect(() => {
    const stored = readStoredTheme(
      typeof window !== "undefined" ? window.localStorage : null,
    );
    apply(stored);
  }, [apply]);

  // Keep the attribute in sync across tabs: adopt a theme change persisted by another tab.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const onStorage = () => apply(readStoredTheme(window.localStorage));
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, [apply]);

  const toggle = useCallback(() => {
    apply(theme === "dark" ? "light" : "dark");
  }, [apply, theme]);

  return (
    <ThemeContext.Provider value={{ theme, toggle, setTheme: apply }}>
      {children}
    </ThemeContext.Provider>
  );
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme must be used within <ThemeProvider>");
  return ctx;
}
