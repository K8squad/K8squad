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

describe("<CredentialsVaultCard> — ISI-5860 Slice B: Rotate + Delete", () => {
  it("a saved, editable row renders enabled Rotate + Delete; the unsatisfied row does not", async () => {
    stubFetch(() =>
      jsonResponse(
        200,
        typesPayload([
          entry({ type: "github", gateState: "satisfied" }),
          entry({ type: "ds", displayName: "DS", gateState: "unsatisfied", requiredByGate: false }),
        ]),
      ),
    );
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    await waitFor(() => screen.getByTestId("credentials-vault-table"));
    expect((screen.getByTestId("cred-rotate-github") as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByTestId("cred-delete-github") as HTMLButtonElement).disabled).toBe(false);
    // The unsatisfied row has Create affordances, not Rotate/Delete.
    expect(screen.queryByTestId("cred-rotate-ds")).toBeNull();
    expect(screen.queryByTestId("cred-delete-ds")).toBeNull();
  });

  it("Rotate reveals a write-only input, PUTs {value,type,projectId}, and never echoes the new value", async () => {
    let rotated = false;
    const puts: Array<{ method: string; body: string }> = [];
    stubFetch((url, init) => {
      if (url.includes("/credential-types")) return jsonResponse(200, typesPayload([entry({ type: "github", gateState: "satisfied" })]));
      if (url === "/api/credentials" && init?.method === "PUT") {
        puts.push({ method: "PUT", body: String(init.body) });
        rotated = true;
        return jsonResponse(200, { secretRef: "secret://squad-a/proj-proj-a-cred-github" });
      }
      return jsonResponse(404, {});
    });
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    // Rotate input is NOT in the DOM until Rotate is clicked.
    await waitFor(() => screen.getByTestId("cred-rotate-github"));
    expect(screen.queryByTestId("cred-rotate-value-github")).toBeNull();
    fireEvent.click(screen.getByTestId("cred-rotate-github"));
    const rinput = (await screen.findByTestId("cred-rotate-value-github")) as HTMLInputElement;
    // write-only: password type, never pre-filled, NO reveal toggle anywhere.
    expect(rinput.type).toBe("password");
    expect(rinput.value).toBe("");
    fireEvent.change(rinput, { target: { value: "ghp_rotated_secret" } });
    fireEvent.click(screen.getByTestId("cred-rotate-confirm-github"));
    await waitFor(() => screen.getByTestId("cred-masked-github"));
    // The PUT carried the vault-axis rotate body (new value verbatim, write-only).
    expect(JSON.parse(puts[0].body)).toEqual({ value: "ghp_rotated_secret", type: "github", projectId: "proj-a" });
    // After the 200 the new value is gone from the DOM — masked, never echoed back.
    void rotated;
    expect(screen.queryByTestId("cred-rotate-value-github")).toBeNull();
    expect(document.body.innerHTML).not.toContain("ghp_rotated_secret");
  });

  it("a rotate 409 surfaces the honest 'still being removed — retry' copy, not a create conflict", async () => {
    stubFetch((url, init) => {
      if (url.includes("/credential-types")) return jsonResponse(200, typesPayload([entry({ type: "github", gateState: "satisfied" })]));
      if (url === "/api/credentials" && init?.method === "PUT")
        return jsonResponse(409, { error: "the previous credential is still being removed; retry the rotation" });
      return jsonResponse(404, {});
    });
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    fireEvent.click(await screen.findByTestId("cred-rotate-github"));
    const rinput = (await screen.findByTestId("cred-rotate-value-github")) as HTMLInputElement;
    fireEvent.change(rinput, { target: { value: "x" } });
    fireEvent.click(screen.getByTestId("cred-rotate-confirm-github"));
    await waitFor(() => screen.getByTestId("cred-msg-github"));
    expect(screen.getByTestId("cred-msg-github").textContent).toMatch(/still being removed/i);
  });

  it("Cancel on rotate drops the half-typed new value so it never lingers", async () => {
    stubFetch(() => jsonResponse(200, typesPayload([entry({ type: "github", gateState: "satisfied" })])));
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    fireEvent.click(await screen.findByTestId("cred-rotate-github"));
    const rinput = (await screen.findByTestId("cred-rotate-value-github")) as HTMLInputElement;
    fireEvent.change(rinput, { target: { value: "ghp_halftyped" } });
    fireEvent.click(screen.getByTestId("cred-rotate-cancel-github"));
    await waitFor(() => screen.getByTestId("cred-masked-github"));
    expect(screen.queryByTestId("cred-rotate-value-github")).toBeNull();
    expect(document.body.innerHTML).not.toContain("ghp_halftyped");
  });

  it("Delete is a two-step inline confirm, DELETEs {type,projectId}, and refreshes the table", async () => {
    let deleted = false;
    const dels: string[] = [];
    stubFetch((url, init) => {
      if (url.includes("/credential-types"))
        return jsonResponse(200, typesPayload([entry({ type: "github", gateState: deleted ? "unsatisfied" : "satisfied" })]));
      if (url === "/api/credentials" && init?.method === "DELETE") {
        dels.push(String(init.body));
        deleted = true;
        return jsonResponse(200, {});
      }
      return jsonResponse(404, {});
    });
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    fireEvent.click(await screen.findByTestId("cred-delete-github"));
    // Nothing destructive fires on the first click — the confirm step gates it.
    const confirm = (await screen.findByTestId("cred-delete-confirm-github")) as HTMLButtonElement;
    expect(dels.length).toBe(0);
    fireEvent.click(confirm);
    await waitFor(() => screen.getByTestId("cred-msg-github"));
    expect(JSON.parse(dels[0])).toEqual({ type: "github", projectId: "proj-a" });
    // Reload reflected the now-unsatisfied gate-state → the write-only create input returns.
    expect(screen.getByTestId("cred-value-github")).toBeTruthy();
    expect(screen.getByTestId("cred-msg-github").textContent).toMatch(/deleted/i);
  });

  it("a delete 404 is treated as 'already gone' — success, refresh, not an error", async () => {
    let gone = false;
    stubFetch((url, init) => {
      if (url.includes("/credential-types"))
        return jsonResponse(200, typesPayload([entry({ type: "github", gateState: gone ? "unsatisfied" : "satisfied" })]));
      if (url === "/api/credentials" && init?.method === "DELETE") {
        gone = true;
        return jsonResponse(404, { error: "no credential of this type for this project" });
      }
      return jsonResponse(404, {});
    });
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    fireEvent.click(await screen.findByTestId("cred-delete-github"));
    fireEvent.click(await screen.findByTestId("cred-delete-confirm-github"));
    await waitFor(() => screen.getByTestId("cred-msg-github"));
    const msg = screen.getByTestId("cred-msg-github");
    expect(msg.getAttribute("class")).toMatch(/--ok/);
    expect(msg.textContent).toMatch(/already removed/i);
  });

  it("Cancel on the delete confirm aborts without a DELETE", async () => {
    const dels: string[] = [];
    stubFetch((url, init) => {
      if (url.includes("/credential-types")) return jsonResponse(200, typesPayload([entry({ type: "github", gateState: "satisfied" })]));
      if (url === "/api/credentials" && init?.method === "DELETE") {
        dels.push(String(init.body));
        return jsonResponse(200, {});
      }
      return jsonResponse(404, {});
    });
    render(<CredentialsVaultCard projectId="proj-a" canEdit={true} />);
    fireEvent.click(await screen.findByTestId("cred-delete-github"));
    fireEvent.click(await screen.findByTestId("cred-delete-cancel-github"));
    await waitFor(() => screen.getByTestId("cred-delete-github"));
    expect(dels.length).toBe(0);
    expect(screen.queryByTestId("cred-delete-confirm-github")).toBeNull();
  });
});
