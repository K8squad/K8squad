import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
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
    credentialRef: "squad-a/sam/hermes-oauth",
    expiresKnown: false,
    health: "connected",
    ...over,
  };
}

describe("<CredentialsScreen> — 8.6 ACs", () => {
  it("renders the per-agent credential rows with BYO Secret refs (FR-G1 surface)", async () => {
    render(
      <CredentialsScreen
        load={async () =>
          jsonResponse(200, overview([row(), row({ agent: "reviewer-openclaw", runtime: "OpenClaw", credentialRef: "squad-a/sam/openclaw-key", credentialClass: "api_key" })]))
        }
        now={clock}
      />,
    );
    await waitFor(() => screen.getByTestId("creds-table"));
    expect(screen.getByText("fixer-hermes")).toBeTruthy();
    expect(screen.getByText("squad-a/sam/hermes-oauth")).toBeTruthy();
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

  it("renders Valid + honest — expiry for unknown horizons (no fabricated numbers)", async () => {
    render(
      <CredentialsScreen load={async () => jsonResponse(200, overview([row()]))} now={clock} />,
    );
    await waitFor(() => screen.getByTestId("creds-table"));
    expect(screen.getAllByTestId("health-badge")[0].textContent).toBe("Valid");
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

  it("Connect Claude surfaces the 7.7 seam's legible not-configured message (501)", async () => {
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
    expect(screen.getByTestId("connect-msg").textContent).toContain("not yet hosted");
    expect(screen.getByTestId("connect-msg").textContent).not.toMatch(/token|secret/i);
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
