"use client";

// components/compose/ProviderModelPicker.tsx — the backend-driven model picker for the
// opencode adapter (ISI-5006 S3, child of ISI-4989; the ISI-4890 AC6 fast-follow the
// `opencodeSlot` seam was designed for).
//
// The board's core ask: on the LLM Settings surface the model dropdown populates FROM THE
// SELECTED BACKEND, and Save creates the right endpoint Secret automatically — no more
// hand-referencing a pre-existing Secret.
//
//   1. Backend dropdown — Ollama · Z.ai · DeepSeek · Kimi · OpenAI-compatible (LLM_PROVIDER_OPTIONS,
//      ids validated by the apiserver registry).
//   2. Conditional credential field — ollama → URL (default http://localhost:11434); zai/deepseek/
//      kimi → masked API key; openai-compatible → base URL + optional key.
//   3. "List models" → POST /api/modelendpoints/list-models → populate the model dropdown, with
//      HONEST empty/error states (unreachable URL, bad key/401, no models). The apiserver — never
//      the browser — dials the provider, so the key stays server-side and a LAN Ollama is reachable.
//   4. Save flow — "Save endpoint" → POST /api/modelendpoints upserts the Secret, then we write the
//      chosen {model, modelEndpointRef} back into the ModelConfig form via `onChange`; the outer
//      "Save org default" persists the ModelConfig triple through the EXISTING compose path. No
//      wire/CRD change.
//
// Advanced escape hatch: what used to be a free-text "reference an existing Secret" is now a dropdown
// fed by GET /api/modelendpoints (`existingEndpoints`) — pick a saved endpoint + type the model id.
//
// This component is self-contained (all provider/credential state is local); it communicates with the
// parent ModelConfig form ONLY through the current {model, modelEndpointRef} it reflects and the
// `onChange` it emits. The same component fronts the fallback row (a second instance targeting the
// fallback fields) — hence the generic {model, modelEndpointRef, onChange} API.

import { useMemo, useState } from "react";
import { Field } from "./fields";
import {
  LLM_PROVIDER_OPTIONS,
  llmProviderById,
  parseSecretRef,
  type CreateEndpointRequest,
  type FieldErrors,
  type ListedModel,
  type ListModelsRequest,
  type ModelEndpointRow,
} from "@/lib/compose";

/** Sentinel <select> value for the "type it in" model escape hatch. */
const CUSTOM_MODEL = "__custom_model__";

/**
 * Seed the picker's local provider/URL from the bound endpoint on first render
 * (ISI-5302). The ModelConfig persists only the model triple, so on refresh the
 * saved opencode selection must be recovered from `modelEndpointRef` against the
 * saved endpoints (GET /api/modelendpoints) — otherwise the picker opens on the
 * default backend (Ollama) and the admin's choice looks lost. Unknown/absent ref ⇒
 * the first provider's defaults, exactly as a fresh picker starts.
 */
function seedFromProps(
  modelEndpointRef: string,
  existingEndpoints: readonly ModelEndpointRow[],
): { provider: string; url: string } {
  const name = parseSecretRef(modelEndpointRef).name;
  const ep = name ? existingEndpoints.find((e) => e.name === name) : undefined;
  const known = ep && LLM_PROVIDER_OPTIONS.some((p) => p.id === ep.provider) ? ep : undefined;
  if (!known) {
    return { provider: LLM_PROVIDER_OPTIONS[0].id, url: LLM_PROVIDER_OPTIONS[0].defaultUrl ?? "" };
  }
  return { provider: known.provider, url: known.url || (llmProviderById(known.provider)?.defaultUrl ?? "") };
}

type FetchState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "ok"; count: number }
  | { kind: "empty" }
  | { kind: "error"; message: string };

type CreateState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "ok"; name: string; operation: string }
  | { kind: "error"; message: string };

/** Pull the apiserver 422 `{fields:[{field,message}]}` into a keyed map. */
function fieldErrorsFrom(body: unknown): FieldErrors {
  const out: FieldErrors = {};
  const fields = (body as { fields?: Array<{ field?: string; message?: string }> } | null)?.fields;
  if (Array.isArray(fields)) for (const f of fields) if (f?.field) out[f.field] = f.message ?? "invalid";
  return out;
}

/** A single top-level error message from a JSON error body, if any. */
function messageFrom(body: unknown, fallback: string): string {
  const m = (body as { error?: string; message?: string } | null) ?? null;
  return m?.error || m?.message || fallback;
}

export function ProviderModelPicker({
  label = "Model",
  model,
  modelEndpointRef,
  existingEndpoints,
  errors,
  onChange,
  idPrefix = "opencode",
}: {
  /** Field label (e.g. "Model" for primary, "Fallback model" for the fallback row). */
  label?: string;
  /** The current model id this picker reflects (from the parent form field). */
  model: string;
  /** The current endpoint Secret ref this picker reflects (name of the auto-created Secret). */
  modelEndpointRef: string;
  /** Saved endpoints for the advanced escape hatch (GET /api/modelendpoints). */
  existingEndpoints: readonly ModelEndpointRow[];
  /** Keyed field errors surfaced from the outer form / a server 422. */
  errors: FieldErrors;
  /** Emits the chosen model and/or endpoint ref back to the parent ModelConfig form. */
  onChange: (next: { model?: string; modelEndpointRef?: string }) => void;
  /** Namespaces DOM ids so primary + fallback instances don't collide. */
  idPrefix?: string;
}) {
  // Seed provider/URL from the bound endpoint so a saved opencode selection survives
  // refresh (ISI-5302). useState initializers run once; the parent only mounts this
  // picker after the form is hydrated, so the props are stable at first render.
  const seed = useMemo(() => seedFromProps(modelEndpointRef, existingEndpoints), []); // eslint-disable-line react-hooks/exhaustive-deps
  const [provider, setProvider] = useState<string>(seed.provider);
  const spec = useMemo(() => llmProviderById(provider), [provider]);
  const [url, setUrl] = useState<string>(seed.url);
  const [apiKey, setApiKey] = useState<string>("");
  const [name, setName] = useState<string>("");
  // Seed the dropdown with the saved model so it renders selected before the admin
  // re-runs "List models" (ISI-5302); a live list replaces this on fetch.
  const [models, setModels] = useState<ListedModel[]>(() => (model.trim() ? [{ id: model.trim() }] : []));
  const [fetchState, setFetchState] = useState<FetchState>({ kind: "idle" });
  const [createState, setCreateState] = useState<CreateState>({ kind: "idle" });
  const [customModel, setCustomModel] = useState<boolean>(false);

  function onProviderChange(next: string) {
    setProvider(next);
    const s = llmProviderById(next);
    setUrl(s?.defaultUrl ?? "");
    setApiKey("");
    setModels([]);
    setFetchState({ kind: "idle" });
    setCreateState({ kind: "idle" });
  }

  // Credential completeness for the fetch/save affordances (mirrors the apiserver resolveTarget:
  // url-mode needs a URL; apiKey-mode needs a key).
  const hasCredential = spec?.mode === "url" ? url.trim() !== "" : apiKey.trim() !== "";

  async function onListModels() {
    if (!spec) return;
    setFetchState({ kind: "loading" });
    setModels([]);
    const req: ListModelsRequest = {
      provider,
      ...(spec.mode === "url" ? { url: url.trim() } : {}),
      ...(apiKey.trim() ? { apiKey: apiKey.trim() } : {}),
    };
    try {
      const res = await fetch("/api/modelendpoints/list-models", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(req),
      });
      const body = await res.json().catch(() => null);
      if (res.ok) {
        const list: ListedModel[] = Array.isArray((body as { models?: ListedModel[] })?.models)
          ? (body as { models: ListedModel[] }).models
          : [];
        setModels(list);
        setFetchState(list.length ? { kind: "ok", count: list.length } : { kind: "empty" });
        return;
      }
      if (res.status === 422) {
        const fe = fieldErrorsFrom(body);
        setFetchState({ kind: "error", message: fe.url || fe.apiKey || fe.provider || "Check the endpoint and credential." });
        return;
      }
      if (res.status === 401 || res.status === 403) {
        setFetchState({ kind: "error", message: "Not authorized to list models (admin only)." });
        return;
      }
      setFetchState({
        kind: "error",
        message: messageFrom(body, "Could not reach the provider — check the URL/key and try again."),
      });
    } catch {
      setFetchState({ kind: "error", message: "Network error while listing models — try again." });
    }
  }

  async function onSaveEndpoint() {
    if (!spec) return;
    setCreateState({ kind: "saving" });
    const req: CreateEndpointRequest = {
      provider,
      ...(name.trim() ? { name: name.trim() } : {}),
      ...(spec.mode === "url" ? { url: url.trim() } : {}),
      ...(apiKey.trim() ? { apiKey: apiKey.trim() } : {}),
    };
    try {
      const res = await fetch("/api/modelendpoints", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(req),
      });
      const body = await res.json().catch(() => null);
      if (res.ok) {
        const savedName = (body as { name?: string })?.name || name.trim() || provider;
        const operation = (body as { operation?: string })?.operation || "saved";
        setCreateState({ kind: "ok", name: savedName, operation });
        // Bind the auto-created Secret into the ModelConfig form.
        onChange({ modelEndpointRef: savedName });
        return;
      }
      if (res.status === 422) {
        const fe = fieldErrorsFrom(body);
        setCreateState({
          kind: "error",
          message: fe.name || fe.url || fe.apiKey || fe.provider || "Check the endpoint fields.",
        });
        return;
      }
      if (res.status === 401 || res.status === 403) {
        setCreateState({ kind: "error", message: "Not authorized to create endpoints (admin only)." });
        return;
      }
      if (res.status === 501) {
        setCreateState({ kind: "error", message: "Endpoint creation isn’t available on this cluster yet." });
        return;
      }
      setCreateState({ kind: "error", message: messageFrom(body, "Could not save the endpoint — try again.") });
    } catch {
      setCreateState({ kind: "error", message: "Network error while saving the endpoint — try again." });
    }
  }

  const modelSelectValue = customModel ? CUSTOM_MODEL : model.trim() && models.some((m) => m.id === model.trim()) ? model.trim() : "";

  function onSelectModel(next: string) {
    if (next === CUSTOM_MODEL) {
      setCustomModel(true);
      return;
    }
    setCustomModel(false);
    onChange({ model: next });
  }

  const credField = spec?.mode === "url"
    ? `${idPrefix}-url`
    : `${idPrefix}-key`;

  return (
    <div className="provider-picker" data-testid={`provider-picker-${idPrefix}`}>
      <Field
        label="Backend"
        hint="Pick the model backend — the model list and Secret are created from your choice."
      >
        <select
          value={provider}
          onChange={(e) => onProviderChange(e.target.value)}
          aria-label={`${label} backend`}
          data-testid={`${idPrefix}-provider`}
        >
          {LLM_PROVIDER_OPTIONS.map((p) => (
            <option key={p.id} value={p.id}>
              {p.label}
            </option>
          ))}
        </select>
      </Field>

      {/* Conditional credential. url-mode → URL (openai-compatible also takes an optional key);
          apiKey-mode → masked key. */}
      {spec?.mode === "url" && (
        <Field
          label={provider === "ollama" ? "Ollama URL" : "Base URL"}
          hint={provider === "ollama" ? "e.g. http://localhost:11434 (LAN URLs are dialed from the cluster)." : "The provider’s OpenAI-compatible base URL."}
          error={errors[credField]}
        >
          <input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            aria-label={`${label} endpoint URL`}
            data-testid={`${idPrefix}-url`}
            placeholder="http://localhost:11434"
            inputMode="url"
          />
        </Field>
      )}

      {(spec?.mode === "apiKey" || spec?.allowKey) && (
        <Field
          label={spec?.allowKey ? "API key (optional)" : "API key"}
          hint="Sent once to the server to list models and stored in the endpoint Secret — never shown again."
          error={errors[`${idPrefix}-key`]}
        >
          <input
            type="password"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            aria-label={`${label} API key`}
            data-testid={`${idPrefix}-key`}
            placeholder="sk-…"
            autoComplete="off"
          />
        </Field>
      )}

      <div className="provider-picker__actions">
        <button
          type="button"
          className="btn"
          onClick={onListModels}
          disabled={!hasCredential || fetchState.kind === "loading"}
          aria-disabled={!hasCredential || fetchState.kind === "loading"}
          data-testid={`${idPrefix}-list`}
        >
          {fetchState.kind === "loading" ? "Listing…" : "List models"}
        </button>
        {fetchState.kind === "ok" && (
          <span className="state state--ok" role="status" data-testid={`${idPrefix}-list-ok`}>
            {fetchState.count} model{fetchState.count === 1 ? "" : "s"} found.
          </span>
        )}
        {fetchState.kind === "empty" && (
          <span className="state" role="status" data-testid={`${idPrefix}-list-empty`}>
            No models returned by this backend.
          </span>
        )}
        {fetchState.kind === "error" && (
          <span className="state state--error" role="alert" data-testid={`${idPrefix}-list-error`}>
            {fetchState.message}
          </span>
        )}
      </div>

      {/* Model dropdown — populated from the live list; a "type it in" escape hatch always works. */}
      <Field
        label={label}
        hint={customModel ? "Any model id — written verbatim." : "Pick from the listed models, or “Type a model id…”."}
        error={errors[`${idPrefix}-model`] || errors["model"]}
      >
        {customModel ? (
          <input
            value={model}
            onChange={(e) => onChange({ model: e.target.value })}
            aria-label={`${label} id`}
            data-testid={`${idPrefix}-model-custom`}
            placeholder="e.g. llama3.1:8b"
            autoFocus
          />
        ) : (
          <select
            value={modelSelectValue}
            onChange={(e) => onSelectModel(e.target.value)}
            aria-label={label}
            data-testid={`${idPrefix}-model`}
          >
            <option value="">{models.length ? "— Select a model —" : "— List models, or type an id —"}</option>
            {models.map((m) => (
              <option key={m.id} value={m.id}>
                {m.label ? `${m.label} (${m.id})` : m.id}
              </option>
            ))}
            <option value={CUSTOM_MODEL}>Type a model id…</option>
          </select>
        )}
      </Field>
      {customModel && (
        <button
          type="button"
          className="btn btn--ghost"
          onClick={() => {
            setCustomModel(false);
            onChange({ model: "" });
          }}
          data-testid={`${idPrefix}-model-back`}
        >
          ‹ Listed models
        </button>
      )}

      {/* Optional custom Secret name (defaults to the provider id server-side). */}
      <Field label="Endpoint name (optional)" hint="Secret name for this endpoint — defaults to the backend id." error={errors[`${idPrefix}-name`]}>
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          aria-label={`${label} endpoint name`}
          data-testid={`${idPrefix}-name`}
          placeholder={provider}
        />
      </Field>

      <div className="provider-picker__actions">
        <button
          type="button"
          className="btn btn--primary"
          onClick={onSaveEndpoint}
          disabled={!hasCredential || !model.trim() || createState.kind === "saving"}
          aria-disabled={!hasCredential || !model.trim() || createState.kind === "saving"}
          data-testid={`${idPrefix}-save-endpoint`}
        >
          {createState.kind === "saving" ? "Saving endpoint…" : "Save endpoint"}
        </button>
        {createState.kind === "ok" && (
          <span className="state state--ok" role="status" data-testid={`${idPrefix}-save-ok`}>
            Endpoint “{createState.name}” {createState.operation}. It will be bound on Save.
          </span>
        )}
        {createState.kind === "error" && (
          <span className="state state--error" role="alert" data-testid={`${idPrefix}-save-error`}>
            {createState.message}
          </span>
        )}
      </div>

      {modelEndpointRef.trim() && (
        <p className="muted provider-picker__bound" data-testid={`${idPrefix}-bound`}>
          Bound endpoint: <code>{modelEndpointRef}</code>
        </p>
      )}

      {/* Advanced escape hatch: reference a previously-created endpoint (was free text; now a
          dropdown fed by GET /api/modelendpoints). The model id is typed in this mode. */}
      {existingEndpoints.length > 0 && (
        <details className="provider-picker__advanced">
          <summary>Use an existing endpoint instead</summary>
          <Field label="Existing endpoint" hint="Bind a Secret you already created.">
            <select
              value={existingEndpoints.some((e) => e.name === modelEndpointRef.trim()) ? modelEndpointRef.trim() : ""}
              onChange={(e) => onChange({ modelEndpointRef: e.target.value })}
              aria-label={`${label} existing endpoint`}
              data-testid={`${idPrefix}-existing`}
            >
              <option value="">— Select an endpoint —</option>
              {existingEndpoints.map((ep) => (
                <option key={ep.name} value={ep.name}>
                  {ep.name} ({ep.provider}
                  {ep.hasToken ? ", key" : ""})
                </option>
              ))}
            </select>
          </Field>
        </details>
      )}
    </div>
  );
}
