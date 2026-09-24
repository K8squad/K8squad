"use client";

// components/compose/RuntimeAdapterStep.tsx — the Frame-4 per-runtime credential
// branch that fronts ModelSelector (ISI-4890 S1 AC5; epic ISI-4822, board LOCK OQ5).
//
// Model configuration BRANCHES by runtime adapter (Henrik, 2026-09-23): the model
// id is provider-agnostic, but HOW credentials reach the provider differs. This
// step renders an adapter selector (claude / codex / opencode), tinted per adapter
// with the LOCKED status hues (no new tokens: claude → --accent, codex →
// --status-paused, opencode → --status-running), plus the adapter's credential
// affordance, then renders `children` (the unchanged ModelSelector, so the persisted
// shape is always the same model triple).
//
//   - claude:   token-only in v1 (OQ5) — a DISABLED "Sign in with Claude" (OAuth)
//               CTA marked *coming soon* beside the working API-token path. Curated
//               Anthropic ids flow through ModelSelector unchanged. Do NOT block the
//               model UI on OAuth.
//   - codex:    API token; curated ids. No live model listing.
//   - opencode: provider + token/endpoint. v1 degrades to manual model entry (the
//               fail-open state); the live fetch-then-pick (ProviderModelPicker +
//               GET/POST /api/modelendpoints/models) slots into `opencodeSlot` in a
//               fast-follow (AC6). `adapter` is UI-only — never persisted.
//
// SHARED WITH S2 (ISI-4891): the API is deliberately minimal — { adapter,
// onAdapterChange, children, opencodeSlot? } — so the Role-tier surface imports it
// with no rework. S1 is the first consumer.

import type { CSSProperties, ReactNode } from "react";
import { RUNTIME_ADAPTERS, RUNTIME_ADAPTER_LABELS, type RuntimeAdapter } from "@/lib/compose";

/** Locked status hues per adapter (zero new tokens). */
const ADAPTER_TINT: Record<RuntimeAdapter, string> = {
  claude: "var(--accent)",
  codex: "var(--status-paused)",
  opencode: "var(--status-running)",
};

export function RuntimeAdapterStep({
  adapter,
  onAdapterChange,
  children,
  opencodeSlot,
}: {
  adapter: RuntimeAdapter;
  onAdapterChange: (a: RuntimeAdapter) => void;
  /** The model picker the adapter fronts (ModelSelector) — rendered for every adapter. */
  children: ReactNode;
  /**
   * Optional live fetch-then-pick region for the opencode branch (ProviderModelPicker,
   * ISI-4890 AC6 fast-follow). Absent ⇒ opencode degrades to manual entry via `children`
   * — the fail-open state, so the flow is never blocked.
   */
  opencodeSlot?: ReactNode;
}) {
  const tint = ADAPTER_TINT[adapter];
  return (
    <div className="runtime-adapter" style={{ "--adapter-accent": tint } as CSSProperties}>
      <div className="runtime-adapter__tabs" role="radiogroup" aria-label="Runtime adapter">
        {RUNTIME_ADAPTERS.map((a) => (
          <button
            key={a}
            type="button"
            role="radio"
            aria-checked={a === adapter}
            data-adapter={a}
            className={`runtime-adapter__tab${a === adapter ? " is-active" : ""}`}
            onClick={() => onAdapterChange(a)}
          >
            {RUNTIME_ADAPTER_LABELS[a]}
          </button>
        ))}
      </div>

      <div className="runtime-adapter__cred" data-adapter={adapter} role="group" aria-label={`${RUNTIME_ADAPTER_LABELS[adapter]} credentials`}>
        {adapter === "claude" && (
          <>
            <button
              type="button"
              className="btn"
              disabled
              aria-disabled="true"
              data-testid="claude-oauth-cta"
            >
              Sign in with Claude <span className="chip chip--soon">coming soon</span>
            </button>
            <p className="muted">
              Claude uses your API token in v1 — pick a curated Anthropic model below. One-click
              &ldquo;Sign in with Claude&rdquo; (OAuth) is coming soon.
            </p>
          </>
        )}
        {adapter === "codex" && (
          <p className="muted">
            Codex uses your API token — pick a curated model below. Live model listing isn&apos;t
            available for this adapter.
          </p>
        )}
        {adapter === "opencode" &&
          (opencodeSlot ?? (
            <p className="muted" data-testid="opencode-manual-note">
              OpenCode reaches your provider through a Bring-your-own endpoint (URL + token) — set the
              endpoint Secret below and enter the model id directly. Fetching the provider&apos;s model
              list is coming soon; until then, manual entry always works.
            </p>
          ))}
      </div>

      {children}
    </div>
  );
}
