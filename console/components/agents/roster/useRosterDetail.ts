"use client";

// components/agents/roster/useRosterDetail.ts — the detail-read hook shared by the roster's
// inline editors (ISI-5362 / S5). Loads (and reloads, via `nonce`) one authoring-spec detail
// read from lib/agents/rosterEdit with abort-safe lifecycle; the editors remount per node
// (the parent keys them by node identity), so a fetch is keyed on `nonce` alone.

import { useEffect, useState } from "react";

export type DetailLoad<T> =
  | { kind: "loading" }
  | { kind: "error"; message: string }
  | { kind: "ok"; detail: T };

export function useRosterDetail<T>(
  load: (signal: AbortSignal) => Promise<T>,
  nonce: number,
): DetailLoad<T> {
  const [state, setState] = useState<DetailLoad<T>>({ kind: "loading" });
  useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    load(controller.signal).catch((e: unknown) => {
      if (controller.signal.aborted) return;
      setState({ kind: "error", message: e instanceof Error ? e.message : "Could not load." });
    });
    return () => controller.abort();
    // `load` is a fresh per-render closure over name/team; the fetch is intentionally keyed on
    // `nonce` alone — a node switch remounts the panel (the parent keys it by node identity).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nonce]);
  return state;
}
