"use client";

// components/inbox/InboxList.tsx — ISI-5535 (E1 of ISI-5531): the "Needs Human Decision" inbox
// list component. Polls GET /api/inbox at 5s (matches ISI-5528 cadence), renders a decision-type
// chip row per item, live-run marker (ISI-5528), unread dot, relative time, click → ticket detail.
//
// Mocks: ux/isi-5531-inbox/02-inbox-list. Decision chip hues: review→violet, proposal→green,
// choose_one/choose_many→blue, free_form→amber.

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import {
  decisionChipHue,
  fetchInbox,
  inboxItemHref,
  inboxMarkSeen,
  type InboxItem,
  type InboxResponse,
} from "@/lib/inbox/api";

const POLL_MS = 5_000;

function relativeTime(iso: string): string {
  const diff = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (diff < 60) return `${diff}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

function DecisionChip({ type }: { type: string }) {
  const hue = decisionChipHue(type);
  const label =
    type === "review"     ? "Review"      :
    type === "proposal"   ? "Proposal"    :
    type === "choose_one" ? "Choose one"  :
    type === "choose_many"? "Choose many" :
    type === "free_form"  ? "Free form"   : type;
  return (
    <span className={`inbox__chip inbox__chip--${hue}`} aria-label={`Decision type: ${label}`}>
      {label}
    </span>
  );
}

function InboxRow({ item }: { item: InboxItem }) {
  const href = inboxItemHref(item);
  const timeLabel = item.lastRunAt ? relativeTime(item.lastRunAt) : "";
  return (
    <Link href={href} className={`inbox__row${item.unread ? " inbox__row--unread" : ""}`}>
      <span className="inbox__row-main">
        <span className="inbox__row-title">
          {item.unread && <span className="inbox__unread-dot" aria-label="Unread" />}
          {item.title}
          {item.live && (
            <span className="inbox__live-badge rail__livebadge" aria-label="Live run" />
          )}
        </span>
        <span className="inbox__row-meta">
          <DecisionChip type={item.decisionType} />
          {item.raisedByAgent && (
            <span className="inbox__raised-by">by {item.raisedByAgent}</span>
          )}
          {timeLabel && <span className="inbox__time">{timeLabel}</span>}
        </span>
      </span>
    </Link>
  );
}

export function InboxList() {
  const [data, setData] = useState<InboxResponse | null>(null);
  const [error, setError] = useState(false);
  // Mark-seen fires ONCE per mount (ADR-0026 §6): opening the Inbox clears the unread dots + nav
  // badge for whatever is unread at that moment. Deliberately not per-poll — a new run that
  // re-surfaces an item as unread while the page is open stays unread until the next visit, so the
  // "needs me again" signal survives. `markedSeen` guards against the 5s poll re-firing it.
  const markedSeen = useRef(false);

  useEffect(() => {
    let cancelled = false;
    const poll = () => {
      if (document.hidden) return;
      fetchInbox()
        .then((d) => {
          if (cancelled) return;
          setData(d);
          setError(false);
          if (!markedSeen.current) {
            markedSeen.current = true;
            const unreadKeys = d.items.filter((i) => i.unread).map((i) => i.key);
            void inboxMarkSeen(unreadKeys);
          }
        })
        .catch(() => { if (!cancelled) setError(true); });
    };
    poll();
    const id = setInterval(poll, POLL_MS);
    return () => { cancelled = true; clearInterval(id); };
  }, []);

  if (error && !data) {
    return <p className="inbox__error">Could not load inbox — retrying…</p>;
  }

  if (!data) {
    return <p className="inbox__loading">Loading…</p>;
  }

  if (data.items.length === 0) {
    return (
      <div className="inbox__empty">
        <p>All caught up — nothing needs a decision right now.</p>
      </div>
    );
  }

  return (
    <ul className="inbox__list" role="list">
      {data.items.map((item) => (
        <li key={item.key} role="listitem">
          <InboxRow item={item} />
        </li>
      ))}
    </ul>
  );
}
