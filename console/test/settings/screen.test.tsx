import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent, act } from "@testing-library/react";
import { ProjectSettingsScreen } from "@/components/settings/ProjectSettingsScreen";
import type { ProjectSettings } from "@/lib/projectSettings";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

function settings(over: Partial<ProjectSettings> = {}): ProjectSettings {
  return {
    project: { name: "proj-a", namespace: "squad-a" },
    repo: { url: "https://github.com/org/repo", ref: "main", provider: "github", syncEnabled: false, pollIntervalSeconds: 0, reflectOutbound: false },
    auth: { connected: true, credentialSecretRefName: "scm-pat", lastTest: "passed" },
    canEdit: true,
    ...over,
  };
}

/** Route the screen's fetches by URL + method to canned responses. */
function stubFetch(routes: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => Promise.resolve(routes(String(input), init))));
}

describe("<ProjectSettingsScreen> — ISI-4000 S2 ACs", () => {
  it("AC1: renders repo url/ref/provider + honest tri-state badge from the S1 projection (ISI-4830: editable card pre-fills the inputs)", async () => {
    stubFetch((url) => (url.includes("/settings") ? jsonResponse(200, settings()) : jsonResponse(404, {})));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    expect((screen.getByTestId("repo-url-input") as HTMLInputElement).value).toBe("https://github.com/org/repo");
    expect((screen.getByTestId("repo-ref-input") as HTMLInputElement).value).toBe("main");
    expect(screen.getByTestId("repo-provider").textContent).toBe("github");
    expect(screen.getByTestId("cred-status-badge").textContent).toBe("Connected — test passed");
  });

  it("AC1: an empty ref shows a 'default branch' placeholder, never a blank claim of a ref", async () => {
    stubFetch(() => jsonResponse(200, settings({ repo: { url: "https://github.com/org/repo", ref: "", provider: "github", syncEnabled: false, pollIntervalSeconds: 0, reflectOutbound: false } })));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    const refInput = screen.getByTestId("repo-ref-input") as HTMLInputElement;
    expect(refInput.value).toBe("");
    expect(refInput.placeholder).toBe("default branch");
  });

  it("AC1/AC6: a read-only viewer sees repo facts as text (url/ref/provider), never inputs", async () => {
    stubFetch(() => jsonResponse(200, settings({ canEdit: false })));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    expect(screen.getByTestId("repo-url").textContent).toBe("https://github.com/org/repo");
    expect(screen.getByTestId("repo-ref").textContent).toBe("main");
    expect(screen.getByTestId("repo-provider").textContent).toBe("github");
  });

  it("AC6: canEdit:false renders read-only — no inputs, no save/PAT affordances", async () => {
    stubFetch(() => jsonResponse(200, settings({ canEdit: false })));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    expect(screen.getByTestId("settings-readonly-note")).toBeTruthy();
    expect(screen.queryByTestId("repo-url-input")).toBeNull();
    expect(screen.queryByTestId("pat-value-input")).toBeNull();
    expect(screen.queryByTestId("repo-save")).toBeNull();
  });

  it("AC7: 501 renders the 'not available yet' frame (never fabricated values)", async () => {
    stubFetch(() => jsonResponse(501, {}));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-not-wired"));
  });

  it("AC7: a 5xx renders the retryable error frame", async () => {
    stubFetch(() => jsonResponse(503, {}));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-error"));
  });

  it("AC7: a project with no repo renders the 'no repo connected' state", async () => {
    stubFetch(() => jsonResponse(200, settings({ repo: { url: "", ref: "", provider: "github", syncEnabled: false, pollIntervalSeconds: 0, reflectOutbound: false }, auth: { connected: false, credentialSecretRefName: "", lastTest: "untested" } })));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    expect(screen.getByTestId("settings-no-repo")).toBeTruthy();
    expect(screen.getByTestId("cred-status-badge").textContent).toBe("Not connected");
  });

  it("AC5: test-connection renders {ok:false, detail} VERBATIM (never rewritten to success)", async () => {
    stubFetch((url, init) => {
      if (url.includes("/repo-auth/test") && init?.method === "POST") {
        return jsonResponse(200, { ok: false, detail: "Rejected HTTP 401" });
      }
      return jsonResponse(200, settings());
    });
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    fireEvent.click(screen.getByTestId("test-connection"));
    await waitFor(() => screen.getByTestId("test-result"));
    const result = screen.getByTestId("test-result");
    expect(result.textContent).toBe("Rejected HTTP 401");
    expect(result.className).toContain("settings__msg--bad");
  });

  it("AC4: saving a PAT clears the token field and never echoes it back", async () => {
    const calls: Array<{ url: string; method?: string; body?: string }> = [];
    stubFetch((url, init) => {
      calls.push({ url, method: init?.method, body: init?.body as string | undefined });
      if (url.includes("/api/credentials") && init?.method === "POST") return jsonResponse(201, { secretRef: "secret://squad-a/fresh-pat" });
      if (url.includes("/api/squad/projects/")) return jsonResponse(200, { name: "proj-a", repo: { url: "https://github.com/org/repo", ref: "main" }, goals: ["g1"] });
      if (url.includes("/api/compose/projects/") && init?.method === "PUT") return jsonResponse(200, { operation: "updated" });
      return jsonResponse(200, settings());
    });
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    fireEvent.change(screen.getByTestId("pat-name-input"), { target: { value: "fresh-pat" } });
    fireEvent.change(screen.getByTestId("pat-value-input"), { target: { value: "ghp_secrettoken" } });
    fireEvent.submit(screen.getByTestId("settings-pat-form"));
    await waitFor(() => screen.getByTestId("pat-msg"));
    // The token input is cleared after submit — never held in the DOM.
    expect((screen.getByTestId("pat-value-input") as HTMLInputElement).value).toBe("");
    // The credential POST carried the token; the compose PUT body did NOT.
    const put = calls.find((c) => c.url.includes("/api/compose/projects/") && c.method === "PUT");
    expect(put?.body).toBeTruthy();
    expect(put!.body).not.toContain("ghp_secrettoken");
    // The PUT preserved the project's goals (full-spec round-trip).
    expect(put!.body).toContain("g1");
  });

  // ── S5 (ISI-4843): SyncCard — the net-new WRITE. ──────────────────────────
  it("S5: enabling sync + editing the poll interval writes a full-spec PUT that preserves goals + the sync passthrough", async () => {
    const calls: Array<{ url: string; method?: string; body?: string }> = [];
    stubFetch((url, init) => {
      calls.push({ url, method: init?.method, body: init?.body as string | undefined });
      if (url.includes("/api/squad/projects/")) {
        return jsonResponse(200, {
          name: "proj-a",
          repo: { url: "https://github.com/org/repo", ref: "main", sync: { provider: "github", pollIntervalSeconds: 300, webhookSecretRef: { name: "hook" } } },
          goals: ["g1"],
        });
      }
      if (url.includes("/api/compose/projects/") && init?.method === "PUT") return jsonResponse(200, { operation: "updated" });
      return jsonResponse(200, settings({ repo: { url: "https://github.com/org/repo", ref: "main", provider: "github", syncEnabled: false, pollIntervalSeconds: 0, reflectOutbound: false } }));
    });
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    fireEvent.click(screen.getByTestId("sync-enabled-input"));
    fireEvent.change(screen.getByTestId("sync-poll-input"), { target: { value: "600" } });
    fireEvent.submit(screen.getByTestId("settings-sync-form"));
    await waitFor(() => screen.getByTestId("sync-msg"));
    const put = calls.find((c) => c.url.includes("/api/compose/projects/") && c.method === "PUT");
    expect(put?.body).toBeTruthy();
    const body = JSON.parse(put!.body as string);
    // The whole sync sub-spec round-trips: edited poll, preserved webhookSecretRef + provider.
    expect(body.repo.sync).toEqual({ provider: "github", pollIntervalSeconds: 600, reflectOutbound: false, webhookSecretRef: { name: "hook" } });
    // Goals preserved (full-spec round-trip, no silent wipe).
    expect(body.goals).toEqual(["g1"]);
  });

  it("S5: a sub-minimum poll interval is rejected client-side before any write", async () => {
    const calls: Array<{ url: string; method?: string }> = [];
    stubFetch((url, init) => {
      calls.push({ url, method: init?.method });
      return jsonResponse(200, settings({ repo: { url: "https://github.com/org/repo", ref: "main", provider: "github", syncEnabled: true, pollIntervalSeconds: 300, reflectOutbound: false } }));
    });
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    fireEvent.change(screen.getByTestId("sync-poll-input"), { target: { value: "30" } });
    fireEvent.submit(screen.getByTestId("settings-sync-form"));
    await waitFor(() => screen.getByTestId("sync-msg"));
    expect(screen.getByTestId("sync-msg").textContent).toContain("at least 60");
    expect(calls.some((c) => c.url.includes("/api/compose/projects/") && c.method === "PUT")).toBe(false);
  });

  it("ISI-4830 S4: the connection-health rail re-projects the S1 payload (linked + credential + last test)", async () => {
    stubFetch((url) => (url.includes("/settings") ? jsonResponse(200, settings({ repo: { url: "https://github.com/org/repo", ref: "main", provider: "github", syncEnabled: true, pollIntervalSeconds: 300, reflectOutbound: false } })) : jsonResponse(404, {})));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-health-rail"));
    expect(screen.getByTestId("health-repo").textContent).toContain("Linked");
    expect(screen.getByTestId("health-cred").textContent).toContain("Connected — test passed");
    expect(screen.getByTestId("health-test").textContent).toContain("Passed");
    expect(screen.getByTestId("health-sync").textContent).toContain("Every 300s");
    expect(screen.getByTestId("health-provider").textContent).toContain("github");
  });

  it("AC3: a save validation 422 surfaces the apiserver's field error VERBATIM", async () => {
    stubFetch((url, init) => {
      if (url.includes("/api/squad/projects/")) return jsonResponse(200, { name: "proj-a", repo: { url: "https://github.com/org/repo" } });
      if (url.includes("/api/compose/projects/") && init?.method === "PUT") {
        return jsonResponse(422, { fields: [{ field: "repo.url", message: "must be a github.com URL" }] });
      }
      return jsonResponse(200, settings());
    });
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    fireEvent.change(screen.getByTestId("repo-url-input"), { target: { value: "https://gitlab.com/org/repo" } });
    fireEvent.submit(screen.getByTestId("settings-repo-form"));
    await waitFor(() => screen.getByTestId("repo-msg"));
    expect(screen.getByTestId("repo-msg").textContent).toContain("repo.url: must be a github.com URL");
  });
});

describe("<ProjectSettingsScreen> — ISI-4839 section-nav scroll-spy", () => {
  it("wires an IntersectionObserver to every section card and moves the active link to whichever crosses the trigger band", async () => {
    // Controllable IntersectionObserver: capture the callback so the test drives entries.
    let ioCallback: IntersectionObserverCallback | null = null;
    const observed: Element[] = [];
    class MockIntersectionObserver {
      constructor(cb: IntersectionObserverCallback) {
        ioCallback = cb;
      }
      observe(el: Element) {
        observed.push(el);
      }
      unobserve() {}
      disconnect() {}
      takeRecords() {
        return [] as IntersectionObserverEntry[];
      }
      root = null;
      rootMargin = "";
      thresholds = [] as number[];
    }
    vi.stubGlobal("IntersectionObserver", MockIntersectionObserver as unknown as typeof IntersectionObserver);

    stubFetch((url) => (url.includes("/settings") ? jsonResponse(200, settings()) : jsonResponse(404, {})));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));

    // On mount the first section is active, and all three section cards are observed.
    expect(screen.getByTestId("settings-nav-repository").className).toContain("settings__nav-link--active");
    expect(observed.length).toBe(3);
    expect(ioCallback).not.toBeNull();

    // "Access & credentials" scrolls into the trigger band → the active link follows it,
    // proving the highlight is no longer hardcoded to the first entry.
    const accessEl = document.getElementById("settings-access") as Element;
    act(() => {
      ioCallback?.(
        [
          {
            target: accessEl,
            isIntersecting: true,
            boundingClientRect: { top: 10 } as DOMRectReadOnly,
          } as unknown as IntersectionObserverEntry,
        ],
        {} as IntersectionObserver,
      );
    });
    expect(screen.getByTestId("settings-nav-access").className).toContain("settings__nav-link--active");
    expect(screen.getByTestId("settings-nav-access").getAttribute("aria-current")).toBe("true");
    expect(screen.getByTestId("settings-nav-repository").className).not.toContain("settings__nav-link--active");
  });
});
