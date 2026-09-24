// components/runs/RunCreatedTickets.tsx — the honest "sub-tickets this run
// created" card (ADR-0024a S6, ISI-4872). It reports the tickets a decomposition
// run authored via the work_item_create MCP tool, sourced from the run-scoped
// `work_item_created` audit rows (backend RunDetailResponse.createdItems), NOT
// the agent's free-text completion prose and NOT a workspace file scan.
//
// Honesty contract (ADR-0013, mirrors the localRunBadge null-vs-honest-absence
// discipline from ISI-4760):
//   - items undefined/null  → render nothing (the read model did not carry it).
//   - items === []          → render an honest "0 sub-tickets created" — never a
//                             success claim, never rounding a file up to a ticket.
//   - items.length === N     → render exactly those N tickets (id + title), as
//                             links when a href builder is supplied, else text.
// A partial-create run lists only the tickets it actually created; the count is
// always the true created set.

import type { CreatedWorkItemWire } from "@/lib/runs";

export function RunCreatedTickets({
  items,
  hrefFor,
}: {
  items?: CreatedWorkItemWire[] | null;
  /** Optional link builder: given a created ticket id, the ticket-detail href.
   * Omitted when the caller cannot build a reliable route — the ticket then
   * renders as honest text (id + title) rather than a guessed, possibly-broken
   * link. */
  hrefFor?: (id: string) => string;
}) {
  // Not carried by the read model → not applicable to this run. Render nothing
  // rather than an empty or fabricated card.
  if (items == null) return null;

  const count = items.length;
  return (
    <section className="card run-created" data-testid="run-created-tickets">
      <h2 className="run-created__title">Sub-tickets created</h2>
      {count === 0 ? (
        <p className="run-created__empty muted" data-testid="run-created-empty">
          0 sub-tickets created
        </p>
      ) : (
        <>
          <p className="run-created__count" data-testid="run-created-count">
            Created {count} sub-ticket{count === 1 ? "" : "s"} on the board
          </p>
          <ul className="run-created__list">
            {items.map((it) => {
              const label = it.title || "(untitled)";
              const href = hrefFor?.(it.id);
              return (
                <li key={it.id} className="run-created__item" data-testid="run-created-item">
                  {href ? (
                    <a className="run-created__link" href={href}>
                      {label}
                    </a>
                  ) : (
                    <span className="run-created__label">{label}</span>
                  )}
                  <code className="run-created__id">{it.id}</code>
                </li>
              );
            })}
          </ul>
        </>
      )}
    </section>
  );
}
