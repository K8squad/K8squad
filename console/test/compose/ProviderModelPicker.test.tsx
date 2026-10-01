// test/compose/ProviderModelPicker.test.tsx — the backend-driven opencode picker at the
// component boundary (ISI-5006 S3, child of ISI-4989). Verifies the board's core ask: pick a
// backend → conditional credential → list live models → pick → Save auto-creates the endpoint
// Secret and binds {model, modelEndpointRef} back into the form. Honest empty/error states.

import { describe, it, expect, vi, afterEach } from "vitest";
import { useState } from "react";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import { ProviderModelPicker } from "@/components/compose/ProviderModelPicker";
import type { FieldErrors, ModelEndpointRow } from "@/lib/compose";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

/** Route the two POSTs the picker makes; capture request bodies. */
function stubEndpoints(opts: {
  list?: Response;
  create?: Response;
  onList?: (body: string) => void;
  onCreate?: (body: string) => void;
}): void {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes("/api/modelendpoints/list-models")) {
        opts.onList?.(String(init?.body));
        return Promise.resolve(opts.list ?? jsonResponse(200, { models: [] }));
      }
      if (url.includes("/api/modelendpoints")) {
        opts.onCreate?.(String(init?.body));
        return Promise.resolve(opts.create ?? jsonResponse(201, { name: "ollama", provider: "ollama", operation: "created" }));
      }
      return Promise.resolve(jsonResponse(404, {}));
    }),
  );
}

/** Stateful harness mirroring ModelPrioritySection's patch of {model, modelEndpointRef}. */
function Harness({
  existingEndpoints = [],
  errors = {},
  onChangeSpy,
  initialModel = "",
  initialRef = "",
}: {
  existingEndpoints?: ModelEndpointRow[];
  errors?: FieldErrors;
  onChangeSpy?: (n: { model?: string; modelEndpointRef?: string }) => void;
  initialModel?: string;
  initialRef?: string;
}) {
  const [f, setF] = useState({ model: initialModel, modelEndpointRef: initialRef });
  return (
    <ProviderModelPicker
      label="Model"
      idPrefix="primary"
      model={f.model}
      modelEndpointRef={f.modelEndpointRef}
      existingEndpoints={existingEndpoints}
      errors={errors}
      onChange={(n) => {
        onChangeSpy?.(n);
        setF((prev) => ({ ...prev, ...n }));
      }}
    />
  );
}

describe("<ProviderModelPicker> — ISI-5006 backend-driven model picker", () => {
  it("defaults to Ollama with the localhost URL prefilled and no API-key field", () => {
    stubEndpoints({});
    render(<Harness />);
    expect((screen.getByTestId("primary-provider") as HTMLSelectElement).value).toBe("ollama");
    expect((screen.getByTestId("primary-url") as HTMLInputElement).value).toBe("http://localhost:11434");
    expect(screen.queryByTestId("primary-key")).toBeNull();
  });

  it("switches to a masked API-key field for an apiKey-mode provider (zai)", () => {
    stubEndpoints({});
    render(<Harness />);
    fireEvent.change(screen.getByTestId("primary-provider"), { target: { value: "zai" } });
    expect(screen.queryByTestId("primary-url")).toBeNull();
    const key = screen.getByTestId("primary-key") as HTMLInputElement;
    expect(key.type).toBe("password");
  });

  it("lists live models from the selected backend and binds the pick via onChange", async () => {
    let listBody = "";
    stubEndpoints({
      list: jsonResponse(200, { models: [{ id: "llama3.1:8b" }, { id: "qwen2.5:7b" }] }),
      onList: (b) => (listBody = b),
    });
    const onChangeSpy = vi.fn();
    render(<Harness onChangeSpy={onChangeSpy} />);

    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-list-ok"));
    expect(JSON.parse(listBody)).toMatchObject({ provider: "ollama", url: "http://localhost:11434" });

    const select = screen.getByTestId("primary-model") as HTMLSelectElement;
    expect(select.disabled).toBe(false);
    fireEvent.change(select, { target: { value: "qwen2.5:7b" } });
    expect(onChangeSpy).toHaveBeenCalledWith({ model: "qwen2.5:7b" });
  });

  it("shows an honest empty state when the backend returns no models", async () => {
    stubEndpoints({ list: jsonResponse(200, { models: [] }) });
    render(<Harness />);
    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-list-empty"));
  });

  it("shows an honest error when the provider is unreachable (502)", async () => {
    stubEndpoints({ list: jsonResponse(502, { error: "could not reach the model provider" }) });
    render(<Harness />);
    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-list-error"));
    expect(screen.getByTestId("primary-list-error").textContent).toMatch(/could not reach/i);
  });

  it("surfaces a 422 field error from list-models", async () => {
    stubEndpoints({ list: jsonResponse(422, { error: "validation failed", fields: [{ field: "url", message: "must be a valid http(s) URL with a host" }] }) });
    render(<Harness />);
    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-list-error"));
    expect(screen.getByTestId("primary-list-error").textContent).toMatch(/valid http/i);
  });

  it("Save endpoint POSTs /api/modelendpoints and binds the returned Secret name", async () => {
    let createBody = "";
    stubEndpoints({
      list: jsonResponse(200, { models: [{ id: "llama3.1:8b" }] }),
      create: jsonResponse(201, { name: "ollama", provider: "ollama", operation: "created" }),
      onCreate: (b) => (createBody = b),
    });
    const onChangeSpy = vi.fn();
    render(<Harness onChangeSpy={onChangeSpy} />);

    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-model"));
    fireEvent.change(screen.getByTestId("primary-model"), { target: { value: "llama3.1:8b" } });

    const save = screen.getByTestId("primary-save-endpoint") as HTMLButtonElement;
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => screen.getByTestId("primary-save-ok"));

    expect(JSON.parse(createBody)).toMatchObject({ provider: "ollama", url: "http://localhost:11434" });
    expect(onChangeSpy).toHaveBeenCalledWith({ modelEndpointRef: "ollama" });
    expect(screen.getByTestId("primary-bound").textContent).toMatch(/ollama/);
  });

  it("keeps Save endpoint disabled until a model is chosen", async () => {
    stubEndpoints({ list: jsonResponse(200, { models: [{ id: "llama3.1:8b" }] }) });
    render(<Harness />);
    expect((screen.getByTestId("primary-save-endpoint") as HTMLButtonElement).disabled).toBe(true);
  });

  it("advanced escape hatch binds an existing endpoint from the dropdown", () => {
    stubEndpoints({});
    const onChangeSpy = vi.fn();
    render(
      <Harness
        onChangeSpy={onChangeSpy}
        existingEndpoints={[{ name: "my-ollama", provider: "ollama", url: "http://x:11434", hasToken: false }]}
      />,
    );
    fireEvent.change(screen.getByTestId("primary-existing"), { target: { value: "my-ollama" } });
    expect(onChangeSpy).toHaveBeenCalledWith({ modelEndpointRef: "my-ollama" });
  });

  // ── ISI-5302: seed local state from props so a saved opencode selection survives refresh ──

  it("seeds the url-mode backend, URL, and saved model from the bound endpoint (no re-list)", () => {
    stubEndpoints({});
    render(
      <Harness
        existingEndpoints={[{ name: "lan-ollama", provider: "ollama", url: "http://10.0.0.5:11434", hasToken: false }]}
        initialModel="llama3.1:8b"
        initialRef="lan-ollama"
      />,
    );
    expect((screen.getByTestId("primary-provider") as HTMLSelectElement).value).toBe("ollama");
    expect((screen.getByTestId("primary-url") as HTMLInputElement).value).toBe("http://10.0.0.5:11434");
    // The saved model renders as the selected option without the admin clicking "List models".
    expect((screen.getByTestId("primary-model") as HTMLSelectElement).value).toBe("llama3.1:8b");
    expect(screen.getByTestId("primary-bound").textContent).toMatch(/lan-ollama/);
  });

  it("seeds an apiKey-mode backend (deepseek) from the bound endpoint", () => {
    stubEndpoints({});
    render(
      <Harness
        existingEndpoints={[{ name: "my-deepseek", provider: "deepseek", url: "", hasToken: true }]}
        initialModel="deepseek-chat"
        initialRef="my-deepseek"
      />,
    );
    expect((screen.getByTestId("primary-provider") as HTMLSelectElement).value).toBe("deepseek");
    expect(screen.queryByTestId("primary-url")).toBeNull(); // apiKey mode — no URL field
    expect(screen.getByTestId("primary-key")).not.toBeNull();
    expect((screen.getByTestId("primary-model") as HTMLSelectElement).value).toBe("deepseek-chat");
  });

  it("falls back to the default backend when the ref is not a known opencode endpoint", () => {
    stubEndpoints({});
    render(<Harness existingEndpoints={[]} initialModel="some-model" initialRef="ghost-endpoint" />);
    expect((screen.getByTestId("primary-provider") as HTMLSelectElement).value).toBe("ollama");
    expect((screen.getByTestId("primary-url") as HTMLInputElement).value).toBe("http://localhost:11434");
    // The saved model id still seeds so it's visible even when the provider can't be resolved.
    expect((screen.getByTestId("primary-model") as HTMLSelectElement).value).toBe("some-model");
  });

  it("supports the type-it-in model escape hatch", () => {
    stubEndpoints({});
    const onChangeSpy = vi.fn();
    render(<Harness onChangeSpy={onChangeSpy} />);
    fireEvent.change(screen.getByTestId("primary-model"), { target: { value: "__custom_model__" } });
    fireEvent.change(screen.getByTestId("primary-model-custom"), { target: { value: "custom/model:1" } });
    expect(onChangeSpy).toHaveBeenCalledWith({ model: "custom/model:1" });
  });
});
