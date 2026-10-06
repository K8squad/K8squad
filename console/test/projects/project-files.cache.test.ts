// test/projects/project-files.cache.test.ts — ISI-5485 (ADR-0025 D5a, child B):
// the client durable listing cache + coherence guard. Covers the cache-first
// synchronous hit, the M1/M2 cacheability rules, the M3 LRU cap, the M4/M5
// revalidate-only fetcher (keep-cache-on-failure + single in-flight), the L1
// in-place reconcile, and the C3 generation coherence verdict.

import { describe, it, expect, afterEach, vi } from "vitest";
import {
  getCachedListing,
  cacheListing,
  isCacheableListing,
  invalidateProjectListings,
  revalidateListing,
  reconcileListing,
  applyRevalidation,
  __resetListingCache,
  type FileListing,
  type FileEntry,
} from "@/lib/project-files";

afterEach(() => {
  __resetListingCache();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/** A cacheable (non-degraded, generation-stamped) listing. */
function listing(gen: string, entries: FileEntry[], over: Partial<FileListing> = {}): FileListing {
  return { path: "", entries, generation: gen, ...over };
}
const file = (name: string, size: number, path = name): FileEntry => ({ name, path, type: "file", size });
const dir = (name: string, path = name): FileEntry => ({ name, path, type: "dir" });

describe("project-files D5a cache — cacheability (M1/M2)", () => {
  it("caches a non-degraded generation-stamped listing and serves it fromCache", () => {
    expect(getCachedListing("p", "")).toBeNull(); // cold miss → cold path
    cacheListing("p", "", listing("g1", [file("a.ts", 1)]));
    const hit = getCachedListing("p", "");
    expect(hit).not.toBeNull();
    expect(hit!.fromCache).toBe(true);
    expect(typeof hit!.cachedAt).toBe("number");
    expect(hit!.generation).toBe("g1");
    expect((hit!.entries ?? []).map((e) => e.name)).toEqual(["a.ts"]);
  });

  it("M1: never caches a degraded / busy / no_browse_target listing", () => {
    cacheListing("p", "", listing("g1", [], { degraded: true, reason: "workspace_busy" }));
    expect(getCachedListing("p", "")).toBeNull();
    cacheListing("p", "", listing("g1", [], { reason: "no_browse_target" }));
    expect(getCachedListing("p", "")).toBeNull();
  });

  it("M2: an absent/empty generation is uncacheable (never keyed on \"\")", () => {
    cacheListing("p", "", { path: "", entries: [file("a.ts", 1)] }); // no generation
    expect(getCachedListing("p", "")).toBeNull();
    cacheListing("p", "", listing("", [file("a.ts", 1)])); // empty generation
    expect(getCachedListing("p", "")).toBeNull();
    expect(isCacheableListing(listing("", []))).toBe(false);
    expect(isCacheableListing(listing("g1", []))).toBe(true);
  });
});

describe("project-files D5a cache — LRU cap (M3)", () => {
  it("evicts the oldest entry once the cap is exceeded", () => {
    const CAP = 256;
    for (let i = 0; i <= CAP; i++) cacheListing("p", `d${i}`, listing("g1", [file("f", 1)]));
    // d0 was the oldest of CAP+1 inserts → evicted; the newest survives.
    expect(getCachedListing("p", "d0")).toBeNull();
    expect(getCachedListing("p", `d${CAP}`)).not.toBeNull();
  });

  it("a cache HIT refreshes recency so it is not the next eviction victim", () => {
    const CAP = 256;
    cacheListing("p", "keep", listing("g1", [file("f", 1)]));
    // Fill exactly to the cap (keep + d0..d254 = 256) so nothing is evicted yet.
    for (let i = 0; i < CAP - 1; i++) cacheListing("p", `d${i}`, listing("g1", [file("f", 1)]));
    // Touch `keep` so it becomes MRU — now d0 is the least-recently-used.
    getCachedListing("p", "keep");
    // One more insert tips over the cap → the victim is d0, NOT the touched keep.
    cacheListing("p", "dX", listing("g1", [file("f", 1)]));
    expect(getCachedListing("p", "keep")).not.toBeNull();
    expect(getCachedListing("p", "d0")).toBeNull();
  });
});

describe("project-files D5a cache — invalidation (C3)", () => {
  it("invalidateProjectListings drops every path for that project only", () => {
    cacheListing("p", "", listing("g1", [dir("src")]));
    cacheListing("p", "src", listing("g1", [file("m.ts", 1, "src/m.ts")]));
    cacheListing("q", "", listing("g1", [file("x", 1)]));
    invalidateProjectListings("p");
    expect(getCachedListing("p", "")).toBeNull();
    expect(getCachedListing("p", "src")).toBeNull();
    expect(getCachedListing("q", "")).not.toBeNull(); // other project untouched
  });
});

describe("project-files D5a — reconcileListing (L1)", () => {
  it("reports added / removed / restated and keeps server order", () => {
    const prev = [dir("src"), file("a.ts", 1), file("gone.ts", 9)];
    const next = [dir("src"), file("a.ts", 2), file("new.ts", 3)];
    const r = reconcileListing(prev, next);
    expect(r.entries.map((e) => e.name)).toEqual(["src", "a.ts", "new.ts"]);
    expect(r.added).toEqual(["new.ts"]);
    expect(r.removed).toEqual(["gone.ts"]);
    expect(r.restated).toEqual(["a.ts"]); // size 1 → 2
  });

  it("a name reused across a type flip counts as remove + add (not a restat)", () => {
    const prev = [file("thing", 1)];
    const next = [dir("thing")];
    const r = reconcileListing(prev, next);
    expect(r.added).toEqual(["thing"]);
    expect(r.removed).toEqual(["thing"]);
    expect(r.restated).toEqual([]);
  });
});

describe("project-files D5a — applyRevalidation coherence verdict", () => {
  it("stale: a null (failed/degraded) revalidate keeps the cache intact (AC3)", () => {
    cacheListing("p", "", listing("g1", [file("a.ts", 1)]));
    const rendered = getCachedListing("p", "")!;
    const out = applyRevalidation("p", "", rendered, null);
    expect(out.kind).toBe("stale");
    expect(getCachedListing("p", "")).not.toBeNull(); // untouched
  });

  it("updated: same generation reconciles in place and refreshes the cache", () => {
    cacheListing("p", "", listing("g1", [file("a.ts", 1), file("gone.ts", 1)]));
    const rendered = getCachedListing("p", "")!;
    const fresh = listing("g1", [file("a.ts", 2), file("new.ts", 1)]);
    const out = applyRevalidation("p", "", rendered, fresh);
    expect(out.kind).toBe("updated");
    if (out.kind === "updated") {
      expect(out.reconcile.added).toEqual(["new.ts"]);
      expect(out.reconcile.removed).toEqual(["gone.ts"]);
      expect(out.reconcile.restated).toEqual(["a.ts"]);
    }
    // Cache now holds the fresh bytes.
    expect((getCachedListing("p", "")!.entries ?? []).map((e) => e.name)).toEqual(["a.ts", "new.ts"]);
  });

  it("replaced: a generation change hard-invalidates the project and replaces wholesale (C3)", () => {
    cacheListing("p", "", listing("g1", [dir("src")]));
    cacheListing("p", "src", listing("g1", [file("old.ts", 1, "src/old.ts")]));
    const rendered = getCachedListing("p", "")!;
    const fresh = listing("g2", [dir("src")]); // new epoch
    const out = applyRevalidation("p", "", rendered, fresh);
    expect(out.kind).toBe("replaced");
    if (out.kind === "replaced") expect(out.listing.generation).toBe("g2");
    // The stale sibling path under g1 is gone; the replaced root is re-cached under g2.
    expect(getCachedListing("p", "src")).toBeNull();
    expect(getCachedListing("p", "")!.generation).toBe("g2");
  });
});

describe("project-files D5a — revalidateListing fetcher (M4/M5)", () => {
  const okJson = (body: unknown): Response =>
    ({ ok: true, status: 200, json: () => Promise.resolve(body) }) as unknown as Response;
  const status = (code: number): Response =>
    ({ ok: code >= 200 && code < 300, status: code, json: () => Promise.resolve(null) }) as unknown as Response;

  it("returns fresh bytes on a 200 cacheable listing", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(okJson({ path: "", entries: [{ name: "a.ts", type: "file", size: 1 }], generation: "g1" }))));
    const fresh = await revalidateListing("p", "");
    expect(fresh).not.toBeNull();
    expect(fresh!.generation).toBe("g1");
    expect((fresh!.entries ?? [])[0].path).toBe("a.ts"); // normalizeListing stamped the path
  });

  it("M4: a 202/503 (foreground state) resolves null — never escalates, cache kept", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(status(503))));
    expect(await revalidateListing("p", "")).toBeNull();
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(status(202))));
    expect(await revalidateListing("p", "")).toBeNull();
  });

  it("M4: a 200-degraded (project went busy) resolves null — keep cache, show stale", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(okJson({ path: "", entries: [], degraded: true, reason: "workspace_busy", generation: "g1" }))));
    expect(await revalidateListing("p", "")).toBeNull();
  });

  it("M5: concurrent revalidates for the same key share one in-flight request", async () => {
    const spy = vi.fn(() => Promise.resolve(okJson({ path: "", entries: [], generation: "g1" })));
    vi.stubGlobal("fetch", spy as unknown as typeof fetch);
    const [a, b] = await Promise.all([revalidateListing("p", ""), revalidateListing("p", "")]);
    expect(spy).toHaveBeenCalledTimes(1);
    expect(a!.generation).toBe("g1");
    expect(b!.generation).toBe("g1");
  });
});
