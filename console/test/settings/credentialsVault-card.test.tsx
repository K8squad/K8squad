import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { CredentialsVaultCard } from "@/components/settings/CredentialsVaultCard";
import type { CredentialTypeEntry, CredentialTypesResponse } from "@/lib/credentialsVault";

// Acceptance tests for the WS-9 S4 Credentials Vault card (ISI-5847, spec §5/§8):
// rows ARE the consumable types; the value field is write-only with NO reveal
// affordance anywhere in the DOM; a 201 masks and never echoes the typed value;
// required-missing sorts first; canEdit gates every write affordance.

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

function entry(over: Partial<CredentialTypeEntry> = {}): CredentialTypeEntry {
  return {
    type: "github",
    displayName: "GitHub",
    consumption: "header",
    headerName: "Authorization",
    dataKey: "token",
    requiredByGate: true,
    probeKind: "http",
    consumedBy: ["github-pr-review"],
    gateState: "unsatisfied",
    ...over,
  };
}

function typesPayload(types: CredentialTypeEntry[]): CredentialTypesResponse {
  return { project: { name: "proj-a", namespace: "squad-a" }, types };
}

/** Route fetches by URL+method to canned responses. */
function stubFetch(routes: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => Promise.resolve(routes(String(input), init))),
  );
}

describe("<CredentialsVaultCard> — ISI-5847 Slice A acceptance", () => {
  it("renders one row per consumable type with its derived status badge (§2/§3)", async () => {
    stubFetch(() =>
      jsonResponse(
        200,
        typesPayload([
          entry({ type: "github", gateState: "satisfied", requiredByGate: true }),
          entry({ type: "dynatrace", displayName: "Dynatrace", gateState: "unsatisfied", requiredByGate: false }),
        ]),
      ),
    );
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    await waitFor(() => screen.getByTestId("credentials-vault-table"));
    expect(screen.getByTestId("cred-status-github").textContent).toBe("Connected");
    expect(screen.getByTestId("cred-status-dynatrace").textContent).toBe("Not set");
  });

  it("sorts required-but-missing rows FIRST (§6 guided setup)", async () => {
    stubFetch(() =>
      jsonResponse(
        200,
        typesPayload([
          entry({ type: "optional", displayName: "Opt", requiredByGate: false, gateState: "unsatisfied" }),
          entry({ type: "needed", displayName: "Needed", requiredByGate: true, gateState: "unsatisfied" }),
        ]),
      ),
    );
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    await waitFor(() => screen.getByTestId("credentials-vault-table"));
    const rows = screen.getAllByTestId(/^cred-row-/);
    expect(rows[0].getAttribute("data-testid")).toBe("cred-row-needed");
    expect(screen.getByTestId("cred-required-hint-needed")).toBeTruthy();
  });

  it("value field is write-only: a password input, NO reveal affordance, never pre-filled (§5/T1)", async () => {
    stubFetch(() => jsonResponse(200, typesPayload([entry({ gateState: "unsatisfied" })])));
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    const input = (await screen.findByTestId("cred-value-github")) as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.value).toBe(""); // never pre-filled
    // No reveal/show toggle affordance anywhere in the DOM (threat T1): no button
    // toggles the field to text, and no element is labelled to reveal it.
    expect(screen.queryByRole("button", { name: /show|reveal|unmask/i })).toBeNull();
    expect(document.querySelector('[aria-label*="show" i],[aria-label*="reveal" i],[title*="reveal" i]')).toBeNull();
    // The only input for the value stays type=password — never flipped to text.
    expect(document.querySelectorAll('[data-testid="cred-value-github"][type="text"]').length).toBe(0);
  });

  it("a satisfied type shows the masked dots, not an input (§5)", async () => {
    stubFetch(() => jsonResponse(200, typesPayload([entry({ gateState: "satisfied" })])));
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    await waitFor(() => screen.getByTestId("cred-masked-github"));
    expect(screen.getByTestId("cred-masked-github").textContent).toMatch(/^[•]+$/);
    expect(screen.queryByTestId("cred-value-github")).toBeNull();
  });

  it("Create POSTs the vault-axis body and masks after 201 — the typed value is never echoed back", async () => {
    let created = false;
    const bodies: string[] = [];
    stubFetch((url, init) => {
      if (url.includes("/credential-types")) {
        // After the create, the reload reflects the now-satisfied gate-state.
        return jsonResponse(200, typesPayload([entry({ gateState: created ? "satisfied" : "unsatisfied" })]));
      }
      if (url === "/api/credentials" && init?.method === "POST") {
        bodies.push(String(init.body));
        created = true;
        return jsonResponse(201, { secretRef: "secret://squad-a/proj-proj-a-cred-github", name: "x", namespace: "squad-a", class: "github" });
      }
      return jsonResponse(404, {});
    });
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    const input = (await screen.findByTestId("cred-value-github")) as HTMLInputElement;
    fireEvent.change(input, { target: { value: "ghp_supersecret" } });
    fireEvent.click(screen.getByTestId("cred-save-github"));
    await waitFor(() => screen.getByTestId("cred-masked-github"));
    // The write body carried the vault axis with the value verbatim…
    expect(JSON.parse(bodies[0])).toEqual({ name: "github", value: "ghp_supersecret", type: "github", projectId: "proj-a" });
    // …and after the 201 the value is gone from the DOM — masked, never re-rendered.
    expect(screen.queryByTestId("cred-value-github")).toBeNull();
    expect(document.body.innerHTML).not.toContain("ghp_supersecret");
  });

  it("a 409 surfaces the Rotate hint (the replace path, Slice B)", async () => {
    stubFetch((url, init) => {
      if (url.includes("/credential-types")) return jsonResponse(200, typesPayload([entry({ gateState: "unsatisfied" })]));
      if (url === "/api/credentials" && init?.method === "POST")
        return jsonResponse(409, { error: "a credential of this type already exists for this project" });
      return jsonResponse(404, {});
    });
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    const input = (await screen.findByTestId("cred-value-github")) as HTMLInputElement;
    fireEvent.change(input, { target: { value: "x" } });
    fireEvent.click(screen.getByTestId("cred-save-github"));
    await waitFor(() => screen.getByTestId("cred-msg-github"));
    expect(screen.getByTestId("cred-msg-github").textContent).toMatch(/already exists/);
  });

  it("Test is disabled for a probeless type with the §7 tooltip", async () => {
    stubFetch(() => jsonResponse(200, typesPayload([entry({ probeKind: "none" })])));
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    const test = (await screen.findByTestId("cred-test-github")) as HTMLButtonElement;
    expect(test.disabled).toBe(true);
    expect(test.title).toMatch(/No connectivity test/);
  });

  it("canEdit=false renders read-only — no value input, no Save/Rotate/Delete", async () => {
    stubFetch(() =>
      jsonResponse(200, typesPayload([entry({ gateState: "unsatisfied" }), entry({ type: "ds", displayName: "DS", gateState: "satisfied" })])),
    );
    render(<CredentialsVaultCard projectId="proj-a" canEdit={false} />);
    await waitFor(() => screen.getByTestId("credentials-vault-table"));
    expect(screen.queryByTestId("cred-value-github")).toBeNull();
    expect(screen.queryByTestId("cred-save-github")).toBeNull();
    expect(screen.queryByTestId("cred-rotate-ds")).toBeNull();
    expect(screen.queryByTestId("cred-delete-ds")).toBeNull();
    expect(screen.getByTestId("cred-value-readonly-github")).toBeTruthy();
  });

  it("empty types ⇒ the friendly empty card, no add button (§6)", async () => {
    stubFetch(() => jsonResponse(200, typesPayload([])));
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    await waitFor(() => screen.getByTestId("credentials-empty"));
    expect(screen.queryByTestId("credentials-vault-table")).toBeNull();
  });

  it("501 ⇒ honest 'not available yet' frame", async () => {
    stubFetch(() => jsonResponse(501, {}));
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    await waitFor(() => screen.getByTestId("credentials-not-wired"));
  });
});
