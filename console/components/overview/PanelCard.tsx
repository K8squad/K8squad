// components/overview/PanelCard.tsx — a titled surface card with an optional header action link
// and first-class loading / empty / error slots (DESIGN-SPEC-ISI-4505 §5.4, degrade-don't-blank
// carried from ISI-4229). Every overview panel is a PanelCard, so no panel can render blank on a
// slow/failed read: it shows a skeleton, an honest empty placeholder, or a retryable error.

import type { ReactNode } from "react";
import "./overview.css";

export type PanelState = "ready" | "loading" | "empty" | "error";

export function PanelCard({
  title,
  action,
  state = "ready",
  emptyLabel = "Nothing here yet.",
  errorLabel = "Couldn't load this panel.",
  onRetry,
  children,
}: {
  title: ReactNode;
  /** Optional header action link (e.g. "View all →"). */
  action?: { label: string; href: string };
  state?: PanelState;
  emptyLabel?: ReactNode;
  errorLabel?: ReactNode;
  /** Retry handler for the error state; when omitted no retry button is shown. */
  onRetry?: () => void;
  children?: ReactNode;
}) {
  return (
    <section className="ov-panel">
      <header className="ov-panel__head">
        <h3 className="ov-panel__title">{title}</h3>
        {action && (
          <a className="ov-panel__action" href={action.href}>
            {action.label}
          </a>
        )}
      </header>
      <div className="ov-panel__body">
        {state === "loading" && (
          <div aria-hidden="true">
            <div className="ov-skel ov-panel__skel-row" />
            <div className="ov-skel ov-panel__skel-row" />
            <div className="ov-skel ov-panel__skel-row" />
          </div>
        )}
        {state === "empty" && (
          <div className="ov-panel__state" role="status">
            <span>{emptyLabel}</span>
          </div>
        )}
        {state === "error" && (
          <div className="ov-panel__state" role="alert">
            <span>{errorLabel}</span>
            {onRetry && (
              <button type="button" className="ov-panel__retry" onClick={onRetry}>
                Retry
              </button>
            )}
          </div>
        )}
        {state === "ready" && children}
      </div>
    </section>
  );
}
