// test/compose/ModelSelector.test.tsx — the guided model picker at the component boundary
// (Story B, ISI-3555; extended E3-S3, ISI-3681). Verifies AC1 (curated one-click), AC2 (Custom
// escape hatch + edit hydration), AC3/AC4 (BYO select-existing-Secret + toggle hydration), AC8
// (a11y wiring), and the E3-S3 fallback control (fallback model + endpoint, advisory trigger chips,
// same-provider warning, apply-to-all affordance).

import { describe, it, expect, afterEach, vi } from "vitest";
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
  fallbackTriggers = [],
  errors = {},
  onApplyToAll,
}: {
  model?: string;
  modelEndpointRef?: string;
  byoEnabled?: boolean;
  fallbackModel?: string;
  fallbackModelEndpointRef?: string;
  fallbackTriggers?: string[];
  errors?: FieldErrors;
  onApplyToAll?: () => void;
}) {
  const [f, setF] = useState({
    model,
    modelEndpointRef,
    byoEnabled,
    fallbackModel,
    fallbackModelEndpointRef,
    fallbackTriggers,
  });
  return (
    <ModelSelector
      model={f.model}
      modelEndpointRef={f.modelEndpointRef}
      byoEnabled={f.byoEnabled}
      fallbackModel={f.fallbackModel}
      fallbackModelEndpointRef={f.fallbackModelEndpointRef}
      fallbackTriggers={f.fallbackTriggers}
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

describe("<ModelSelector> — E3-S3 fallback control (ISI-3681)", () => {
  it("AC2: the fallback toggle is accessible and reveals curated fallback picker", () => {
    render(<Harness />);
    const toggle = screen.getByRole("button", { name: /add a fallback model/i });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAttribute("aria-controls");
    fireEvent.click(toggle);
    const fb = screen.getByLabelText("Fallback model") as HTMLSelectElement;
    // The curated ids are offered for the fallback picker (scoped to it, since the primary picker
    // renders the same option labels).
    const fbOptionLabels = Array.from(fb.options).map((o) => o.textContent);
    for (const h of CURATED_MODELS) {
      expect(fbOptionLabels).toContain(h.label);
    }
    fireEvent.change(fb, { target: { value: "claude-haiku-4-5" } });
    expect((screen.getByLabelText("Fallback model") as HTMLSelectElement).value).toBe("claude-haiku-4-5");
  });

  it("AC2: a bound fallbackModel hydrates the section open (curated in the list)", async () => {
    render(<Harness fallbackModel="claude-haiku-4-5" />);
    const fb = (await screen.findByLabelText("Fallback model")) as HTMLSelectElement;
    expect(fb.value).toBe("claude-haiku-4-5");
  });

  it("AC2: a non-curated fallback opens in Custom, pre-filled; endpoint ref rides through", async () => {
    render(<Harness fallbackModel="ollama/llama3.1:8b" fallbackModelEndpointRef="fb-endpoint" />);
    const custom = (await screen.findByLabelText("Custom fallback model id")) as HTMLInputElement;
    expect(custom.value).toBe("ollama/llama3.1:8b");
    expect((screen.getByLabelText("Fallback endpoint Secret ref") as HTMLInputElement).value).toBe("fb-endpoint");
  });

  it("clears the fallback when the section is toggled off (no half-filled fallback)", async () => {
    render(<Harness fallbackModel="claude-haiku-4-5" />);
    await screen.findByLabelText("Fallback model");
    fireEvent.click(screen.getByRole("button", { name: /fallback model/i }));
    await waitFor(() => expect(screen.queryByLabelText("Fallback model")).not.toBeInTheDocument());
  });

  it("AC2 (FR-4.1/4.2): warns when primary and fallback share a provider", () => {
    render(<Harness model="claude-opus-4-8" fallbackModel="claude-haiku-4-5" />);
    expect(screen.getByRole("status")).toHaveTextContent(/same provider \(anthropic\)/i);
  });

  it("AC2: no same-provider warning when providers differ", () => {
    render(<Harness model="claude-opus-4-8" fallbackModel="ollama/llama3.1:8b" />);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("AC3: trigger chips are toggleable and advisory (aria-pressed reflects state)", async () => {
    render(<Harness fallbackModel="claude-haiku-4-5" />);
    await screen.findByLabelText("Fallback model");
    const chip = screen.getByRole("button", { name: "on rate-limit" });
    expect(chip).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(chip);
    expect(screen.getByRole("button", { name: "on rate-limit" })).toHaveAttribute("aria-pressed", "true");
  });

  it("AC3: renders the labelled v2 multi-fallback placeholder", async () => {
    render(<Harness fallbackModel="claude-haiku-4-5" />);
    expect(await screen.findByText(/ordered multi-fallback list — coming soon \(v2\)/i)).toBeInTheDocument();
  });

  it("AC4: apply-to-all is disabled (coming soon) without a handler", () => {
    render(<Harness />);
    expect(screen.getByRole("button", { name: /apply to all agents/i })).toBeDisabled();
  });

  it("AC4: apply-to-all fires the handler when a squad-aware parent wires it", () => {
    const onApplyToAll = vi.fn();
    render(<Harness onApplyToAll={onApplyToAll} />);
    const btn = screen.getByRole("button", { name: /apply to all agents/i });
    expect(btn).toBeEnabled();
    fireEvent.click(btn);
    expect(onApplyToAll).toHaveBeenCalledOnce();
  });
});
