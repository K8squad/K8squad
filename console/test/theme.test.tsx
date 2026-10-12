// test/theme.test.tsx — theme persistence (ISI-5701).
//
// The 8.9 toggle was a token swap with NO persistence: DEFAULT_THEME ("dark") was
// both the SSR attribute and the provider's initial state, so a hard reload or a
// run-page mount re-defaulted the shell to dark and discarded a "light" choice.
// These cover the durable-store contract: the helpers round-trip to localStorage,
// the pre-hydration script restores before paint, and the provider reconciles React
// state on mount instead of forcing the dark default.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, act } from "@testing-library/react";
import {
  DEFAULT_THEME,
  THEME_STORAGE_KEY,
  THEME_INIT_SCRIPT,
  isThemeName,
  readStoredTheme,
  persistTheme,
} from "@/lib/theme";
import { ThemeProvider, useTheme } from "@/components/ThemeProvider";
import { ThemeToggle } from "@/components/ThemeToggle";

afterEach(() => {
  cleanup();
  window.localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
});

describe("theme persistence helpers", () => {
  it("isThemeName narrows only the two valid names", () => {
    expect(isThemeName("dark")).toBe(true);
    expect(isThemeName("light")).toBe(true);
    expect(isThemeName("solarized")).toBe(false);
    expect(isThemeName(null)).toBe(false);
    expect(isThemeName(undefined)).toBe(false);
  });

  it("readStoredTheme returns the stored valid choice", () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, "light");
    expect(readStoredTheme(window.localStorage)).toBe("light");
  });

  it("readStoredTheme falls back to the default for missing/garbage/null", () => {
    expect(readStoredTheme(window.localStorage)).toBe(DEFAULT_THEME);
    window.localStorage.setItem(THEME_STORAGE_KEY, "neon");
    expect(readStoredTheme(window.localStorage)).toBe(DEFAULT_THEME);
    expect(readStoredTheme(null)).toBe(DEFAULT_THEME);
  });

  it("persistTheme round-trips through storage", () => {
    persistTheme("light", window.localStorage);
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");
    expect(readStoredTheme(window.localStorage)).toBe("light");
  });

  it("persist/read are best-effort when storage throws (private mode)", () => {
    const throwing = {
      getItem: () => {
        throw new Error("denied");
      },
      setItem: () => {
        throw new Error("denied");
      },
    } as unknown as Storage;
    expect(() => persistTheme("light", throwing)).not.toThrow();
    expect(readStoredTheme(throwing)).toBe(DEFAULT_THEME);
  });
});

describe("THEME_INIT_SCRIPT (pre-hydration restore)", () => {
  it("stamps the stored theme onto <html> before paint", () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, "light");
    // The script runs against the ambient document/localStorage.
    // eslint-disable-next-line no-eval
    (0, eval)(THEME_INIT_SCRIPT);
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("leaves the attribute untouched when nothing/garbage is stored", () => {
    document.documentElement.setAttribute("data-theme", DEFAULT_THEME);
    window.localStorage.setItem(THEME_STORAGE_KEY, "neon");
    // eslint-disable-next-line no-eval
    (0, eval)(THEME_INIT_SCRIPT);
    expect(document.documentElement.getAttribute("data-theme")).toBe(DEFAULT_THEME);
  });
});

function ThemeReadout() {
  const { theme } = useTheme();
  return <span data-testid="theme">{theme}</span>;
}

describe("ThemeProvider — persistence & restore", () => {
  it("restores the persisted 'light' theme on mount (not the dark default)", () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, "light");
    render(
      <ThemeProvider>
        <ThemeReadout />
      </ThemeProvider>,
    );
    expect(screen.getByTestId("theme")).toHaveTextContent("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("persists the choice so a remount (run-page mount / reload) keeps it", () => {
    const { unmount } = render(
      <ThemeProvider>
        <ThemeToggle />
        <ThemeReadout />
      </ThemeProvider>,
    );
    // Toggle dark -> light.
    fireEvent.click(screen.getByRole("button", { name: /switch to light mode/i }));
    expect(screen.getByTestId("theme")).toHaveTextContent("light");
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");

    // Simulate the run-page remount that used to reset to dark.
    unmount();
    document.documentElement.setAttribute("data-theme", DEFAULT_THEME);
    render(
      <ThemeProvider>
        <ThemeReadout />
      </ThemeProvider>,
    );
    expect(screen.getByTestId("theme")).toHaveTextContent("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("adopts a theme change persisted by another tab (storage event)", () => {
    render(
      <ThemeProvider>
        <ThemeReadout />
      </ThemeProvider>,
    );
    expect(screen.getByTestId("theme")).toHaveTextContent(DEFAULT_THEME);
    act(() => {
      window.localStorage.setItem(THEME_STORAGE_KEY, "light");
      window.dispatchEvent(new StorageEvent("storage", { key: THEME_STORAGE_KEY }));
    });
    expect(screen.getByTestId("theme")).toHaveTextContent("light");
  });
});
