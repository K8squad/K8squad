"use client";

// app/login/page.tsx — the branded K8squad welcome + sign-in screen (ISI-3523 / parent ISI-3520).
//
// A full-screen, shell-free first impression: unauthenticated visitors land here instead of the
// old 404-inside-<ConsoleShell> leak. It is rendered OUTSIDE the operator nav shell by the login
// route group's bare layout (Architect child 7021f975 — a Next.js route-group split moves the
// authenticated pages under an (app) group and keeps /login on a minimal layout).
//
// Design system: reuses the token contract in app/globals.css verbatim — dark canvas #0b1220,
// surface #111a2e, theme-invariant azure accent #3D7DFF (--accent), 10px radius, the .btn/.field-*
// idioms — so this screen re-tints with the rest of the console and invents no new colors. The
// 8-Crest coordinator-node mark is the shared <Logo> lockup (components/Logo.tsx, ISI-2137/ISI-3529).
//
// Auth contract (verified against internal/apiserver/authroutes.go): the live method is username +
// password. The form POSTs same-origin JSON {username, password} to the BFF at /api/session, which
// proxies apiserver POST /auth/login → sets the opaque HttpOnly ksquad_session cookie. On success we
// redirect to the ?next= target (default "/"). OIDC/SSO is a future config-only leg (15.9), so it is
// a secondary, non-blocking affordance here — never a dead button.

import { useState, type FormEvent } from "react";
import { Logo } from "@/components/Logo";
import { sanitizeNext } from "./safeNext";

// Same-origin BFF endpoint the login form submits to. The Architect child adds the POST handler on
// console/app/api/session/route.ts (GET → /auth/me, POST → /auth/login). Kept as a const so the seam is
// obvious and easy to repoint.
const SESSION_ENDPOINT = "/api/session";

// safeNext reads the live ?next= off window and delegates to the pure, testable sanitizeNext
// (co-located in ./safeNext — kept out of this page.tsx so `next build` doesn't reject an
// extra named export on a route file).
function safeNext(): string {
  if (typeof window === "undefined") return "/";
  const raw = new URLSearchParams(window.location.search).get("next");
  return sanitizeNext(raw, window.location.origin);
}

export default function LoginPage() {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (submitting) return;
    setError(null);
    setSubmitting(true);
    try {
      const res = await fetch(SESSION_ENDPOINT, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      if (res.ok) {
        // The session cookie is set by the BFF/apiserver; hard-navigate so the authenticated shell
        // re-renders with the fresh cookie (and envoy re-evaluates the request).
        window.location.assign(safeNext());
        return;
      }
      // Map the apiserver's opaque auth shapes to human copy without leaking which factor failed.
      if (res.status === 401) setError("Invalid username or password.");
      else if (res.status === 429)
        setError("Too many attempts. Please wait a moment and try again.");
      else if (res.status === 400)
        setError("Enter both your username and password.");
      else setError("Sign-in is temporarily unavailable. Please try again.");
      setSubmitting(false);
    } catch {
      setError("Can’t reach the sign-in service. Check your connection and retry.");
      setSubmitting(false);
    }
  }

  return (
    <main className="login">
      {/* Brand panel — the welcome hero. Hidden on narrow viewports; the form leads on mobile. */}
      <section className="login__brand" aria-hidden="true">
        <div className="login__brand-inner">
          <Logo size={40} withWordmark={false} />
          <p className="login__eyebrow">K8squad Console</p>
          <h1 className="login__hero">
            Operate your agent squads with confidence.
          </h1>
          <p className="login__lede">
            The control surface for Kubernetes-native agent squads — compose CRDs,
            watch runs stream live, and govern every team from one place.
          </p>
          <ul className="login__points">
            <li>Live run streams &amp; provenance</li>
            <li>CRD authoring &amp; composition</li>
            <li>Role-scoped, audited access</li>
          </ul>
        </div>
      </section>

      {/* Form panel — the sign-in card. */}
      <section className="login__panel">
        <div className="login__card card">
          <div className="login__wordmark">
            <Logo size={28} withWordmark={false} />
            <strong className="login__brandname">K8squad</strong>
          </div>

          <h2 className="login__title">Sign in</h2>
          <p className="login__subtitle muted">
            Welcome back. Sign in to reach your console.
          </p>

          <form className="login__form" onSubmit={onSubmit} noValidate>
            {error && (
              <p className="login__error field-error" role="alert" aria-live="assertive">
                {error}
              </p>
            )}

            <label className="login__field">
              <span>Username</span>
              <input
                type="text"
                name="username"
                autoComplete="username"
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                required
                autoFocus
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                disabled={submitting}
              />
            </label>

            <label className="login__field">
              <span>Password</span>
              <input
                type="password"
                name="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={submitting}
              />
            </label>

            <button
              type="submit"
              className="btn btn--primary login__submit"
              disabled={submitting || !username || !password}
            >
              {submitting ? "Signing in…" : "Sign in"}
            </button>
          </form>

          <p className="login__sso muted">
            Using single sign-on? Contact your workspace administrator.
          </p>
        </div>

        <p className="login__legal muted">
          K8squad · Kubernetes-native agent squads
        </p>
      </section>
    </main>
  );
}
