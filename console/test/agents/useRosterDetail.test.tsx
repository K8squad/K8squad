import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import { useRosterDetail } from "@/components/agents/roster/useRosterDetail";

// ISI-5416 regression pins: the hook's effect originally attached ONLY a .catch — a successful
// 200 fetch resolved into nothing, the state stayed {kind:"loading"} forever, and every roster
// inline editor (Model / Skills panels, Frame 02/03) was permanently stuck on "Loading…".
// These tests pin the full DetailLoad lifecycle: loading → ok on resolve, loading → error on
// reject, abort suppressing late setState, and the nonce-keyed reload.

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("useRosterDetail", () => {
  it("transitions to {kind:'ok'} when the load resolves (ISI-5416 — the missing success arm)", async () => {
    const d = deferred<{ name: string }>();
    const load = vi.fn(() => d.promise);
    const { result } = renderHook(() => useRosterDetail<{ name: string }>(load, 0));

    expect(result.current.kind).toBe("loading");
    await act(async () => {
      d.resolve({ name: "architect" });
    });
    expect(result.current).toEqual({ kind: "ok", detail: { name: "architect" } });
  });

  it("transitions to {kind:'error'} when the load rejects", async () => {
    const d = deferred<string>();
    const { result } = renderHook(() => useRosterDetail<string>(() => d.promise, 0));

    await act(async () => {
      d.reject(new Error("Could not load."));
    });
    expect(result.current).toEqual({ kind: "error", message: "Could not load." });
  });

  it("keeps a rejected non-Error message honest", async () => {
    const d = deferred<string>();
    const { result } = renderHook(() => useRosterDetail<string>(() => d.promise, 0));

    await act(async () => {
      d.reject("boom");
    });
    expect(result.current).toEqual({ kind: "error", message: "Could not load." });
  });

  it("does not setState after abort (a late resolve on an abandoned fetch is dropped)", async () => {
    const d = deferred<string>();
    const load = vi.fn((_signal: AbortSignal) => d.promise);
    const { result, unmount } = renderHook(() => useRosterDetail<string>(load, 0));

    unmount(); // runs the effect cleanup → controller.abort()
    await act(async () => {
      d.resolve("late");
    });
    // No update after unmount, and no act() warning: the abort guard returned before setState.
    expect(result.current.kind).toBe("loading");
  });

  it("reloads when the nonce bumps (the post-save re-read path)", async () => {
    const d1 = deferred<string>();
    const d2 = deferred<string>();
    const loads = [d1.promise, d2.promise];
    const load = vi.fn(() => loads.shift() ?? Promise.resolve("never"));
    const { result, rerender } = renderHook(({ nonce }) => useRosterDetail<string>(load, nonce), {
      initialProps: { nonce: 0 },
    });

    await act(async () => {
      d1.resolve("first");
    });
    expect(result.current).toEqual({ kind: "ok", detail: "first" });

    await act(async () => {
      rerender({ nonce: 1 });
    });
    expect(result.current.kind).toBe("loading");
    await act(async () => {
      d2.resolve("second");
    });
    expect(result.current).toEqual({ kind: "ok", detail: "second" });
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("passes the effect's AbortSignal to the loader", () => {
    const load = vi.fn(() => new Promise<string>(() => {}));
    renderHook(() => useRosterDetail<string>(load, 0));
    expect(load).toHaveBeenCalledWith(expect.any(AbortSignal));
  });
});
