"use client";

// components/compose/ModelSelector.tsx — the guided model picker for the Agent compose form
// (Story B, ISI-3555 / feature ISI-3544; design ISI-3546 / Winston; extended E3-S3, ISI-3681).
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
// E3-S3 (ISI-3681) EXTENDS this same component (F-UI-2 retracted — no parallel picker) with a single
// FALLBACK control mapping to Agent.spec.fallbackModel.{model,modelEndpointRef} (AD-4): a fallback
// model id (curated or custom) + its own optional endpoint ref, advisory trigger chips (on error /
// rate-limit / timeout — UI-only in v1; the CRD FallbackModel has no trigger field, AC3/Q3/R5), a
// same-provider resilience warning (FR-4.1/4.2), a labelled v2 placeholder for the multi-order
// fallback list, and an "apply to all agents" squad-default shortcut (FR-4.4).
//
// The authoritative output is always a plain `model` string (+ `modelEndpointRef` when BYO is on,
// + `fallbackModel`/`fallbackModelEndpointRef` when a fallback is set); this component never encodes
// an endpoint or token into `model`. It owns only presentation state (curated-vs-custom mode); the
// form values + validation live in lib/compose + lib/modelHints.

import { useEffect, useState } from "react";
import { Field } from "./fields";
import { isCuratedModel, modelShapeHint, sameProviderWarning, useModelHints } from "@/lib/modelHints";
import type { FieldErrors } from "@/lib/compose";

/** Sentinel <option> value for the "Custom model…" escape hatch (never a real model id). */
const CUSTOM = "__custom__";

/** The id of the BYO region, referenced by the toggle's aria-controls. */
const BYO_REGION_ID = "compose-byo-endpoint";

/** The id of the fallback region, referenced by the toggle's aria-controls. */
const FALLBACK_REGION_ID = "compose-fallback-model";

/** The advisory fallback triggers (UI-only in v1; the CRD FallbackModel has no trigger field). */
const FALLBACK_TRIGGERS: readonly { id: string; label: string }[] = [
  { id: "error", label: "on error" },
  { id: "rate-limit", label: "on rate-limit" },
  { id: "timeout", label: "on timeout" },
];

export function ModelSelector({
  model,
  modelEndpointRef,
  byoEnabled,
  fallbackModel,
  fallbackModelEndpointRef,
  fallbackTriggers,
  errors,
  patch,
  onApplyToAll,
}: {
  model: string;
  modelEndpointRef: string;
  byoEnabled: boolean;
  fallbackModel: string;
  fallbackModelEndpointRef: string;
  fallbackTriggers: string[];
  errors: FieldErrors;
  patch: (p: Record<string, unknown>) => void;
  // onApplyToAll, when wired by a squad-aware parent, applies this model + fallback across every
  // agent in the squad (FR-4.4). Absent (single-agent compose today) ⇒ the affordance renders an
  // honest "coming soon" disabled state — no silent no-op button.
  onApplyToAll?: () => void;
}) {
  const hints = useModelHints();
  const curated = isCuratedModel(model, hints);

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

  // ── Fallback control (E3-S3, AD-4) ──────────────────────────────────────────
  // The fallback section is "on" whenever a fallback model/endpoint is already set (edit hydration)
  // or the user opened it. Its own curated-vs-custom presentation mirrors the primary picker.
  const fbHasValue = fallbackModel.trim() !== "" || fallbackModelEndpointRef.trim() !== "";
  const [fallbackEnabled, setFallbackEnabled] = useState(() => fbHasValue);
  const fbCurated = isCuratedModel(fallbackModel, hints);
  const [fbCustomMode, setFbCustomMode] = useState(() => fallbackModel.trim() !== "" && !fbCurated);
  const fbSelectValue = fbCustomMode ? CUSTOM : fbCurated ? fallbackModel.trim() : "";

  // A fallback endpoint ref bound externally (edit hydration) implies the section is open.
  useEffect(() => {
    if (fbHasValue && !fallbackEnabled) setFallbackEnabled(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fallbackModel, fallbackModelEndpointRef]);

  function toggleFallback() {
    const next = !fallbackEnabled;
    setFallbackEnabled(next);
    // Turning off clears the fallback so a half-filled control never rides along on the apply
    // (toWire also omits an empty fallbackModel, but clearing keeps the wire and the UI in lockstep).
    if (!next) {
      setFbCustomMode(false);
      patch({ fallbackModel: "", fallbackModelEndpointRef: "", fallbackTriggers: [] });
    }
  }

  function onFbSelect(next: string) {
    if (next === CUSTOM) {
      setFbCustomMode(true);
      return;
    }
    setFbCustomMode(false);
    patch({ fallbackModel: next });
  }

  function fbBackToList() {
    setFbCustomMode(false);
    patch({ fallbackModel: "" });
  }

  function toggleTrigger(id: string) {
    // Advisory chips (v1): toggle membership in the UI-only fallbackTriggers set. NOT persisted.
    const on = fallbackTriggers.includes(id);
    const next = on ? fallbackTriggers.filter((t) => t !== id) : [...fallbackTriggers, id];
    patch({ fallbackTriggers: next });
  }

  const fbShapeHint = fbCustomMode ? modelShapeHint(fallbackModel) : undefined;
  const providerWarning = fallbackEnabled ? sameProviderWarning(model, fallbackModel) : undefined;

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

      {/* ── Fallback model (E3-S3, AD-4) ─────────────────────────────────────── */}
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
          <div id={FALLBACK_REGION_ID} className="compose__fallback-region" role="group" aria-label="Fallback model settings">
            <Field
              label="Fallback model"
              hint={
                fbCustomMode
                  ? fbShapeHint ?? "Any model id — the shim switches to it mid-Run on a rate-limit signal."
                  : "The secondary model the shim switches to mid-Run on a rate-limit signal."
              }
              error={errors["fallbackModel.model"]}
            >
              {fbCustomMode ? (
                <input
                  value={fallbackModel}
                  onChange={(e) => patch({ fallbackModel: e.target.value })}
                  aria-invalid={!!errors["fallbackModel.model"]}
                  aria-label="Custom fallback model id"
                  placeholder="e.g. ollama/llama3.1:8b"
                  autoFocus
                />
              ) : (
                <select
                  value={fbSelectValue}
                  onChange={(e) => onFbSelect(e.target.value)}
                  aria-invalid={!!errors["fallbackModel.model"]}
                  aria-label="Fallback model"
                >
                  <option value="">— Select a fallback model —</option>
                  {hints.map((h) => (
                    <option key={h.id} value={h.id}>
                      {h.label}
                    </option>
                  ))}
                  <option value={CUSTOM}>Custom model…</option>
                </select>
              )}
            </Field>

            {fbCustomMode && (
              <button type="button" className="btn btn--ghost compose__model-back" onClick={fbBackToList}>
                ‹ Curated list
              </button>
            )}

            {providerWarning && (
              <p className="muted compose__fallback-warning" role="status">
                ⚠ {providerWarning}
              </p>
            )}

            {/* Advisory trigger chips (AC3): presentational only in v1 — the CRD FallbackModel has no
                trigger field, so these are NOT persisted. They document the intended switch signals. */}
            <div
              className="compose__fallback-triggers"
              role="group"
              aria-label="Fallback triggers (advisory)"
            >
              {FALLBACK_TRIGGERS.map((t) => {
                const on = fallbackTriggers.includes(t.id);
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
              Triggers are advisory in this version — the shim recovers on rate-limit today; per-trigger
              routing and an ordered multi-fallback list are coming next.
            </p>

            <Field
              label="Fallback endpoint Secret ref"
              hint="optional — name or name/key of an endpoint Secret for the fallback (defaults to the primary endpoint)"
              error={errors["fallbackModel.modelEndpointRef.name"]}
            >
              <input
                value={fallbackModelEndpointRef}
                onChange={(e) => patch({ fallbackModelEndpointRef: e.target.value })}
                aria-invalid={!!errors["fallbackModel.modelEndpointRef.name"]}
                aria-label="Fallback endpoint Secret ref"
                placeholder="my-endpoint or my-endpoint/url"
              />
            </Field>

            {/* Multi-order fallback list — labelled v2 placeholder (AC3), not built. */}
            <p className="muted compose__fallback-v2" aria-disabled="true">
              Ordered multi-fallback list — coming soon (v2).
            </p>
          </div>
        )}
      </div>

      {/* Apply this model + fallback across the whole squad (FR-4.4). Wired only when a squad-aware
          parent provides onApplyToAll; otherwise an honest disabled "coming soon" affordance. */}
      <div className="compose__apply-all">
        <button
          type="button"
          className="btn btn--ghost"
          onClick={() => onApplyToAll?.()}
          disabled={!onApplyToAll}
          title={
            onApplyToAll
              ? "Apply this model and fallback to every agent in the squad"
              : "Applying a squad-wide default is coming soon"
          }
        >
          Apply to all agents
        </button>
        {!onApplyToAll && (
          <p className="muted compose__apply-all-note">
            Sets one model + fallback across the squad — coming soon.
          </p>
        )}
      </div>
    </>
  );
}
