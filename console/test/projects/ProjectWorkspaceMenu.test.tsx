// test/projects/ProjectWorkspaceMenu.test.tsx — the workspace left menu client island (ISI-3957 S1).
// Covers AC1 (six entries render; collapse toggle persists to localStorage; keyboard-operable),
// AC3 (active entry derives from the pathname, marked aria-current).

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, fireEvent, within } from "@testing-library/react";

// Mutable pathname the next/navigation mock reads — set per test before render.
let currentPath = "/projects/webapp";

// next/link → plain anchor (no AppRouter context in jsdom); usePathname is driven by currentPath.
vi.mock("next/link", () => ({
  __esModule: true,
  default: ({ href, children, ...rest }: any) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}));
vi.mock("next/navigation", () => ({ usePathname: () => currentPath }));

import { ProjectWorkspaceMenu } from "@/components/projects/ProjectWorkspaceMenu";

beforeEach(() => {
  currentPath = "/projects/webapp";
  window.localStorage.clear();
});
afterEach(cleanup);

describe("ProjectWorkspaceMenu — AC1 menu", () => {
  it("renders the six sections in UX order with correct hrefs", () => {
    render(<ProjectWorkspaceMenu projectId="webapp" />);
    const links = within(screen.getByRole("navigation", { name: "Project sections" })).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual([
      "Landing",
      "Issues",
      "Runs",
      "Discussion",
      "File Explorer",
      "GitHub",
    ]);
    expect(screen.getByTestId("pmenu-landing")).toHaveAttribute("href", "/projects/webapp");
    expect(screen.getByTestId("pmenu-issues")).toHaveAttribute("href", "/projects/webapp/issues");
    expect(screen.getByTestId("pmenu-files")).toHaveAttribute("href", "/projects/webapp/files");
  });

  it("collapse toggles and persists to localStorage; labels hide via data-collapsed", () => {
    render(<ProjectWorkspaceMenu projectId="webapp" />);
    const nav = screen.getByRole("navigation", { name: "Project sections" });
    const toggle = screen.getByRole("button", { name: "Collapse project menu" });

    expect(nav).not.toHaveAttribute("data-collapsed");
    fireEvent.click(toggle);
    expect(nav).toHaveAttribute("data-collapsed");
    expect(window.localStorage.getItem("ksq:workspace-menu-collapsed")).toBe("1");

    // Re-expand.
    fireEvent.click(screen.getByRole("button", { name: "Expand project menu" }));
    expect(nav).not.toHaveAttribute("data-collapsed");
    expect(window.localStorage.getItem("ksq:workspace-menu-collapsed")).toBe("0");
  });

  it("restores the collapsed preference from localStorage on mount", () => {
    window.localStorage.setItem("ksq:workspace-menu-collapsed", "1");
    render(<ProjectWorkspaceMenu projectId="webapp" />);
    expect(screen.getByRole("navigation", { name: "Project sections" })).toHaveAttribute("data-collapsed");
    // Labels still present in the DOM (text equivalent preserved), just CSS-hidden.
    expect(screen.getByTestId("pmenu-landing")).toHaveAttribute("aria-label", "Landing");
  });

  it("entries are real, focusable links (keyboard-operable)", () => {
    render(<ProjectWorkspaceMenu projectId="webapp" />);
    const link = screen.getByTestId("pmenu-runs");
    link.focus();
    expect(document.activeElement).toBe(link);
  });
});

describe("ProjectWorkspaceMenu — AC3 active highlight", () => {
  it("marks Landing active at the bare project root", () => {
    currentPath = "/projects/webapp";
    render(<ProjectWorkspaceMenu projectId="webapp" />);
    expect(screen.getByTestId("pmenu-landing")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("pmenu-issues")).not.toHaveAttribute("aria-current");
  });

  it("marks the section active from a deep route", () => {
    currentPath = "/projects/webapp/github";
    render(<ProjectWorkspaceMenu projectId="webapp" />);
    expect(screen.getByTestId("pmenu-github")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("pmenu-github")).toHaveAttribute("data-active", "true");
    expect(screen.getByTestId("pmenu-landing")).not.toHaveAttribute("aria-current");
  });

  it("AC6 — the legacy /tickets route highlights nothing", () => {
    currentPath = "/projects/webapp/tickets";
    render(<ProjectWorkspaceMenu projectId="webapp" />);
    for (const id of ["landing", "issues", "runs", "discussion", "files", "github"]) {
      expect(screen.getByTestId(`pmenu-${id}`)).not.toHaveAttribute("aria-current");
    }
  });
});
