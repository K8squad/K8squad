"use client";

// components/compose/ModelSelector.tsx — the guided model picker for the Agent compose form
// (Story B, ISI-3555 / feature ISI-3544; design ISI-3546 / Winston).
//
// Replaces the old unlabeled free-text Model input with three vendor-neutral paths (ADR-026):
//   1. curated Claude ids from useModelHints() — one click (AC1),
//   2. "Custom model…" free-text escape hatch — any id, verbatim (AC2, AC7),
//   3. "Bring your own endpoint" toggle — reference an EXISTING endpoint Secret, setting
//      modelEndpointRef via the unchanged compose apply (AC3). No Secret is written here: the
//      compose SA has no Secret-write RBAC by design (ISI-3546); inline URL+token creation is a
//      deferred fast-follow (AC3b) that must route through internal/apiserver/credentials.go — see
//      the note rendered under the toggle.
//
// The authoritative output is always a plain `model` string (+ `modelEndpointRef` when BYO is on);
// this component never encodes an endpoint or token into `model`. It owns only presentation state
// (curated-vs-custom mode); the form values + validation live in lib/compose + lib/modelHints.
//
// ISI-3681 E3-S3 extension (AD-4/AD-5, extends ISI-3555 in place — no parallel component): the same
// component gains a single Fallback control mapping to `Agent.spec.fallbackModel.{model,
// modelEndpointRef}` (fallbackModel/fallbackModelEndpointRef form fields), advisory trigger chips
// (on error / on rate-limit / on timeout — presentational v1, CRD has no trigger field), a
// same-provider resilience warning (FR-4.1/4.2), and an optional "apply default to all agents"
// shortcut (FR-4.4). The multi-order fallback list is a labelled v2 placeholder, not built (AC3).

import { useEffect, useState } from "react";
import { Field } from "./fields";
import { isCuratedModel, modelShapeHint, sameProvider, useModelHints } from "@/lib/modelHints";
import type { FieldErrors } from "@/lib/compose";

/** Sentinel <option> value for the "Custom model…" escape hatch (never a real model id). */
const CUSTOM = "__custom__";

/** The id of the BYO region, referenced by the toggle's aria-controls. */
const BYO_REGION_ID = "compose-byo-endpoint";

/** The id of the Fallback region, referenced by the toggle's aria-controls. */
const FALLBACK_REGION_ID = "compose-fallback-model";

/** The advisory fallback triggers (UI-only in v1 — the CRD FallbackModel has no trigger field). */
const FALLBACK_TRIGGERS = [
  { id: "error", label: "on error" },
  { id: "rate-limit", label: "on rate-limit" },
  { id: "timeout", label: "on timeout" },
] as const;
type TriggerId = (typeof FALLBACK_TRIGGERS)[number]["id"];

export function ModelSelector({
  model,
  modelEndpointRef,
  byoEnabled,
  fallbackModel,
  fallbackModelEndpointRef,
  errors,
  patch,
  onApplyToAll,
}: {
  model: string;
  modelEndpointRef: string;
  byoEnabled: boolean;
  fallbackModel: string;
  fallbackModelEndpointRef: string;
  errors: FieldErrors;
  patch: (p: Record<string, unknown>) => void;
  // FR-4.4 "apply a default to all agents": when supplied, the shortcut is rendered and invoked with
  // the current primary+fallback selection so the parent can fan it across the squad. Absent ⇒ the
  // shortcut is not rendered (a single-Agent compose form has no squad to fan across).
  onApplyToAll?: (defaults: {
    model: string;
    modelEndpointRef: string;
    fallbackModel: string;
    fallbackModelEndpointRef: string;
  }) => void;
}) {
  const hints = useModelHints();
  const curated = isCuratedModel(model, hints);

  // Advisory trigger chips (v1, not persisted): rate-limit is the signal the shim actually switches
  // on today (FallbackModel CRD doc); the others are presentational placeholders. Local state only —
  // no CRD field, so nothing rides toWire.
  const [triggers, setTriggers] = useState<Set<TriggerId>>(() => new Set<TriggerId>(["rate-limit"]));
  function toggleTrigger(id: TriggerId) {
    setTriggers((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  // Fallback section open-state is presentation-only (there is no fallbackEnabled form field): a
  // saved fallbackModel (edit hydration) opens it, and the user may toggle it explicitly. Turning it
  // off clears both fallback fields so a hidden fallback never rides the apply.
  const [fallbackEnabled, setFallbackEnabled] = useState(() => fallbackModel.trim() !== "");
  useEffect(() => {
    if (fallbackModel.trim() !== "" && !fallbackEnabled) setFallbackEnabled(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fallbackModel]);
  function toggleFallback() {
    if (fallbackEnabled) {
      setFallbackEnabled(false);
      patch({ fallbackModel: "", fallbackModelEndpointRef: "" });
    } else {
      setFallbackEnabled(true);
    }
  }

  // Same-provider resilience warning (FR-4.1/4.2): only meaningful while the fallback is on and both
  // ids resolve to the same coarse provider.
  const showSameProviderWarning = fallbackEnabled && sameProvider(model, fallbackModel);

  // Custom mode is presentation-only: derive its initial value from a saved-but-non-curated model
  // (edit hydration → opens in Custom pre-filled, AC2), then let the user toggle it explicitly.
  const [customMode, setCustomMode] = useState(() => model.trim() !== "" && !curated);

  // Keep the BYO toggle honest if modelEndpointRef is populated externally (e.g. a future edit
  // hydration): a bound Secret means the endpoint is on (AC4). Turning BYO off clears the ref, so
  // this never fights the user's own toggle.
  useEffect(() => {
    if (modelEndpointRef.trim() !== "" && !byoEnabled) patch({ byoEnabled: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [modelEndpointRef]);

  // The <select> shows: a curated id when the model is one; "Custom model…" while in custom mode;
  // otherwise the empty placeholder (create-time, nothing chosen yet). Curated matching trims, so
  // the value handed to the controlled <select> must be normalized too — otherwise a saved model
  // with stray whitespace ("  claude-opus-4-8  ") is classified curated yet matches no <option>
  // and silently renders as the placeholder (Copilot review, PR #240).
  const selectValue = customMode ? CUSTOM : curated ? model.trim() : "";

  function onSelect(next: string) {
    if (next === CUSTOM) {
      // Enter Custom: keep any current id as the free-text starting point (no silent loss, AC2).
      setCustomMode(true);
      return;
    }
    // A curated pick (or the placeholder) — leave custom mode and write the id verbatim (AC1).
    setCustomMode(false);
    patch({ model: next });
  }

  function backToList() {
    // Return to the curated list; drop the custom value so the dropdown isn't shadowing a hidden
    // stale id. The user re-picks from the list (or re-enters Custom).
    setCustomMode(false);
    patch({ model: "" });
  }

  function toggleByo() {
    const next = !byoEnabled;
    // Turning off clears the ref so an unused endpoint never rides along on the apply.
    patch(next ? { byoEnabled: true } : { byoEnabled: false, modelEndpointRef: "" });
  }

  const shapeHint = customMode ? modelShapeHint(model) : undefined;

  return (
    <>
      <Field
        label="Model"
        hint={
          customMode
            ? shapeHint ?? "Any model id — written verbatim to the Agent."
            : "Pick a curated Claude model, or choose “Custom model…” for any other id."
        }
        error={errors["model"]}
      >
        {customMode ? (
          <input
            value={model}
            onChange={(e) => patch({ model: e.target.value })}
            aria-invalid={!!errors["model"]}
            aria-label="Custom model id"
            placeholder="e.g. ollama/llama3.1:8b"
            autoFocus
          />
        ) : (
          <select
            value={selectValue}
            onChange={(e) => onSelect(e.target.value)}
            aria-invalid={!!errors["model"]}
            aria-label="Model"
          >
            <option value="">— Select a model —</option>
            {hints.map((h) => (
              <option key={h.id} value={h.id}>
                {h.label}
              </option>
            ))}
            <option value={CUSTOM}>Custom model…</option>
          </select>
        )}
      </Field>

      {/* The "back to curated list" control is a sibling of the <label> Field renders — never a
          child of it: a <button> inside a <label> alongside the input is invalid, ambiguous-focus
          markup for assistive tech (Copilot review, PR #240). */}
      {customMode && (
        <button type="button" className="btn btn--ghost compose__model-back" onClick={backToList}>
          ‹ Curated list
        </button>
      )}

      <div className="compose__byo">
        <button
          type="button"
          className={`btn ${byoEnabled ? "btn--primary" : ""}`}
          aria-pressed={byoEnabled}
          aria-expanded={byoEnabled}
          aria-controls={BYO_REGION_ID}
          onClick={toggleByo}
        >
          {byoEnabled ? "✓ Bring your own endpoint" : "Bring your own endpoint"}
        </button>

        {byoEnabled && (
          <div id={BYO_REGION_ID} className="compose__byo-region" role="group" aria-label="Bring your own endpoint">
            <Field
              label="Endpoint Secret ref"
              hint="name or name/key of an existing endpoint Secret (self-hosted / Ollama / non-Anthropic)"
              error={errors["modelEndpointRef.name"]}
            >
              <input
                value={modelEndpointRef}
                onChange={(e) => patch({ modelEndpointRef: e.target.value })}
                aria-invalid={!!errors["modelEndpointRef.name"]}
                aria-label="Endpoint Secret ref"
                placeholder="my-endpoint or my-endpoint/url"
              />
            </Field>
            <p className="muted compose__byo-note">
              Select an existing endpoint Secret — this sets <code>modelEndpointRef</code> only and writes no
              Secret. Creating a new endpoint inline (URL + token) is coming soon and will go through the
              credentials surface, never the compose service account.
            </p>
          </div>
        )}
      </div>

      {/* Fallback model (ISI-3681 E3-S3 / AD-4): a single secondary model the shim switches to on a
          rate_limited signal → Agent.spec.fallbackModel.{model,modelEndpointRef}. */}
      <div className="compose__fallback">
        <button
          type="button"
          className={`btn ${fallbackEnabled ? "btn--primary" : ""}`}
          aria-pressed={fallbackEnabled}
          aria-expanded={fallbackEnabled}
          aria-controls={FALLBACK_REGION_ID}
          onClick={toggleFallback}
        >
          {fallbackEnabled ? "✓ Fallback model" : "Add a fallback model"}
        </button>

        {fallbackEnabled && (
          <div id={FALLBACK_REGION_ID} className="compose__fallback-region" role="group" aria-label="Fallback model">
            <Field
              label="Fallback model"
              hint="Secondary model the runtime switches to mid-Run on a rate-limit — any model id."
              error={errors["fallbackModel.model"]}
            >
              <input
                value={fallbackModel}
                onChange={(e) => patch({ fallbackModel: e.target.value })}
                aria-invalid={!!errors["fallbackModel.model"]}
                aria-label="Fallback model id"
                placeholder="e.g. claude-haiku-4-5 or ollama/llama3.1:8b"
              />
            </Field>

            <Field
              label="Fallback endpoint Secret ref"
              hint="optional — name or name/key of the fallback's own endpoint Secret"
              error={errors["fallbackModel.modelEndpointRef.name"]}
            >
              <input
                value={fallbackModelEndpointRef}
                onChange={(e) => patch({ fallbackModelEndpointRef: e.target.value })}
                aria-invalid={!!errors["fallbackModel.modelEndpointRef.name"]}
                aria-label="Fallback endpoint Secret ref"
                placeholder="optional — my-endpoint or my-endpoint/url"
              />
            </Field>

            {/* Advisory trigger chips — presentational only in v1 (no CRD trigger field). */}
            <div className="compose__fallback-triggers" role="group" aria-label="Fallback triggers (advisory)">
              {FALLBACK_TRIGGERS.map((t) => {
                const on = triggers.has(t.id);
                return (
                  <button
                    key={t.id}
                    type="button"
                    className={`chip ${on ? "chip--on" : ""}`}
                    aria-pressed={on}
                    onClick={() => toggleTrigger(t.id)}
                  >
                    {t.label}
                  </button>
                );
              })}
            </div>
            <p className="muted compose__fallback-note">
              Triggers are advisory in this version — the runtime switches on rate-limit today. Ordered
              multi-fallback is coming in a future version.
            </p>

            {showSameProviderWarning && (
              <p className="warn compose__fallback-warning" role="alert">
                Your fallback is the same provider as your primary model — a provider-wide rate limit could
                affect both. Consider a different provider for real resilience.
              </p>
            )}
          </div>
        )}
      </div>

      {onApplyToAll && (
        <button
          type="button"
          className="btn btn--ghost compose__apply-all"
          onClick={() =>
            onApplyToAll({ model, modelEndpointRef, fallbackModel, fallbackModelEndpointRef })
          }
        >
          Apply this default to all agents
        </button>
      )}
    </>
  );
}
