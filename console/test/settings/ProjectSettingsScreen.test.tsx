// test/settings/ProjectSettingsScreen.test.tsx — the project Settings tab (ISI-4000 / S2).
// AC1 renders projection values; AC3 save issues the merged compose PUT + surfaces a field error
// verbatim; AC4 PAT POST → repoint → field clears (never echoed); AC5 test-connection renders
// {ok,detail} verbatim (incl. an honest red); AC6 read-only viewer has no write affordances;
// AC7 honest 501 / error / no-repo frames.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { ProjectSettingsScreen } from "@/components/settings/ProjectSettingsScreen";
import type { ProjectSettings } from "@/lib/projectSettings";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

interface Call {
  url: string;
  method: string;
  body: unknown;
}

/** Route a fetch by (method, url-substring) to a {status, body}. Records every call. */
function routeFetch(routes: Array<{ match: (url: string, method: string) => boolean; status: number; body?: unknown }>) {
  const calls: Call[] = [];
  const spy = vi.fn((url: string, init?: RequestInit) => {
    const method = (init?.method ?? "GET").toUpperCase();
    let body: unknown = undefined;
    if (typeof init?.body === "string") {
      try {
        body = JSON.parse(init.body);
      } catch {
        body = init.body;
      }
    }
    calls.push({ url, method, body });
    const route = routes.find((r) => r.match(url, method));
    const status = route?.status ?? 500;
    return Promise.resolve({
      ok: status >= 200 && status < 300,
      status,
      json: () => Promise.resolve(route?.body ?? null),
      text: () => Promise.resolve(JSON.stringify(route?.body ?? null)),
    } as Response);
  });
  vi.stubGlobal("fetch", spy as unknown as typeof fetch);
  return calls;
}

const connected: ProjectSettings = {
  project: { name: "web", namespace: "squad-a" },
  repo: { url: "https://github.com/acme/web", ref: "main", provider: "github", syncEnabled: false, pollIntervalSeconds: 0, reflectOutbound: false },
  auth: { connected: true, credentialSecretRefName: "web-scm-abc", lastTest: "passed" },
  canEdit: true,
};

const authoring = {
  name: "web",
  repo: { url: "https://github.com/acme/web", ref: "main", auth: { credentialSecretRef: { name: "web-scm-abc", key: "apiKey" } } },
  goals: ["ship it"],
  egressPolicyRef: { name: "default-egress" },
};

describe("ProjectSettingsScreen", () => {
  it("renders values from the projection read-only, with no write affordances (AC1 + AC6)", async () => {
    routeFetch([{ match: (u) => u.includes("/settings"), status: 200, body: { ...connected, canEdit: false } }]);
    render(<ProjectSettingsScreen projectId="web" />);

    await waitFor(() => expect(screen.getByTestId("settings-repo-readonly")).toBeTruthy());
    expect(screen.getByText("https://github.com/acme/web")).toBeTruthy();
    expect(screen.getByText("main")).toBeTruthy();
    // Honest tri-state badge (AC2).
    expect(screen.getByTestId("settings-cred-status").textContent).toContain("Connected — test passed");
    // No write affordances for a read-only viewer.
    expect(screen.queryByTestId("settings-repo-url")).toBeNull();
    expect(screen.queryByTestId("settings-pat-input")).toBeNull();
    expect(screen.queryByTestId("settings-repo-save")).toBeNull();
    expect(screen.getByTestId("settings-readonly-note")).toBeTruthy();
  });

  it("renders the honest 501 not-wired frame (AC7)", async () => {
    routeFetch([{ match: (u) => u.includes("/settings"), status: 501 }]);
    render(<ProjectSettingsScreen projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("settings-not-wired")).toBeTruthy());
  });

  it("renders the honest error frame on a 5xx (AC7)", async () => {
    routeFetch([{ match: (u) => u.includes("/settings"), status: 500 }]);
    render(<ProjectSettingsScreen projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("settings-error")).toBeTruthy());
  });

  it("shows 'no repository connected' honestly when the repo is empty (AC7)", async () => {
    const empty: ProjectSettings = { ...connected, repo: { ...connected.repo, url: "" }, auth: { connected: false, credentialSecretRefName: "", lastTest: "untested" } };
    routeFetch([{ match: (u) => u.includes("/settings"), status: 200, body: empty }]);
    render(<ProjectSettingsScreen projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("settings-no-repo")).toBeTruthy());
    expect(screen.getByTestId("settings-cred-status").textContent).toContain("Not connected");
  });

  it("saves the repo via a MERGED compose PUT (preserving goals+auth) and re-reads (AC3)", async () => {
    const calls = routeFetch([
      { match: (u) => u.includes("/settings"), status: 200, body: connected },
      { match: (u) => u.includes("/api/squad/projects/"), status: 200, body: authoring },
      { match: (u, m) => u.includes("/api/compose/projects/") && m === "PUT", status: 200, body: { operation: "updated" } },
    ]);
    render(<ProjectSettingsScreen projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("settings-repo-url")).toBeTruthy());

    fireEvent.change(screen.getByTestId("settings-repo-url"), { target: { value: "https://github.com/acme/web2" } });
    fireEvent.click(screen.getByTestId("settings-repo-save"));

    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    const put = calls.find((c) => c.method === "PUT")!;
    const body = put.body as { repo: { url: string; auth?: unknown }; goals?: string[] };
    expect(body.repo.url).toBe("https://github.com/acme/web2");
    // no-wipe: goals + the existing credential ride through the PUT.
    expect(body.goals).toEqual(["ship it"]);
    expect(body.repo.auth).toEqual({ credentialSecretRef: { name: "web-scm-abc", key: "apiKey" } });
  });

  it("surfaces a compose field error VERBATIM, not a generic toast (AC3)", async () => {
    routeFetch([
      { match: (u) => u.includes("/settings"), status: 200, body: connected },
      { match: (u) => u.includes("/api/squad/projects/"), status: 200, body: authoring },
      { match: (u, m) => u.includes("/api/compose/projects/") && m === "PUT", status: 422, body: { error: "validation failed", fields: [{ field: "repo.url", message: "v1 supports github.com repositories only" }] } },
    ]);
    render(<ProjectSettingsScreen projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("settings-repo-url")).toBeTruthy());
    fireEvent.change(screen.getByTestId("settings-repo-url"), { target: { value: "https://gitlab.com/acme/web" } });
    fireEvent.click(screen.getByTestId("settings-repo-save"));
    await waitFor(() =>
      expect(screen.getByText("v1 supports github.com repositories only")).toBeTruthy(),
    );
  });

  it("sets the PAT (POST credentials → repoint PUT), clears the field, and never echoes it (AC4)", async () => {
    const calls = routeFetch([
      { match: (u) => u.includes("/settings"), status: 200, body: connected },
      { match: (u, m) => u.includes("/api/credentials") && m === "POST", status: 201, body: { name: "web-scm-new", secretRef: "secret://squad-a/web-scm-new" } },
      { match: (u) => u.includes("/api/squad/projects/"), status: 200, body: authoring },
      { match: (u, m) => u.includes("/api/compose/projects/") && m === "PUT", status: 200, body: { operation: "updated" } },
    ]);
    render(<ProjectSettingsScreen projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("settings-pat-input")).toBeTruthy());

    const patInput = screen.getByTestId("settings-pat-input") as HTMLInputElement;
    fireEvent.change(patInput, { target: { value: "ghp_supersecret" } });
    fireEvent.click(screen.getByTestId("settings-pat-save"));

    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    // The credential POST carried the token; the repoint PUT points at the new Secret with key apiKey.
    const post = calls.find((c) => c.url.includes("/api/credentials") && c.method === "POST")!;
    expect((post.body as { value: string }).value).toBe("ghp_supersecret");
    const put = calls.find((c) => c.method === "PUT")!;
    expect((put.body as { repo: { auth: { credentialSecretRef: { name: string; key: string } } } }).repo.auth).toEqual({
      credentialSecretRef: { name: "web-scm-new", key: "apiKey" },
    });
    // Write-only: the field clears and the token is nowhere in the DOM.
    await waitFor(() => expect(patInput.value).toBe(""));
    expect(document.body.innerHTML).not.toContain("ghp_supersecret");
  });

  it("renders the test-connection {ok:false, detail} VERBATIM (an honest red — AC5)", async () => {
    routeFetch([
      { match: (u) => u.includes("/settings"), status: 200, body: connected },
      { match: (u, m) => u.includes("/repo-auth/test") && m === "POST", status: 200, body: { ok: false, detail: "provider rejected the credential (401 unauthorized)" } },
    ]);
    render(<ProjectSettingsScreen projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("settings-test-btn")).toBeTruthy());
    fireEvent.click(screen.getByTestId("settings-test-btn"));
    await waitFor(() => {
      const el = screen.getByTestId("settings-test-result");
      expect(el.getAttribute("data-ok")).toBe("false");
      expect(el.textContent).toContain("provider rejected the credential (401 unauthorized)");
    });
  });
});
