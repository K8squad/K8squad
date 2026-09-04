"use client";

// components/nav/UserMenu.tsx — the signed-in account block + SIGN-OUT control (ISI-3570).
//
// Gap fix: the authenticated Console had no way to end a session from the UI. This is the account
// footer that lives at the bottom of the nav rail (and inside the mobile drawer): it shows who is
// signed in and gives a visible "Sign out" button.
//
// Sign-out contract (mirrors the login leg, ISI-3522): the button calls DELETE /api/session, the BFF
// route that proxies apiserver POST /auth/logout — the ONE authz choke point (§13 / ADR-013) — which
// invalidates the server-side session AND relays the cookie-clearing Set-Cookie so the HttpOnly
// ksquad_session actually drops from the browser. We then HARD-navigate to /login so the shell
// re-challenges with the cleared cookie (mirrors the login redirect). Even if the network call fails
// we still land on /login: a stale cookie is rejected upstream, so the worst case is a re-login, not
// a lingering "signed-in" shell.
//
// Deliberately an always-visible inline block, not a popover — a left-rail account footer is the
// standard pattern and avoids click-outside/focus-trap complexity. On the collapsed icon-rail the
// name hides (globals.css) and the avatar + button remain reachable.

import { useState } from "react";

// initial derives a single-letter avatar glyph from the username (upper-cased first char), falling
// back to a neutral bullet when identity could not be resolved (viewer() failed closed).
function initial(username: string | null): string {
  const c = username?.trim()?.[0];
  return c ? c.toUpperCase() : "•";
}

export function UserMenu({
  username,
  variant = "rail",
}: {
  username: string | null;
  variant?: "rail" | "drawer" | "avatar";
}) {
  const [busy, setBusy] = useState(false);

  // Avatar-only variant (ISI-3725): the top-bar identity glyph from the ISI-3641 mock. No sign-out
  // here — that control stays in the rail-foot rail/drawer variants; this is a read-only "who am I"
  // indicator so the avatar is not a duplicate action.
  if (variant === "avatar") {
    return (
      <span
        className="usermenu usermenu--avatar"
        title={username ?? "Account"}
        aria-label={username ? `Signed in as ${username}` : "Account"}
      >
        <span className="usermenu__avatar" aria-hidden="true">
          {initial(username)}
        </span>
      </span>
    );
  }

  async function signOut() {
    if (busy) return;
    setBusy(true);
    try {
      await fetch("/api/session", { method: "DELETE" });
    } catch {
      // Network error clearing the session: still send the user to /login. The stale cookie (if any)
      // is rejected by the apiserver on the next request, so we fail toward "signed out", never trap
      // the user in an un-exitable shell.
    }
    window.location.assign("/login");
  }

  return (
    <div className={`usermenu usermenu--${variant}`}>
      <div className="usermenu__id">
        <span className="usermenu__avatar" aria-hidden="true">
          {initial(username)}
        </span>
        <span className="usermenu__name">{username ?? "Account"}</span>
      </div>
      <button
        type="button"
        className="btn btn--danger usermenu__signout"
        onClick={signOut}
        disabled={busy}
      >
        {busy ? "Signing out…" : "Sign out"}
      </button>
    </div>
  );
}
