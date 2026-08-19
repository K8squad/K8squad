"use client";

// components/ThemeToggle.tsx — the shell theme toggle (story 8.9).

import { useTheme } from "@/components/ThemeProvider";

export function ThemeToggle() {
  const { theme, toggle } = useTheme();
  return (
    <button
      type="button"
      onClick={toggle}
      aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} mode`}
      className="theme-toggle"
    >
      {theme === "dark" ? "☾ Dark" : "☀ Light"}
    </button>
  );
}
