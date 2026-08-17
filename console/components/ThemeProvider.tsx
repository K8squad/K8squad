'use client';

// components/ThemeProvider.tsx — whole-shell theming (story 8.9).
//
// The toggle is a v1 TOKEN SWAP: it sets `data-theme` on <html>, and globals.css maps every
// token role from the DARK/LIGHT maps (lib/theme.ts). One accent, invariant across themes;
// reserved status hues invariant. Runtime persistence (localStorage / prefers-color-scheme) is
// explicitly out of scope for v1 (story 8.9 "Out of scope"); the mechanism is the token swap.

import { createContext, useCallback, useContext, useState } from 'react';
import type { ReactNode } from 'react';
import { DEFAULT_THEME, type ThemeName } from '@/lib/theme';

type ThemeContextValue = {
  theme: ThemeName;
  toggle: () => void;
  setTheme: (t: ThemeName) => void;
};

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setThemeState] = useState<ThemeName>(DEFAULT_THEME);

  const apply = useCallback((t: ThemeName) => {
    setThemeState(t);
    if (typeof document !== 'undefined') {
      document.documentElement.setAttribute('data-theme', t);
    }
  }, []);

  const toggle = useCallback(() => {
    apply(theme === 'dark' ? 'light' : 'dark');
  }, [apply, theme]);

  return (
    <ThemeContext.Provider value={{ theme, toggle, setTheme: apply }}>
      {children}
    </ThemeContext.Provider>
  );
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error('useTheme must be used within <ThemeProvider>');
  return ctx;
}
