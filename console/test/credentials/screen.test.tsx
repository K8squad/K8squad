import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { CredentialsScreen } from "@/components/credentials/CredentialsScreen";
import type { AgentCredentialRow, CredentialsOverview } from "@/lib/credentials";

afterEach(cleanup);

// The 8.6 screen at the component boundary: rows + paused banner render from the
// read model, unknown expiry stays honest, the documented 501 renders its
// unconfigured state, the deny collapse renders not-found, and the Connect
// Claude button surfaces the 7.7 seam's legible not-configured message.

const NOW = new Date("2026-08-20T13:00:00Z");
const clock = () => NOW;

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function overview(agents: AgentCredentialRow[]): CredentialsOverview {
  return { team: "squad-a", agents, connectClaude: false };
}

function row(over: Partial<AgentCredentialRow> = {}): AgentCredentialRow {
  return {
    agent: "fixer-hermes",
    namespace: "squad-a",
    runtime: "Hermes",
    credentialRef: "squad-a/sam-hermes-oauth",
    expiresKnown: false,
    // Mirrors the honest backend default: zero credential knowledge ⇒ unknown (PR #87 review).
    health: "unknown",
    ...over,
  };
}

describe("<CredentialsScreen> — 8.6 ACs", () => {
  it("renders the per-agent credential rows with BYO Secret refs (FR-G1 surface)", async () => {
    render(
      <CredentialsScreen
        load={async () =>
          jsonResponse(200, overview([row(), row({ agent: "reviewer-openclaw", runtime: "OpenClaw", credentialRef: "squad-a/sam-openclaw-key", credentialClass: "api_key" })]))
        }
        now={clock}
      />,
    );
    await waitFor(() => screen.getByTestId("creds-table"));
    expect(screen.getByText("fixer-hermes")).toBeTruthy();
    expect(screen.getByText("squad-a/sam-hermes-oauth")).toBeTruthy();
    expect(screen.getByText("API key")).toBeTruthy();
    expect(screen.getByText("— (static)")).toBeTruthy();
    // FR-G1 footer fact is on the screen.
    expect(screen.getByText(/KSquad never stores a shared master credential/)).toBeTruthy();
  });

  it("shows the clear paused-on-expiry banner + expired·paused badge (S10 / 7.4)", async () => {
    const since = new Date("2026-08-20T12:41:00Z").toISOString();
    render(
      <CredentialsScreen
        load={async () =>
          jsonResponse(200, overview([row({ health: "expired", pausedRuns: [{ name: "run-139", reason: "credential_expired", since }] })]))
        }
        now={clock}
      />,
    );
    await waitFor(() => screen.getByTestId("paused-banner"));
    expect(screen.getAllByText(/run-139/).length).toBeGreaterThan(0);
    expect(screen.getByText(/token expired/)).toBeTruthy();
    expect(screen.getByTestId("health-badge").textContent).toBe("Expired · paused");
    const link = screen.getByRole("link", { name: "#run-139" }) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/runs/run-139");
  });

  it("renders Unknown + honest — expiry for zero-knowledge rows (no fabricated green badge)", async () => {
    render(
      <CredentialsScreen load={async () => jsonResponse(200, overview([row()]))} now={clock} />,
    );
    await waitFor(() => screen.getByTestId("creds-table"));
    // Zero credential knowledge ⇒ Unknown (idle tone), never a fabricated Valid badge (PR #87
    // review: absence of a paused Run is not evidence a credential works).
    expect(screen.getAllByTestId("health-badge")[0].textContent).toBe("Unknown");
    expect(screen.getAllByText("—").length).toBeGreaterThan(0);
    expect(screen.queryByTestId("paused-banner")).toBeNull();
  });

  it("renders the documented-501 unconfigured state, not a fake table", async () => {
    render(
      <CredentialsScreen
        load={async () => jsonResponse(501, { error: "not implemented" })}
        now={clock}
      />,
    );
    await waitFor(() => screen.getByTestId("creds-unconfigured"));
    expect(screen.queryByTestId("creds-table")).toBeNull();
  });

  it("renders the deny-collapsed not-found state (401/403/404 indistinguishable)", async () => {
    render(
      <CredentialsScreen load={async () => jsonResponse(403, {})} now={clock} />,
    );
    await waitFor(() => screen.getByTestId("creds-not-found"));
  });

  it("renders the error state on 5xx", async () => {
    render(
      <CredentialsScreen load={async () => jsonResponse(502, {})} now={clock} />,
    );
    await waitFor(() => screen.getByTestId("creds-error"));
  });

  it("empty squad renders an explicit empty row, never a blank table", async () => {
    render(
      <CredentialsScreen load={async () => jsonResponse(200, overview([]))} now={clock} />,
    );
    await waitFor(() => screen.getByTestId("creds-table"));
    expect(screen.getByText(/No agents with credentials/)).toBeTruthy();
  });

  it("Connect Claude shows a friendly, honest fallback on 501 — never the raw apiserver detail, never a nonexistent CLI (ISI-3945)", async () => {
    render(
      <CredentialsScreen
        load={async () => jsonResponse(200, overview([]))}
        connect={async () =>
          jsonResponse(501, { error: "not implemented", detail: "Connect Claude (zero-touch OAuth lifecycle, story 7.7) is not yet hosted by the apiserver", tracking: "ISI-2899" })
        }
        now={clock}
      />,
    );
    await waitFor(() => screen.getByTestId("creds-table"));
    const btn = screen.getByTestId("connect-claude") as HTMLButtonElement;
    btn.click();
    await waitFor(() => screen.getByTestId("connect-msg"));
    const msg = screen.getByTestId("connect-msg").textContent ?? "";
    // Friendly, honest copy: names the coming-soon tracking, no false present-tense claims.
    expect(msg).toContain("isn't available yet");
    expect(msg).toContain("ISI-2899");
    // ISI-3945: no dangling nonexistent CLI — the honesty fix Henrik asked for.
    expect(msg).not.toContain("ksquad auth login");
    expect(msg).not.toContain("auth login");
    // The raw backend detail never reaches the user (ISI-3935 leak guard, preserved).
    expect(msg).not.toContain("not yet hosted");
    expect(msg).not.toContain("story 7.7");
    expect(msg).not.toContain("zero-touch");
    expect(msg).not.toMatch(/token|secret/i);
  });

  it("Connect Claude hint is honest — states no CLI ships, offers no fake command (ISI-3945)", async () => {
    render(
      <CredentialsScreen load={async () => jsonResponse(200, overview([]))} now={clock} />,
    );
    await waitFor(() => screen.getByTestId("creds-table"));
    const btn = screen.getByTestId("connect-claude");
    const hint = screen.getByText(/No CLI is shipped/);
    // Button and hint remain distinct siblings in the creds__connect flex row.
    expect(btn.parentElement).toBe(hint.parentElement);
    // No dangling nonexistent CLI command, in any form (code element or backticks).
    expect(hint.querySelector("code")).toBeNull();
    expect(hint.textContent).not.toContain("ksquad auth login");
    expect((hint.parentElement as HTMLElement)?.textContent).not.toContain("`");
  });

  it("network failure of the loader degrades to the error state (never a crash)", async () => {
    render(
      <CredentialsScreen
        load={async () => {
          throw new Error("network down");
        }}
        now={clock}
      />,
    );
    await waitFor(() => screen.getByTestId("creds-error"));
  });
});

// ── ISI-3983: the BYO write UX (paste form + fleet-admin team picker) ────────────

function teamsResponse(teams: { name: string; namespace: string; uid: string }[], fleet = true): Response {
  return jsonResponse(200, { teams, fleet });
}

describe("<CredentialsScreen> — BYO create form (ISI-3983)", () => {
  it("renders the paste form with the credinject runtimes and stores a key (write-only)", async () => {
    const create = vi.fn(async () =>
      jsonResponse(201, { secretRef: "secret://ksquad-team-x/team-anthropic", name: "team-anthropic", namespace: "ksquad-team-x", class: "service-account" }),
    );
    render(
      <CredentialsScreen load={async () => jsonResponse(200, overview([]))} create={create} now={clock} />,
    );
    await waitFor(() => screen.getByTestId("creds-create-form"));

    // Runtime options mirror the injection table (claude-code/openclaw/hermes).
    const runtime = screen.getByTestId("creds-runtime") as HTMLSelectElement;
    expect([...runtime.options].map((o) => o.value)).toEqual(["claude-code", "openclaw", "hermes"]);

    fireEvent.change(screen.getByTestId("creds-name"), { target: { value: "team-anthropic" } });
    fireEvent.change(screen.getByTestId("creds-value"), { target: { value: "sk-secret-key" } });
    // The key input is a password field — never a visible token string on the page (FR-G2).
    expect((screen.getByTestId("creds-value") as HTMLInputElement).type).toBe("password");

    fireEvent.click(screen.getByTestId("creds-submit"));
    await waitFor(() => screen.getByTestId("creds-create-msg"));

    expect(create).toHaveBeenCalledTimes(1);
    const body = (create.mock.calls[0] as unknown[])[0] as Record<string, string>;
    expect(body).toMatchObject({ name: "team-anthropic", runtime: "claude-code", class: "service-account", value: "sk-secret-key" });
    // Success shows the returned ref, never the pasted value; the value field is cleared.
    const msg = screen.getByTestId("creds-create-msg").textContent ?? "";
    expect(msg).toContain("secret://ksquad-team-x/team-anthropic");
    expect(msg).not.toContain("sk-secret-key");
    expect((screen.getByTestId("creds-value") as HTMLInputElement).value).toBe("");
  });

  it("surfaces the apiserver's 422 field errors honestly (never echoes the value)", async () => {
    const create = vi.fn(async () =>
      jsonResponse(422, { error: "validation failed", fields: [{ field: "name", message: "name must be a DNS-1123 subdomain" }] }),
    );
    render(
      <CredentialsScreen load={async () => jsonResponse(200, overview([]))} create={create} now={clock} />,
    );
    await waitFor(() => screen.getByTestId("creds-create-form"));
    fireEvent.change(screen.getByTestId("creds-name"), { target: { value: "BAD_NAME" } });
    fireEvent.change(screen.getByTestId("creds-value"), { target: { value: "k" } });
    fireEvent.click(screen.getByTestId("creds-submit"));
    await waitFor(() => screen.getByTestId("creds-create-msg"));
    expect(screen.getByTestId("creds-create-msg").textContent).toContain("DNS-1123");
  });

  it("Test connection after create posts the teamId hint and shows the honest probe result", async () => {
    const create = vi.fn(async () => jsonResponse(201, { secretRef: "secret://ns/team-anthropic", name: "team-anthropic", namespace: "ns", class: "service-account" }));
    const test = vi.fn(async () => jsonResponse(200, { ok: false, detail: "Rejected — the endpoint declined the credential (HTTP 401)" }));
    render(
      <CredentialsScreen load={async () => jsonResponse(200, overview([]))} create={create} test={test} now={clock} />,
    );
    await waitFor(() => screen.getByTestId("creds-create-form"));
    fireEvent.change(screen.getByTestId("creds-name"), { target: { value: "team-anthropic" } });
    fireEvent.change(screen.getByTestId("creds-value"), { target: { value: "sk-x" } });
    fireEvent.click(screen.getByTestId("creds-submit"));
    await waitFor(() => screen.getByTestId("creds-test"));
    fireEvent.click(screen.getByTestId("creds-test"));
    await waitFor(() => screen.getByTestId("creds-test-msg"));
    expect(test).toHaveBeenCalledWith("team-anthropic", { runtime: "claude-code", teamId: undefined });
    expect(screen.getByTestId("creds-test-msg").textContent).toContain("declined the credential");
  });

  it("Connect Claude's honest 501 copy routes the user to the BYO paste form as the v1 path", async () => {
    render(
      <CredentialsScreen
        load={async () => jsonResponse(200, overview([]))}
        connect={async () => jsonResponse(501, { error: "not implemented", tracking: "ISI-2899" })}
        now={clock}
      />,
    );
    await waitFor(() => screen.getByTestId("creds-create-form"));
    screen.getByTestId("connect-claude").click();
    await waitFor(() => screen.getByTestId("connect-msg"));
    const msg = screen.getByTestId("connect-msg").textContent ?? "";
    expect(msg).toContain("isn't available yet");
    expect(msg.toLowerCase()).toContain("service-account key below");
    // No token/secret leak, no nonexistent CLI (ISI-3945/ISI-3935 guards preserved).
    expect(msg).not.toMatch(/token|secret/i);
    expect(msg).not.toContain("auth login");
  });
});

describe("<CredentialsScreen> — fleet-admin team picker (ISI-3937/ISI-3983)", () => {
  it("400 'select a team' shows the picker; choosing a team re-fetches with teamId", async () => {
    const load = vi.fn(async (teamId?: string) =>
      teamId ? jsonResponse(200, overview([row({ agent: "a" })])) : jsonResponse(400, { error: "select a team for this credential" }),
    );
    const loadTeams = vi.fn(async () =>
      teamsResponse([
        { name: "alpha", namespace: "ksquad-team-alpha", uid: "uid-alpha" },
        { name: "beta", namespace: "ksquad-team-beta", uid: "uid-beta" },
      ]),
    );
    render(<CredentialsScreen load={load} loadTeams={loadTeams} now={clock} />);

    await waitFor(() => screen.getByTestId("creds-team-prompt"));
    // The write form waits for a team selection.
    expect((screen.getByTestId("creds-submit") as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(screen.getByTestId("creds-team-select"), { target: { value: "uid-beta" } });
    await waitFor(() => screen.getByTestId("creds-table"));
    expect(load).toHaveBeenLastCalledWith("uid-beta");
    expect((screen.getByTestId("creds-submit") as HTMLButtonElement).disabled).toBe(false);
  });

  it("single-team fleet falls back to that team automatically — no prompt (issue's default)", async () => {
    const load = vi.fn(async (teamId?: string) =>
      teamId ? jsonResponse(200, overview([])) : jsonResponse(400, { error: "select a team for this credential" }),
    );
    const loadTeams = vi.fn(async () => teamsResponse([{ name: "solo", namespace: "ksquad-team-solo", uid: "uid-solo" }]));
    render(<CredentialsScreen load={load} loadTeams={loadTeams} now={clock} />);

    await waitFor(() => screen.getByTestId("creds-table"));
    expect(load).toHaveBeenLastCalledWith("uid-solo");
    expect(screen.queryByTestId("creds-team-prompt")).toBeNull();
    // A single team needs no picker at all.
    expect(screen.queryByTestId("creds-team-picker")).toBeNull();
  });
});
