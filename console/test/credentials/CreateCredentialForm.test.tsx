// test/credentials/CreateCredentialForm.test.tsx — the extracted BYO paste form at the
// component boundary (ISI-3983 contract, unchanged by the move out of CredentialsScreen)
// plus the fixedRuntime pin the Claude lane adds (ISI-6702 WS-4): pinned to claude-code
// the runtime select locks, and the write/probe bodies carry runtime "claude-code" —
// the runtime the ANTHROPIC_API_KEY probe resolves through the injection table.

import { describe, it, expect, vi, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { CreateCredentialForm } from "@/components/credentials/CreateCredentialForm";

afterEach(cleanup);

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

describe("<CreateCredentialForm> — fixedRuntime pin (ISI-6702)", () => {
  it("pinned to claude-code: the runtime select locks and the title override renders", () => {
    render(
      <CreateCredentialForm
        fixedRuntime="claude-code"
        title="Attach an Anthropic API key"
        teamId=""
        teamReady
      />,
    );
    const runtime = screen.getByTestId("creds-runtime") as HTMLSelectElement;
    expect(runtime.disabled).toBe(true);
    expect(runtime.value).toBe("claude-code");
    expect(runtime.options).toHaveLength(1); // no other runtime is offered
    expect(screen.getByTestId("creds-create-form").textContent).toContain(
      "Attach an Anthropic API key",
    );
  });

  it("unpinned: the runtime dropdown offers the full set with claude-code default", () => {
    render(<CreateCredentialForm teamId="" teamReady />);
    const runtime = screen.getByTestId("creds-runtime") as HTMLSelectElement;
    expect(runtime.disabled).toBe(false);
    expect(runtime.value).toBe("claude-code"); // first option — screen contract holds
    expect(runtime.options.length).toBeGreaterThan(1);
  });

  it("pinned submit writes runtime claude-code through the same body contract", async () => {
    const create = vi
      .fn()
      .mockResolvedValue(jsonResponse(201, { secretRef: "ns/team-anthropic" }));
    render(
      <CreateCredentialForm
        fixedRuntime="claude-code"
        create={create}
        teamId="t-1"
        teamReady
      />,
    );
    fireEvent.change(screen.getByTestId("creds-name"), {
      target: { value: "team-anthropic" },
    });
    fireEvent.change(screen.getByTestId("creds-value"), {
      target: { value: "sk-ant-test" },
    });
    fireEvent.submit(screen.getByTestId("creds-create-form"));
    await waitFor(() =>
      expect(
        screen.getByTestId("creds-create-msg").getAttribute("data-tone"),
      ).toBe("ok"),
    );
    expect(create).toHaveBeenCalledWith({
      name: "team-anthropic",
      runtime: "claude-code",
      class: "service-account",
      value: "sk-ant-test",
      teamId: "t-1",
    });
    // Write-only discipline: the pasted key never lingers in state after the write.
    expect((screen.getByTestId("creds-value") as HTMLInputElement).value).toBe(
      "",
    );
  });

  it("after a pinned create, Test connection probes the stored claude-code credential", async () => {
    const create = vi
      .fn()
      .mockResolvedValue(jsonResponse(201, { secretRef: "ns/x" }));
    const test = vi
      .fn()
      .mockResolvedValue(
        jsonResponse(200, { ok: true, detail: "anthropic 200" }),
      );
    render(
      <CreateCredentialForm
        fixedRuntime="claude-code"
        create={create}
        test={test}
        teamId=""
        teamReady
      />,
    );
    fireEvent.change(screen.getByTestId("creds-name"), {
      target: { value: "k" },
    });
    fireEvent.change(screen.getByTestId("creds-value"), {
      target: { value: "v" },
    });
    fireEvent.submit(screen.getByTestId("creds-create-form"));
    await waitFor(() => expect(screen.getByTestId("creds-test")).toBeTruthy());
    fireEvent.click(screen.getByTestId("creds-test"));
    await waitFor(() =>
      expect(
        screen.getByTestId("creds-test-msg").getAttribute("data-tone"),
      ).toBe("ok"),
    );
    expect(test).toHaveBeenCalledWith("k", {
      runtime: "claude-code",
      teamId: undefined,
    });
  });
});
