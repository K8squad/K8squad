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
  fallbackModel = "",
  fallbackModelEndpointRef = "",
  errors = {},
  onApplyToAll,
}: {
  model?: string;
  modelEndpointRef?: string;
  byoEnabled?: boolean;
  fallbackModel?: string;
  fallbackModelEndpointRef?: string;
  errors?: FieldErrors;
  onApplyToAll?: (d: {
    model: string;
    modelEndpointRef: string;
    fallbackModel: string;
    fallbackModelEndpointRef: string;
  }) => void;
}) {
  const [f, setF] = useState({ model, modelEndpointRef, byoEnabled, fallbackModel, fallbackModelEndpointRef });
  return (
    <ModelSelector
      model={f.model}
      modelEndpointRef={f.modelEndpointRef}
      byoEnabled={f.byoEnabled}
      fallbackModel={f.fallbackModel}
      fallbackModelEndpointRef={f.fallbackModelEndpointRef}
      errors={errors}
      patch={(p) => setF((prev) => ({ ...prev, ...p }))}
      onApplyToAll={onApplyToAll}
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

describe("<ModelSelector> — ISI-3681 E3-S3 fallback extension", () => {
  it("AC2: the fallback control reveals a model input that writes fallbackModel", () => {
    render(<Harness model="claude-opus-4-8" />);
    const toggle = screen.getByRole("button", { name: /add a fallback model/i });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);
    const fb = screen.getByLabelText("Fallback model id") as HTMLInputElement;
    fireEvent.change(fb, { target: { value: "ollama/llama3.1:8b" } });
    expect((screen.getByLabelText("Fallback model id") as HTMLInputElement).value).toBe("ollama/llama3.1:8b");
  });

  it("AC2: a saved fallbackModel hydrates the fallback section open (edit mode)", () => {
    render(<Harness model="claude-opus-4-8" fallbackModel="claude-haiku-4-5" />);
    expect(screen.getByLabelText("Fallback model id")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /fallback model/i })).toHaveAttribute("aria-expanded", "true");
  });

  it("clears both fallback fields when the fallback is turned off", async () => {
    render(<Harness model="claude-opus-4-8" fallbackModel="claude-haiku-4-5" />);
    await screen.findByLabelText("Fallback model id");
    fireEvent.click(screen.getByRole("button", { name: /fallback model/i }));
    await waitFor(() => expect(screen.queryByLabelText("Fallback model id")).not.toBeInTheDocument());
  });

  it("AC3: renders advisory trigger chips that toggle (presentational only)", () => {
    render(<Harness model="claude-opus-4-8" fallbackModel="claude-haiku-4-5" />);
    const rateLimit = screen.getByRole("button", { name: "on rate-limit" });
    // rate-limit defaults on (the signal the runtime actually switches on today).
    expect(rateLimit).toHaveAttribute("aria-pressed", "true");
    const onError = screen.getByRole("button", { name: "on error" });
    expect(onError).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(onError);
    expect(screen.getByRole("button", { name: "on error" })).toHaveAttribute("aria-pressed", "true");
  });

  it("AC2/FR-4.1: warns when the fallback is the same provider as the primary", () => {
    render(<Harness model="claude-opus-4-8" fallbackModel="claude-haiku-4-5" />);
    // Both Claude → same-provider resilience warning.
    expect(screen.getByRole("alert")).toHaveTextContent(/same provider/i);
  });

  it("FR-4.1: no same-provider warning when providers differ", () => {
    render(<Harness model="claude-opus-4-8" fallbackModel="ollama/llama3.1:8b" />);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("AC4/FR-4.4: the apply-to-all shortcut is shown only when wired, and emits the defaults", () => {
    // Absent callback → no shortcut.
    const { unmount } = render(<Harness model="claude-opus-4-8" />);
    expect(screen.queryByRole("button", { name: /apply this default to all agents/i })).toBeNull();
    unmount();

    const calls: Array<{ model: string; fallbackModel: string }> = [];
    render(
      <Harness
        model="claude-opus-4-8"
        fallbackModel="claude-haiku-4-5"
        onApplyToAll={(d) => calls.push(d)}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /apply this default to all agents/i }));
    expect(calls).toEqual([
      { model: "claude-opus-4-8", modelEndpointRef: "", fallbackModel: "claude-haiku-4-5", fallbackModelEndpointRef: "" },
    ]);
  });
});
