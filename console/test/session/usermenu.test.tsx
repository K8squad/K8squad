// test/session/usermenu.test.tsx — the sign-out control's behavior (ISI-3570).
//
// The gap this closes: the authenticated Console had no way to end a session. UserMenu is that
// control. The non-trivial logic is the sign-out handler: it must call DELETE /api/session (the BFF
// leg that invalidates the server session + clears the cookie) and then land the user on /login —
// including the failure path, where a network error must STILL send the user to /login (fail toward
// signed-out, never trap them in the shell).

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { UserMenu } from "@/components/nav/UserMenu";

const realFetch = globalThis.fetch;

// jsdom's window.location.assign is a non-configurable no-op; stub it so we can assert the redirect.
const assign = vi.fn();
Object.defineProperty(window, "location", {
  value: { ...window.location, assign },
  writable: true,
});

afterEach(() => {
  globalThis.fetch = realFetch;
  vi.restoreAllMocks();
  assign.mockReset();
});

describe("UserMenu — the sign-out control", () => {
  it("shows the signed-in username", () => {
    render(<UserMenu username="operator" />);
    expect(screen.getByText("operator")).toBeTruthy();
  });

  it("falls back to a generic label when identity is unresolved", () => {
    render(<UserMenu username={null} />);
    expect(screen.getByText("Account")).toBeTruthy();
  });

  it("DELETEs /api/session then hard-navigates to /login on click", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    render(<UserMenu username="operator" />);
    fireEvent.click(screen.getByRole("button", { name: /sign out/i }));

    await waitFor(() => expect(assign).toHaveBeenCalledWith("/login"));
    expect(fetchMock).toHaveBeenCalledWith("/api/session", { method: "DELETE" });
  });

  it("still navigates to /login when the logout call fails (fail toward signed-out)", async () => {
    const fetchMock = vi.fn(async () => {
      throw new Error("network down");
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    render(<UserMenu username="operator" />);
    fireEvent.click(screen.getByRole("button", { name: /sign out/i }));

    await waitFor(() => expect(assign).toHaveBeenCalledWith("/login"));
  });
});
