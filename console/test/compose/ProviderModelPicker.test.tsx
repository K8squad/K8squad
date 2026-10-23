// test/compose/ProviderModelPicker.test.tsx — the backend-driven opencode picker at the
// component boundary (ISI-5006 S3, child of ISI-4989). Verifies the board's core ask: pick a
// backend → conditional credential → list live models → pick → Save auto-creates the endpoint
// Secret and binds {model, modelEndpointRef} back into the form. Honest empty/error states.

import { describe, it, expect, vi, afterEach } from "vitest";
import { useState } from "react";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { ProviderModelPicker } from "@/components/compose/ProviderModelPicker";
import type {
  FieldErrors,
  LlmProviderId,
  ModelEndpointRow,
} from "@/lib/compose";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
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
        return Promise.resolve(
          opts.create ??
            jsonResponse(201, {
              name: "ollama",
              provider: "ollama",
              operation: "created",
            }),
        );
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
  fixedProvider,
}: {
  existingEndpoints?: ModelEndpointRow[];
  errors?: FieldErrors;
  onChangeSpy?: (n: { model?: string; modelEndpointRef?: string }) => void;
  initialModel?: string;
  initialRef?: string;
  fixedProvider?: LlmProviderId;
}) {
  const [f, setF] = useState({
    model: initialModel,
    modelEndpointRef: initialRef,
  });
  return (
    <ProviderModelPicker
      label="Model"
      idPrefix="primary"
      fixedProvider={fixedProvider}
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
    expect(
      (screen.getByTestId("primary-provider") as HTMLSelectElement).value,
    ).toBe("ollama");
    expect((screen.getByTestId("primary-url") as HTMLInputElement).value).toBe(
      "http://localhost:11434",
    );
    expect(screen.queryByTestId("primary-key")).toBeNull();
  });

  it("switches to a masked API-key field for an apiKey-mode provider (zai)", () => {
    stubEndpoints({});
    render(<Harness />);
    fireEvent.change(screen.getByTestId("primary-provider"), {
      target: { value: "zai" },
    });
    expect(screen.queryByTestId("primary-url")).toBeNull();
    const key = screen.getByTestId("primary-key") as HTMLInputElement;
    expect(key.type).toBe("password");
  });

  it("lists live models from the selected backend and binds the pick via onChange", async () => {
    let listBody = "";
    stubEndpoints({
      list: jsonResponse(200, {
        models: [{ id: "llama3.1:8b" }, { id: "qwen2.5:7b" }],
      }),
      onList: (b) => (listBody = b),
    });
    const onChangeSpy = vi.fn();
    render(<Harness onChangeSpy={onChangeSpy} />);

    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-list-ok"));
    expect(JSON.parse(listBody)).toMatchObject({
      provider: "ollama",
      url: "http://localhost:11434",
    });

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
    stubEndpoints({
      list: jsonResponse(502, { error: "could not reach the model provider" }),
    });
    render(<Harness />);
    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-list-error"));
    expect(screen.getByTestId("primary-list-error").textContent).toMatch(
      /could not reach/i,
    );
  });

  it("surfaces a 422 field error from list-models", async () => {
    stubEndpoints({
      list: jsonResponse(422, {
        error: "validation failed",
        fields: [
          { field: "url", message: "must be a valid http(s) URL with a host" },
        ],
      }),
    });
    render(<Harness />);
    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-list-error"));
    expect(screen.getByTestId("primary-list-error").textContent).toMatch(
      /valid http/i,
    );
  });

  it("Save endpoint POSTs /api/modelendpoints and binds the returned Secret name", async () => {
    let createBody = "";
    stubEndpoints({
      list: jsonResponse(200, { models: [{ id: "llama3.1:8b" }] }),
      create: jsonResponse(201, {
        name: "ollama",
        provider: "ollama",
        operation: "created",
      }),
      onCreate: (b) => (createBody = b),
    });
    const onChangeSpy = vi.fn();
    render(<Harness onChangeSpy={onChangeSpy} />);

    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-model"));
    fireEvent.change(screen.getByTestId("primary-model"), {
      target: { value: "llama3.1:8b" },
    });

    const save = screen.getByTestId(
      "primary-save-endpoint",
    ) as HTMLButtonElement;
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => screen.getByTestId("primary-save-ok"));

    expect(JSON.parse(createBody)).toMatchObject({
      provider: "ollama",
      url: "http://localhost:11434",
    });
    expect(onChangeSpy).toHaveBeenCalledWith({ modelEndpointRef: "ollama" });
    expect(screen.getByTestId("primary-bound").textContent).toMatch(/ollama/);
  });

  it("keeps Save endpoint disabled until a model is chosen", async () => {
    stubEndpoints({
      list: jsonResponse(200, { models: [{ id: "llama3.1:8b" }] }),
    });
    render(<Harness />);
    expect(
      (screen.getByTestId("primary-save-endpoint") as HTMLButtonElement)
        .disabled,
    ).toBe(true);
  });

  it("advanced escape hatch binds an existing endpoint from the dropdown", () => {
    stubEndpoints({});
    const onChangeSpy = vi.fn();
    render(
      <Harness
        onChangeSpy={onChangeSpy}
        existingEndpoints={[
          {
            name: "my-ollama",
            provider: "ollama",
            url: "http://x:11434",
            hasToken: false,
          },
        ]}
      />,
    );
    fireEvent.change(screen.getByTestId("primary-existing"), {
      target: { value: "my-ollama" },
    });
    expect(onChangeSpy).toHaveBeenCalledWith({ modelEndpointRef: "my-ollama" });
  });

  // ── ISI-5302: seed local state from props so a saved opencode selection survives refresh ──

  it("seeds the url-mode backend, URL, and saved model from the bound endpoint (no re-list)", () => {
    stubEndpoints({});
    render(
      <Harness
        existingEndpoints={[
          {
            name: "lan-ollama",
            provider: "ollama",
            url: "http://10.0.0.5:11434",
            hasToken: false,
          },
        ]}
        initialModel="llama3.1:8b"
        initialRef="lan-ollama"
      />,
    );
    expect(
      (screen.getByTestId("primary-provider") as HTMLSelectElement).value,
    ).toBe("ollama");
    expect((screen.getByTestId("primary-url") as HTMLInputElement).value).toBe(
      "http://10.0.0.5:11434",
    );
    // The saved model renders as the selected option without the admin clicking "List models".
    expect(
      (screen.getByTestId("primary-model") as HTMLSelectElement).value,
    ).toBe("llama3.1:8b");
    expect(screen.getByTestId("primary-bound").textContent).toMatch(
      /lan-ollama/,
    );
  });

  it("seeds an apiKey-mode backend (deepseek) from the bound endpoint", () => {
    stubEndpoints({});
    render(
      <Harness
        existingEndpoints={[
          {
            name: "my-deepseek",
            provider: "deepseek",
            url: "",
            hasToken: true,
          },
        ]}
        initialModel="deepseek-chat"
        initialRef="my-deepseek"
      />,
    );
    expect(
      (screen.getByTestId("primary-provider") as HTMLSelectElement).value,
    ).toBe("deepseek");
    expect(screen.queryByTestId("primary-url")).toBeNull(); // apiKey mode — no URL field
    expect(screen.getByTestId("primary-key")).not.toBeNull();
    expect(
      (screen.getByTestId("primary-model") as HTMLSelectElement).value,
    ).toBe("deepseek-chat");
  });

  it("falls back to the default backend when the ref is not a known opencode endpoint", () => {
    stubEndpoints({});
    render(
      <Harness
        existingEndpoints={[]}
        initialModel="some-model"
        initialRef="ghost-endpoint"
      />,
    );
    expect(
      (screen.getByTestId("primary-provider") as HTMLSelectElement).value,
    ).toBe("ollama");
    expect((screen.getByTestId("primary-url") as HTMLInputElement).value).toBe(
      "http://localhost:11434",
    );
    // The saved model id still seeds so it's visible even when the provider can't be resolved.
    expect(
      (screen.getByTestId("primary-model") as HTMLSelectElement).value,
    ).toBe("some-model");
  });

  it("supports the type-it-in model escape hatch", () => {
    stubEndpoints({});
    const onChangeSpy = vi.fn();
    render(<Harness onChangeSpy={onChangeSpy} />);
    fireEvent.change(screen.getByTestId("primary-model"), {
      target: { value: "__custom_model__" },
    });
    fireEvent.change(screen.getByTestId("primary-model-custom"), {
      target: { value: "custom/model:1" },
    });
    expect(onChangeSpy).toHaveBeenCalledWith({ model: "custom/model:1" });
  });
});

// ── ISI-6202 (ISI-6158 C2): the Mistral (vibe) BYO entry ────────────────────────────────────

describe("<ProviderModelPicker> — Mistral (vibe) provider", () => {
  it("offers Mistral (vibe): masked key + an OPTIONAL base-URL field (BYO lane)", () => {
    stubEndpoints({});
    render(<Harness />);
    fireEvent.change(screen.getByTestId("primary-provider"), {
      target: { value: "mistral" },
    });
    // Masked key…
    const key = screen.getByTestId("primary-key") as HTMLInputElement;
    expect(key.type).toBe("password");
    // …plus the optional base-URL field, empty by default.
    const url = screen.getByTestId("primary-url") as HTMLInputElement;
    expect(url.value).toBe("");
    // List stays key-gated: a URL alone is not a credential for an apiKey-mode provider.
    expect(
      (screen.getByTestId("primary-list") as HTMLButtonElement).disabled,
    ).toBe(true);
    fireEvent.change(key, { target: { value: "mist-key" } });
    expect(
      (screen.getByTestId("primary-list") as HTMLButtonElement).disabled,
    ).toBe(false);
  });

  it("mistral list-models sends the key alone when no BYO URL is set, and the URL when typed", async () => {
    const bodies: string[] = [];
    // A FRESH Response per call — a Response body can only be read once, and this test lists twice.
    const listResponse = () =>
      jsonResponse(200, { models: [{ id: "mistral-large-latest" }] });
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input).includes("/api/modelendpoints/list-models")) {
          bodies.push(String(init?.body));
          return Promise.resolve(listResponse());
        }
        return Promise.resolve(jsonResponse(404, {}));
      }),
    );
    render(<Harness />);
    fireEvent.change(screen.getByTestId("primary-provider"), {
      target: { value: "mistral" },
    });
    fireEvent.change(screen.getByTestId("primary-key"), {
      target: { value: "mist-key" },
    });

    // No BYO URL typed ⇒ the request carries provider+apiKey only (the Mistral API lane).
    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-list-ok"));
    expect(JSON.parse(bodies[0])).toEqual({
      provider: "mistral",
      apiKey: "mist-key",
    });

    // BYO URL typed ⇒ it rides the same request (the MISTRAL_BASE_URL lane).
    fireEvent.change(screen.getByTestId("primary-url"), {
      target: { value: "http://ollama.lan:11434/v1" },
    });
    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-list-ok"));
    expect(JSON.parse(bodies[1])).toEqual({
      provider: "mistral",
      apiKey: "mist-key",
      url: "http://ollama.lan:11434/v1",
    });
  });

  it("mistral Save endpoint posts key + optional URL and binds the Secret", async () => {
    let createBody = "";
    stubEndpoints({
      list: jsonResponse(200, { models: [{ id: "mistral-large-latest" }] }),
      create: jsonResponse(201, {
        name: "mistral",
        provider: "mistral",
        operation: "created",
      }),
      onCreate: (b) => (createBody = b),
    });
    const onChangeSpy = vi.fn();
    render(<Harness onChangeSpy={onChangeSpy} />);
    fireEvent.change(screen.getByTestId("primary-provider"), {
      target: { value: "mistral" },
    });
    fireEvent.change(screen.getByTestId("primary-key"), {
      target: { value: "mist-key" },
    });
    fireEvent.change(screen.getByTestId("primary-url"), {
      target: { value: "http://ollama.lan:11434/v1" },
    });
    fireEvent.click(screen.getByTestId("primary-list"));
    await waitFor(() => screen.getByTestId("primary-model"));
    fireEvent.change(screen.getByTestId("primary-model"), {
      target: { value: "mistral-large-latest" },
    });
    fireEvent.click(screen.getByTestId("primary-save-endpoint"));
    await waitFor(() => screen.getByTestId("primary-save-ok"));
    expect(JSON.parse(createBody)).toEqual({
      provider: "mistral",
      apiKey: "mist-key",
      url: "http://ollama.lan:11434/v1",
    });
    expect(onChangeSpy).toHaveBeenCalledWith({ modelEndpointRef: "mistral" });
  });

  it("seeds a saved mistral BYO endpoint (provider + URL) from the bound ref — key never echoed", () => {
    stubEndpoints({});
    render(
      <Harness
        existingEndpoints={[
          {
            name: "my-mistral",
            provider: "mistral",
            url: "http://ollama.lan:11434/v1",
            hasToken: true,
          },
        ]}
        initialModel="mistral-large-latest"
        initialRef="my-mistral"
      />,
    );
    expect(
      (screen.getByTestId("primary-provider") as HTMLSelectElement).value,
    ).toBe("mistral");
    expect((screen.getByTestId("primary-url") as HTMLInputElement).value).toBe(
      "http://ollama.lan:11434/v1",
    );
    expect((screen.getByTestId("primary-key") as HTMLInputElement).value).toBe(
      "",
    );
    expect(
      (screen.getByTestId("primary-model") as HTMLSelectElement).value,
    ).toBe("mistral-large-latest");
  });

  it("pinned vibe picker keeps the saved BYO base URL (ISI-5302 holds under the pin)", () => {
    stubEndpoints({});
    render(
      <Harness
        fixedProvider="mistral"
        existingEndpoints={[
          {
            name: "my-mistral",
            provider: "mistral",
            url: "http://ollama.lan:11434/v1",
            hasToken: true,
          },
        ]}
        initialModel="mistral-large-latest"
        initialRef="my-mistral"
      />,
    );
    expect((screen.getByTestId("primary-url") as HTMLInputElement).value).toBe(
      "http://ollama.lan:11434/v1",
    );
  });

  it("pinned vibe picker's escape hatch only lists mistral endpoints, not ollama (ISI-6380)", () => {
    stubEndpoints({});
    render(
      <Harness
        fixedProvider="mistral"
        existingEndpoints={[
          {
            name: "my-mistral",
            provider: "mistral",
            url: "http://ollama.lan:11434/v1",
            hasToken: true,
          },
          {
            name: "lan-ollama",
            provider: "ollama",
            url: "http://10.0.0.5:11434",
            hasToken: false,
          },
        ]}
        initialModel=""
        initialRef=""
      />,
    );
    const existing = screen.queryByTestId(
      "primary-existing",
    ) as HTMLSelectElement | null;
    // only mistral endpoint is offered — lan-ollama must not appear
    expect(existing).not.toBeNull();
    const options = Array.from(existing!.options).map((o) => o.value);
    expect(options).toContain("my-mistral");
    expect(options).not.toContain("lan-ollama");
  });
});
