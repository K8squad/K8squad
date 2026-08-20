import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import {
  ArtifactBrowser,
  classifyArtifactsStatus,
  decodeContent,
} from "@/components/ArtifactBrowser";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const handoff = {
  did: ["shipped the parser"],
  decisions: ["went recursive-descent"],
  next: ["add fuzz tests"],
  blockers: [],
  findings: "grammar was left-recursive",
  recommended_next: [{ title: "Fuzz the parser", body: "oss-fuzz harness" }],
  artifacts_for_downstream: [{ kind: "report", uri: "coord+audit://9", sha256: "ff" }],
};

const listing = {
  runId: "run-1",
  artifacts: [
    {
      id: "art-1",
      workItemId: "wi-1",
      runId: "run-1",
      kind: "handoff",
      uri: "coord+audit://7",
      sha256: "deadbeefdeadbeef",
      createdAt: "2026-08-20T10:00:00Z",
    },
  ],
  handoff,
};

function stubFetch(responses: { ok: boolean; status: number; json: () => Promise<unknown> }[]) {
  let i = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn().mockImplementation(() => {
      const r = responses[Math.min(i, responses.length - 1)];
      i++;
      return Promise.resolve(r);
    }),
  );
}

// Story 8.3 wiring: the screen FETCHES the BFF listing and renders handoff + rows; Inspect
// fetches the content envelope and decodes it.

describe("<ArtifactBrowser> — story 8.3 wiring (ISI-2900)", () => {
  it("fetches the listing and renders the structured handoff cards", async () => {
    stubFetch([{ ok: true, status: 200, json: () => Promise.resolve(listing) }]);
    render(<ArtifactBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("artifacts-ready")).toBeTruthy());
    expect(screen.getByTestId("handoff-card")).toBeTruthy();
    expect(screen.getByTestId("handoff-did").textContent).toContain("shipped the parser");
    expect(screen.getByTestId("handoff-findings").textContent).toContain("left-recursive");
    expect(screen.getByTestId("handoff-recommended").textContent).toContain("Fuzz the parser");
    expect(screen.getByTestId("handoff-downstream").textContent).toContain("coord+audit://9");
    expect(screen.getAllByTestId("artifact-row").length).toBe(1);
  });

  it("renders the empty-record card when the Run has no artifacts", async () => {
    stubFetch([
      { ok: true, status: 200, json: () => Promise.resolve({ runId: "run-1", artifacts: [] }) },
    ]);
    render(<ArtifactBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("artifacts-empty")).toBeTruthy());
    expect(screen.queryByTestId("handoff-card")).toBeNull();
  });

  it("inspects an artifact: fetches content by id and decodes the base64 envelope", async () => {
    const payload = JSON.stringify({ did: ["x"] });
    stubFetch([
      { ok: true, status: 200, json: () => Promise.resolve(listing) },
      {
        ok: true,
        status: 200,
        json: () =>
          Promise.resolve({
            artifact: listing.artifacts[0],
            // base64 of payload — what Go []byte marshalling emits.
            content: btoa(payload),
            size: payload.length,
            truncated: false,
          }),
      },
    ]);
    render(<ArtifactBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("artifact-row")).toBeTruthy());
    (screen.getByTestId("artifact-open") as HTMLButtonElement).click();
    await waitFor(() => expect(screen.getByTestId("artifact-viewer")).toBeTruthy());
    expect(screen.getByTestId("artifact-viewer").textContent).toContain('"did"');
    expect(screen.getByTestId("artifact-viewer").textContent).not.toContain("truncated");
  });

  it("renders the SAME 404 card for missing and denied Runs (existence-hiding)", async () => {
    stubFetch([{ ok: false, status: 404, json: () => Promise.resolve(null) }]);
    render(<ArtifactBrowser runId="run-x" />);
    await waitFor(() => expect(screen.getByTestId("artifacts-not-found")).toBeTruthy());
  });

  it("renders the not-wired card on the documented 501", async () => {
    stubFetch([{ ok: false, status: 501, json: () => Promise.resolve(null) }]);
    render(<ArtifactBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("artifacts-not-wired")).toBeTruthy());
  });

  it("renders the unauthenticated card on 401", async () => {
    stubFetch([{ ok: false, status: 401, json: () => Promise.resolve(null) }]);
    render(<ArtifactBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("artifacts-unauthenticated")).toBeTruthy());
  });
});

describe("classifyArtifactsStatus / decodeContent — unit contract", () => {
  it("maps relayed statuses to distinct honest states", () => {
    expect(classifyArtifactsStatus(401).kind).toBe("unauthenticated");
    expect(classifyArtifactsStatus(404).kind).toBe("not-found");
    expect(classifyArtifactsStatus(501).kind).toBe("not-wired");
    expect(classifyArtifactsStatus(503).kind).toBe("error");
  });

  it("decodes base64 content and degrades undecodable bytes visibly", () => {
    expect(decodeContent(btoa("hello"))).toBe("hello");
    expect(decodeContent("!!!not-base64!!!")).toBe("(undecodable content)");
  });
});
