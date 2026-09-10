import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
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
  it("AC1: renders repo url/ref/provider + honest tri-state badge from the S1 projection", async () => {
    stubFetch((url) => (url.includes("/settings") ? jsonResponse(200, settings()) : jsonResponse(404, {})));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    expect(screen.getByTestId("repo-url").textContent).toBe("https://github.com/org/repo");
    expect(screen.getByTestId("repo-ref").textContent).toBe("main");
    expect(screen.getByTestId("repo-provider").textContent).toBe("github");
    expect(screen.getByTestId("cred-status-badge").textContent).toBe("Connected — test passed");
  });

  it("AC1: an empty ref renders 'default branch', never a blank", async () => {
    stubFetch(() => jsonResponse(200, settings({ repo: { url: "https://github.com/org/repo", ref: "", provider: "github", syncEnabled: false, pollIntervalSeconds: 0, reflectOutbound: false } })));
    render(<ProjectSettingsScreen projectId="proj-a" />);
    await waitFor(() => screen.getByTestId("settings-ready"));
    expect(screen.getByTestId("repo-ref").textContent).toBe("default branch");
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
