import React from "react";

export interface EmptyStateProps {
  title: string;
  why: string;
  ctaLabel?: string;
  onCta?: () => void;
  dependencyNudge?: React.ReactNode;
  /** Stable observability id; defaults to "empty-state". */
  testId?: string;
}

export function EmptyState({ title, why, ctaLabel, onCta, dependencyNudge, testId }: EmptyStateProps) {
  return (
    <div className="card empty-state" data-testid={testId ?? "empty-state"}>
      <h2 className="empty-state__title">{title}</h2>
      <p className="muted empty-state__why">{why}</p>
      {dependencyNudge && (
        <div className="empty-state__nudge muted">{dependencyNudge}</div>
      )}
      {ctaLabel && onCta && (
        <button className="btn btn--primary empty-state__cta" onClick={onCta}>
          {ctaLabel}
        </button>
      )}
    </div>
  );
}
