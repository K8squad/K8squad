// test/compose/ModelSelector.test.tsx — the guided model picker at the component boundary
// (Story B, ISI-3555). Verifies AC1 (curated one-click), AC2 (Custom escape hatch + edit
// hydration), AC3/AC4 (BYO select-existing-Secret + toggle hydration) and AC8 (a11y wiring).

import { describe, it, expect, afterEach } from "vitest";
import { useState } from "react";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import { ModelSelector } from "@/components/compose/ModelSelector";
import { CURATED_MODELS } from "@/lib/modelHints";
import type { FieldErrors } from "@/lib/compose";

afterEach(cleanup);

/** Stateful harness: mirrors ComposeScreen's patch() so the controlled selector round-trips. */
function Harness({
  model = "",
  modelEndpointRef = "",
  byoEnabled = false,
  errors = {},
}: {
  model?: string;
  modelEndpointRef?: string;
  byoEnabled?: boolean;
  errors?: FieldErrors;
}) {
  const [f, setF] = useState({ model, modelEndpointRef, byoEnabled });
  return (
    <ModelSelector
      model={f.model}
      modelEndpointRef={f.modelEndpointRef}
      byoEnabled={f.byoEnabled}
      errors={errors}
      patch={(p) => setF((prev) => ({ ...prev, ...p }))}
    />
  );
}

describe("<ModelSelector> — Story B ACs", () => {
  it("AC1: offers curated Claude ids; selecting one sets model verbatim", () => {
    render(<Harness />);
    const select = screen.getByLabelText("Model") as HTMLSelectElement;
    // Every curated id is offered as an option.
    for (const h of CURATED_MODELS) {
      expect(screen.getByRole("option", { name: h.label })).toBeInTheDocument();
    }
    fireEvent.change(select, { target: { value: "claude-opus-4-8" } });
    expect((screen.getByLabelText("Model") as HTMLSelectElement).value).toBe("claude-opus-4-8");
  });

  it("AC2: 'Custom model…' reveals free-text that writes model verbatim", () => {
    render(<Harness />);
    fireEvent.change(screen.getByLabelText("Model"), { target: { value: "__custom__" } });
    const custom = screen.getByLabelText("Custom model id") as HTMLInputElement;
    fireEvent.change(custom, { target: { value: "ollama/llama3.1:8b" } });
    expect((screen.getByLabelText("Custom model id") as HTMLInputElement).value).toBe("ollama/llama3.1:8b");
  });

  it("AC2: an edit-mode non-curated saved model opens in Custom, pre-filled", () => {
    render(<Harness model="my-self-hosted-model" />);
    const custom = screen.getByLabelText("Custom model id") as HTMLInputElement;
    expect(custom.value).toBe("my-self-hosted-model");
  });

  it("AC1: a curated saved model opens in the list (not Custom)", () => {
    render(<Harness model="claude-opus-4-8" />);
    const select = screen.getByLabelText("Model") as HTMLSelectElement;
    expect(select.value).toBe("claude-opus-4-8");
  });

  it("normalizes a curated model with stray whitespace to the matching option (Copilot #240)", () => {
    render(<Harness model="  claude-opus-4-8  " />);
    // Curated (trim-matched) → shown in the list at the real option value, never the placeholder.
    const select = screen.getByLabelText("Model") as HTMLSelectElement;
    expect(select.value).toBe("claude-opus-4-8");
  });

  it("keeps the 'Curated list' button OUT of the label wrapper (Copilot #240)", () => {
    render(<Harness model="my-self-hosted-model" />);
    const back = screen.getByRole("button", { name: /curated list/i });
    // Invalid markup guard: a <button> must not live inside the Field's <label>.
    expect(back.closest("label")).toBeNull();
  });

  it("AC3/AC8: BYO toggle is accessible and reveals the endpoint-Secret field", () => {
    render(<Harness />);
    const toggle = screen.getByRole("button", { name: /bring your own endpoint/i });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAttribute("aria-controls");
    fireEvent.click(toggle);
    expect(screen.getByRole("button", { name: /bring your own endpoint/i })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    const ref = screen.getByLabelText("Endpoint Secret ref") as HTMLInputElement;
    fireEvent.change(ref, { target: { value: "my-endpoint/url" } });
    expect((screen.getByLabelText("Endpoint Secret ref") as HTMLInputElement).value).toBe("my-endpoint/url");
  });

  it("AC4: a bound modelEndpointRef hydrates the BYO toggle open", async () => {
    render(<Harness modelEndpointRef="existing-endpoint" />);
    // The mount effect flips byoEnabled on for a populated ref → region appears.
    expect(await screen.findByLabelText("Endpoint Secret ref")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /bring your own endpoint/i })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
  });

  it("AC3: turning BYO off clears the endpoint ref (no stale ref rides the apply)", async () => {
    render(<Harness modelEndpointRef="existing-endpoint" />);
    // Wait for hydration to open the toggle, then turn it off.
    await screen.findByLabelText("Endpoint Secret ref");
    fireEvent.click(screen.getByRole("button", { name: /bring your own endpoint/i }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Endpoint Secret ref")).not.toBeInTheDocument(),
    );
  });
});
