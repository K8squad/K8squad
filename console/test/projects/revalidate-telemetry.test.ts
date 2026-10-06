// test/projects/revalidate-telemetry.test.ts — ISI-5500 [S5a-C-fu2]: the D5a
// file-revalidate beacon emitter + its wiring into the three project-files.ts
// seams (serve / revalidate / invalidate). Locks the ISI-5486 §3 field contract,
// the §8 PII/cardinality firewall, the always-send-failures sampling rule, and
// the hard "never throws into render" guarantee.

import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  emitServe,
  emitRevalidate,
  emitInvalidate,
  REVALIDATE_TELEMETRY_URL,
  REVALIDATE_SAMPLE_N,
} from "@/lib/revalidate-telemetry";
import {
  getCachedListing,
  cacheListing,
  invalidateProjectListings,
  applyRevalidation,
  __resetListingCache,
  type FileListing,
} from "@/lib/project-files";

// Capture every navigator.sendBeacon(url, blob) call. Node 18+ gives us Blob.
type Beacon = { url: string; blob: Blob };
let beacons: Beacon[];
const sendBeacon = vi.fn((url: string, blob: Blob) => {
  beacons.push({ url, blob });
  return true;
});

async function bodyOf(i: number): Promise<Record<string, unknown>> {
  return JSON.parse(await beacons[i].blob.text());
}

// The test environment's Blob does not implement .text(); stub a minimal Blob
// that records the stringified parts so we can read the beacon body back.
class TextBlob {
  private _text: string;
  readonly type: string;
  constructor(parts: string[], opts?: { type?: string }) {
    this._text = parts.join("");
    this.type = opts?.type ?? "";
  }
  async text(): Promise<string> {
    return this._text;
  }
}

beforeEach(() => {
  beacons = [];
  sendBeacon.mockClear();
  vi.stubGlobal("navigator", { sendBeacon });
  vi.stubGlobal("Blob", TextBlob);
});

afterEach(() => {
  __resetListingCache();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const listing = (gen: string, over: Partial<FileListing> = {}): FileListing => ({
  path: "",
  entries: [{ name: "a.ts", path: "a.ts", type: "file", size: 1 }],
  generation: gen,
  ...over,
});

describe("revalidate-telemetry — §3 field contract", () => {
  it("serve beacon carries the exact §3 fields and posts to the local sink", async () => {
    emitServe({ projectId: "p1", path: "src/lib/x", fromCache: true, revisit: true, generation: "a1b2c3deadbeef" });
    expect(sendBeacon).toHaveBeenCalledTimes(1);
    expect(beacons[0].url).toBe(REVALIDATE_TELEMETRY_URL);
    const b = await bodyOf(0);
    expect(b.tag).toBe("file-revalidate");
    expect(b.event).toBe("serve");
    expect(b.fromCache).toBe(true);
    expect(b.revisit).toBe(true);
    expect(b.projectId).toBe("p1");
    expect(b.pathDepth).toBe(3); // "src/lib/x" → 3, NOT the string
    expect(b.genHashPrefix).toBe("a1b2c3"); // first 6 hex only
    expect(b.sampleRate).toBe(REVALIDATE_SAMPLE_N);
  });

  it("§8 PII firewall: no path string, no file names, no raw generation in the body", async () => {
    emitServe({ projectId: "p1", path: "secrets/prod/.env", fromCache: false, revisit: false, generation: "deadbeefcafe" });
    const b = await bodyOf(0);
    const serialized = JSON.stringify(b);
    expect(b).not.toHaveProperty("path");
    expect(serialized).not.toContain("secrets/prod/.env");
    expect(serialized).not.toContain(".env");
    expect(serialized).not.toContain("deadbeefcafe"); // only the 6-hex prefix
    expect(b.genHashPrefix).toBe("deadbe");
  });

  it("revalidate success carries outcome/latency/generationChanged", async () => {
    emitRevalidate({ projectId: "p1", path: "", outcome: "success", latencyMs: 142, generationChanged: false });
    const b = await bodyOf(0);
    expect(b.event).toBe("revalidate");
    expect(b.outcome).toBe("success");
    expect(b.latencyMs).toBe(142);
    expect(b.generationChanged).toBe(false);
  });

  it("revalidate failed is ALWAYS sent and carries a required reason", async () => {
    emitRevalidate({ projectId: "p1", path: "", outcome: "failed", reason: "snapshot_unavailable", latencyMs: 9, generationChanged: false });
    expect(sendBeacon).toHaveBeenCalledTimes(1);
    const b = await bodyOf(0);
    expect(b.outcome).toBe("failed");
    expect(b.reason).toBe("snapshot_unavailable");
  });

  it("invalidate is ALWAYS sent with pathsEvicted and sampleRate=1", async () => {
    emitInvalidate({ projectId: "p1", reason: "generation_changed", pathsEvicted: 4 });
    const b = await bodyOf(0);
    expect(b.event).toBe("invalidate");
    expect(b.reason).toBe("generation_changed");
    expect(b.pathsEvicted).toBe(4);
    expect(b.sampleRate).toBe(1);
  });

  it("never throws when sendBeacon is unavailable (SSR / old browser)", () => {
    vi.stubGlobal("navigator", {});
    expect(() => emitServe({ projectId: "p", path: "", fromCache: false, revisit: false })).not.toThrow();
    expect(() => emitInvalidate({ projectId: "p", reason: "busy_flip", pathsEvicted: 0 })).not.toThrow();
  });

  it("never throws when sendBeacon itself throws", () => {
    vi.stubGlobal("navigator", { sendBeacon: () => { throw new Error("boom"); } });
    expect(() => emitRevalidate({ projectId: "p", path: "", outcome: "failed", reason: "network", latencyMs: 0, generationChanged: false })).not.toThrow();
  });
});

describe("revalidate-telemetry — wired into the project-files seams", () => {
  it("getCachedListing hit emits serve{fromCache:true, revisit:true}", async () => {
    cacheListing("p1", "", listing("gen123456"));
    const hit = getCachedListing("p1", "");
    expect(hit).not.toBeNull();
    const serves = beacons.filter((_, i) => true);
    expect(serves.length).toBe(1);
    const b = await bodyOf(0);
    expect(b.event).toBe("serve");
    expect(b.fromCache).toBe(true);
    expect(b.revisit).toBe(true);
    expect(b.genHashPrefix).toBe("gen123");
  });

  it("applyRevalidation success (same generation) emits one revalidate{success}", async () => {
    const rendered = listing("g1");
    const fresh = listing("g1");
    const outcome = applyRevalidation("p1", "", rendered, fresh);
    expect(outcome.kind).toBe("updated");
    const reval = beacons.length - 1;
    const b = await bodyOf(reval);
    expect(b.event).toBe("revalidate");
    expect(b.outcome).toBe("success");
    expect(b.generationChanged).toBe(false);
  });

  it("applyRevalidation across a generation boundary emits BOTH invalidate and revalidate{generationChanged:true}", async () => {
    cacheListing("p1", "", listing("g1"));
    cacheListing("p1", "sub", listing("g1", { path: "sub" }));
    beacons = []; // drop the write-through serve noise
    const outcome = applyRevalidation("p1", "", listing("g1"), listing("g2"));
    expect(outcome.kind).toBe("replaced");
    const events = await Promise.all(beacons.map((_, i) => bodyOf(i)));
    const invalidate = events.find((e) => e.event === "invalidate");
    const revalidate = events.find((e) => e.event === "revalidate");
    expect(invalidate).toBeTruthy();
    expect(invalidate!.pathsEvicted).toBe(2); // both cached paths evicted
    expect(revalidate!.outcome).toBe("success");
    expect(revalidate!.generationChanged).toBe(true);
  });

  it("invalidateProjectListings emits invalidate with the exact evicted count", async () => {
    cacheListing("p1", "", listing("g1"));
    cacheListing("p1", "a", listing("g1", { path: "a" }));
    cacheListing("p2", "", listing("g1")); // other project — not evicted
    beacons = [];
    invalidateProjectListings("p1");
    const b = await bodyOf(0);
    expect(b.event).toBe("invalidate");
    expect(b.projectId).toBe("p1");
    expect(b.pathsEvicted).toBe(2);
  });
});
