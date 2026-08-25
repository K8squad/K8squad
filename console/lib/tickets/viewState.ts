// lib/tickets/viewState.ts — view-toggle persistence (story 8.14d).
//
// The Kanban↔List choice is a READ/ORGANIZATION preference, never coord state
// (R6 scope guard): it persists per user via localStorage AND the `?view=` URL
// param, so a reload or a shared deep-link restores the same view. URL param
// wins on load (explicit deep-link intent); the toggle writes both.

export type TicketsView = "kanban" | "list";

export const VIEW_STORAGE_KEY = "ksq.tickets.view";

export function isTicketsView(v: string | null | undefined): v is TicketsView {
  return v === "kanban" || v === "list";
}

/** Resolve the initial view: `?view=` param first, then localStorage, else Kanban. */
export function resolveInitialView(search: string, storage: Storage | null): TicketsView {
  const fromUrl = new URLSearchParams(search).get("view");
  if (isTicketsView(fromUrl)) return fromUrl;
  try {
    const stored = storage?.getItem(VIEW_STORAGE_KEY) ?? null;
    if (isTicketsView(stored)) return stored;
  } catch {
    // Private mode / storage disabled — fall through to the default.
  }
  return "kanban";
}

/** Persist the choice to localStorage (best-effort) — URL sync is the caller's job. */
export function persistView(view: TicketsView, storage: Storage | null): void {
  try {
    storage?.setItem(VIEW_STORAGE_KEY, view);
  } catch {
    // Best-effort only; the ?view= param still carries the deep-link.
  }
}
