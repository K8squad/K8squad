"use client";

// components/settings/ModelPrioritySection.tsx — Settings → Configuration: the
// org-default model tier surface (ISI-4890 S1, epic ISI-4822 Flow A).
//
// A squad admin sets/changes the org-wide DEFAULT model (primary + fallback +
// optional BYO endpoint) — the FLOOR of the Model-Per-Role resolution ladder
// (agent → role → this default, ISI-4430), which shipped an engine but no UI. This
// section is additive on Settings→Configuration beside OtlpConfigScreen (they share
// no state).
//
// Contracts:
//   - Admin gate (AC2): the editable form renders only when `isAdmin` (resolved
//     server-side from /auth/me → globalRole and passed as a prop — UX only). The
//     SERVER is authoritative: the `modelconfig` compose kind is writeScope
//     adminOnly, so a forged write is a 403 regardless. Non-admins see a read-only
//     "managed by an admin" note.
//   - Load (AC1): GET /api/modelconfig → hydrate via modelConfigFromWire; 404 ⇒ the
//     empty-form state (no default configured yet).
//   - Save (AC3): POST /api/compose/modelconfig with modelConfigToWire(form)
//     (upsert of the k8squad-system/default singleton). Status codes surface
//     verbatim (201/200 ok, 422 field error, 401/403, 409, 501 cluster-less).
//   - Fail-closed guardrail (AC4): Save is DISABLED while the primary model is empty
//     (the ONLY tier where blank is never "inherit" — nothing resolves below it),
//     mirroring ISI-4430 D3 (helm required). The fallback/BYO controls do not bypass
//     it — only a non-empty primary enables Save.
//   - Frame-4 (AC5): RuntimeAdapterStep fronts ModelSelector, branching the
//     credential path per adapter (claude BYO Anthropic-key attach + coming-soon OAuth
//     CTA — ISI-6702 / codex
//     token / opencode BYO endpoint → manual entry, the fail-open state / vibe
//     Mistral endpoint via a mistral-pinned ProviderModelPicker / goose preview-disabled
//     CTA — ISI-6380).
//   - Same-provider advisory (AC7): reused verbatim from ModelSelector.

import { useEffect, useState } from "react";
import { ModelSelector } from "@/components/compose/ModelSelector";
import { ProviderModelPicker } from "@/components/compose/ProviderModelPicker";
import { RuntimeAdapterStep } from "@/components/compose/RuntimeAdapterStep";
import {
  CreateCredentialForm,
  TeamPicker,
} from "@/components/credentials/CreateCredentialForm";
import type {
  CredentialTeamList,
  CredentialTeamOption,
} from "@/lib/credentials";
import {
  emptyModelConfigForm,
  isModelConfigValid,
  modelConfigFromWire,
  modelConfigToWire,
  validateModelConfig,
  type FieldErrors,
  type LlmProviderId,
  type ModelConfigForm,
  type ModelEndpointRow,
  type RuntimeAdapter,
} from "@/lib/compose";

type LoadState =
  | { kind: "loading" }
  | { kind: "ready" } // loaded (empty via 404, or hydrated)
  | { kind: "error"; status: number };

type SaveState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "ok" }
  | { kind: "error"; status: number; message: string };

/** Parse the apiserver 422 body `{ fields: [{field, message}] }` into keyed errors. */
function fieldsFrom(body: unknown): FieldErrors {
  const errs: FieldErrors = {};
  const fields = (
    body as { fields?: Array<{ field?: string; message?: string }> } | null
  )?.fields;
  if (Array.isArray(fields)) {
    for (const f of fields) {
      if (f?.field) errs[f.field] = f.message ?? "invalid";
    }
  }
  return errs;
}

export function ModelPrioritySection({ isAdmin }: { isAdmin: boolean }) {
  const [load, setLoad] = useState<LoadState>({
    kind: isAdmin ? "loading" : "ready",
  });
  const [form, setForm] = useState<ModelConfigForm>(() =>
    emptyModelConfigForm(),
  );
  const [save, setSave] = useState<SaveState>({ kind: "idle" });
  // Field errors surface only after a Save attempt (or a server 422), so an empty
  // create form doesn't shout "is required" before the admin has touched anything.
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [attempted, setAttempted] = useState(false);
  // Saved endpoints for the opencode picker's advanced "existing endpoint" dropdown
  // (GET /api/modelendpoints). Admin-only; a failure degrades to no escape hatch (the
  // guided provider flow still works), so it never blocks the surface.
  const [endpoints, setEndpoints] = useState<ModelEndpointRow[]>([]);
  const [fallbackOpen, setFallbackOpen] = useState(false);
  // Fleet-admin team context for the Claude key-attach lane (ISI-6702 WS-4, reusing
  // the ISI-3983/3937 picker): a fleet admin with more than one Team picks where the
  // Anthropic-key Secret lands; a single-team (or failed) list omits the hint upstream,
  // so the lane never blocks on it.
  const [teams, setTeams] = useState<CredentialTeamOption[]>([]);
  const [selectedTeam, setSelectedTeam] = useState<string>("");
  useEffect(() => {
    if (!isAdmin) return; // non-admins never see the attach lane (the form is admin-only)
    let alive = true;
    (async () => {
      try {
        const r = await fetch("/api/squad/teams", { cache: "no-store" });
        if (!r.ok) return;
        const body = (await r.json().catch(() => ({}))) as CredentialTeamList;
        const list = (body?.teams ?? []).filter(
          (t): t is CredentialTeamOption => !!t,
        );
        if (!alive) return;
        setTeams(list);
        if (list.length === 1) setSelectedTeam(list[0].uid);
      } catch {
        /* best-effort: no picker, own-team default upstream */
      }
    })();
    return () => {
      alive = false;
    };
  }, [isAdmin]);

  // Hydrate in one ordered pass (ISI-5302): the saved endpoints must load BEFORE the
  // model config, because the persisted spec is adapter-agnostic and the adapter is
  // inferred from the primary modelEndpointRef's provider (modelConfigFromWire). Fetch
  // endpoints first (tolerating failure → empty list, which just degrades inference to
  // claude and drops the "existing endpoint" escape hatch), then hydrate the form.
  useEffect(() => {
    if (!isAdmin) return; // non-admins never fetch (and would 403 anyway)
    let alive = true;
    (async () => {
      let rows: ModelEndpointRow[] = [];
      try {
        const r = await fetch("/api/modelendpoints", { cache: "no-store" });
        if (r.ok) {
          const body = await r.json().catch(() => null);
          const got = (body as { endpoints?: ModelEndpointRow[] })?.endpoints;
          if (Array.isArray(got)) rows = got;
        }
      } catch {
        /* endpoints are best-effort; fall through with an empty list */
      }
      if (!alive) return;
      setEndpoints(rows);

      try {
        const r = await fetch("/api/modelconfig", { cache: "no-store" });
        if (!alive) return;
        if (r.status === 404) {
          setLoad({ kind: "ready" }); // no default yet — empty form
          return;
        }
        if (!r.ok) {
          setLoad({ kind: "error", status: r.status });
          return;
        }
        const wire = await r.json();
        if (!alive) return;
        setForm(modelConfigFromWire(wire, rows));
        setLoad({ kind: "ready" });
      } catch {
        if (alive) setLoad({ kind: "error", status: 0 });
      }
    })();
    return () => {
      alive = false;
    };
  }, [isAdmin]);

  function patch(p: Record<string, unknown>) {
    setForm((f) => ({ ...f, ...p }));
    setSave({ kind: "idle" });
  }
  function onAdapterChange(adapter: RuntimeAdapter) {
    patch({ adapter });
  }

  const valid = isModelConfigValid(form);
  // Show validation errors only once the admin has tried to save; merge any server
  // 422 field errors on top (they always show).
  const shownErrors: FieldErrors = attempted
    ? { ...validateModelConfig(form), ...serverErrors }
    : serverErrors;

  // The BYO picker block, shared by the opencode and vibe tabs (ISI-6380): opencode
  // keeps the free backend dropdown; vibe pins it to mistral — the backend-complete
  // lane (masked API key + optional base URL + live model list + Secret save).
  const byoPicker = (adapter: "opencode" | "vibe", fixed?: LlmProviderId) => (
    <div
      className="settings-modelpriority__opencode"
      data-testid={`modelpriority-${adapter}`}
    >
      <p className="muted">
        {adapter === "vibe"
          ? "Paste your Mistral API key, list its live models, and Save — K8squad creates the endpoint Secret automatically. An optional base URL overrides the hosted Mistral API (the BYO lane)."
          : "Pick a backend, list its live models, and Save — K8squad creates the endpoint Secret automatically. No pre-existing Secret required."}
      </p>
      <ProviderModelPicker
        label="Model"
        idPrefix="primary"
        fixedProvider={fixed}
        model={form.model}
        modelEndpointRef={form.modelEndpointRef}
        existingEndpoints={endpoints}
        errors={shownErrors}
        onChange={(next) =>
          patch({
            ...(next.model !== undefined ? { model: next.model } : {}),
            ...(next.modelEndpointRef !== undefined
              ? {
                  modelEndpointRef: next.modelEndpointRef,
                  byoEnabled: next.modelEndpointRef.trim() !== "",
                }
              : {}),
          })
        }
      />

      <div className="settings-modelpriority__fallback">
        {!fallbackOpen && form.fallbackModel.trim() === "" ? (
          <button
            type="button"
            className="btn btn--ghost"
            onClick={() => setFallbackOpen(true)}
            data-testid="modelpriority-add-fallback"
          >
            Add a fallback model
          </button>
        ) : (
          <ProviderModelPicker
            label="Fallback model"
            idPrefix="fallback"
            fixedProvider={fixed}
            model={form.fallbackModel}
            modelEndpointRef={form.fallbackModelEndpointRef}
            existingEndpoints={endpoints}
            errors={shownErrors}
            onChange={(next) =>
              patch({
                ...(next.model !== undefined
                  ? { fallbackModel: next.model }
                  : {}),
                ...(next.modelEndpointRef !== undefined
                  ? { fallbackModelEndpointRef: next.modelEndpointRef }
                  : {}),
              })
            }
          />
        )}
      </div>
    </div>
  );

  // The Claude key-attach block (ISI-6702 WS-4): the ISI-3983 BYO paste form + fleet
  // team picker, pinned to the claude-code runtime, with the ANTHROPIC_API_KEY
  // test-connection probe. The OAuth CTA itself stays disabled inside
  // RuntimeAdapterStep (ToS-gated, ISI-3661 — never a live subscription click).
  const needsTeamSelection = teams.length > 1;
  const claudeKeyAttach = (
    <div
      className="settings-modelpriority__claude"
      data-testid="modelpriority-claude"
    >
      <p className="muted">
        Claude Code runs on your Anthropic API key — paste it, Save, and Test
        connection (a live probe against api.anthropic.com). The key is stored
        as a per-team Secret (ANTHROPIC_API_KEY) and never shown back.
        Subscription sign-in stays gated on a board Terms-of-Service decision
        (ISI-3661).
      </p>
      {needsTeamSelection && (
        <TeamPicker
          teams={teams}
          selected={selectedTeam}
          onSelect={setSelectedTeam}
          prompt
        />
      )}
      <CreateCredentialForm
        fixedRuntime="claude-code"
        title="Attach an Anthropic API key"
        lede="Bring your own Anthropic API key — KSquad stores it as a per-team Kubernetes Secret and never shows it back. Subscription sign-in (Sign in with Claude) is pending a board Terms-of-Service decision (ISI-3661) — no ETA."
        teamId={selectedTeam}
        teamReady={!needsTeamSelection || selectedTeam !== ""}
      />
    </div>
  );

  async function onSave() {
    setAttempted(true);
    if (!valid) return;
    setSave({ kind: "saving" });
    setServerErrors({});
    try {
      const res = await fetch("/api/compose/modelconfig", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(modelConfigToWire(form)),
      });
      if (res.ok) {
        setSave({ kind: "ok" });
        return;
      }
      let message = `Save failed (status ${res.status}).`;
      if (res.status === 422) {
        const body = await res.json().catch(() => null);
        setServerErrors(fieldsFrom(body));
        message = "Some fields need attention.";
      } else if (res.status === 401 || res.status === 403) {
        message =
          "You don’t have permission to change the org default (admin only).";
      } else if (res.status === 409) {
        message = "The org default changed elsewhere — reload and try again.";
      } else if (res.status === 501) {
        message = "Model configuration isn’t available on this cluster yet.";
      }
      setSave({ kind: "error", status: res.status, message });
    } catch {
      setSave({
        kind: "error",
        status: 0,
        message: "Network error — try again.",
      });
    }
  }

  return (
    <section
      className="settings-modelpriority"
      aria-labelledby="modelpriority-heading"
    >
      <h2 id="modelpriority-heading">Model Priority</h2>
      <p className="muted">
        The org-wide <strong>default</strong> model — the floor every Agent and
        Role falls back to when it doesn’t name its own. This can never be
        empty: without it, the platform can’t resolve a model at all.
      </p>

      {!isAdmin && (
        <p className="muted" data-testid="modelpriority-readonly" role="note">
          The org default model is managed by an admin.
        </p>
      )}

      {isAdmin && load.kind === "loading" && <p className="muted">Loading…</p>}
      {isAdmin && load.kind === "error" && (
        <p
          className="state state--error"
          role="alert"
          data-testid="modelpriority-load-error"
        >
          Could not read the org default model (status{" "}
          {load.status || "network"}).
        </p>
      )}

      {isAdmin && load.kind === "ready" && (
        <div
          className="card settings-modelpriority__form"
          data-testid="modelpriority-form"
        >
          <RuntimeAdapterStep
            adapter={form.adapter}
            onAdapterChange={onAdapterChange}
            opencodeSlot={
              form.adapter === "opencode" ? byoPicker("opencode") : undefined
            }
            vibeSlot={
              form.adapter === "vibe" ? byoPicker("vibe", "mistral") : undefined
            }
            claudeSlot={form.adapter === "claude" ? claudeKeyAttach : undefined}
          >
            {form.adapter === "opencode" || form.adapter === "vibe" ? (
              <></>
            ) : (
              <ModelSelector
                model={form.model}
                modelEndpointRef={form.modelEndpointRef}
                byoEnabled={form.byoEnabled}
                fallbackModel={form.fallbackModel}
                fallbackModelEndpointRef={form.fallbackModelEndpointRef}
                errors={shownErrors}
                patch={patch}
              />
            )}
          </RuntimeAdapterStep>

          <div className="settings-modelpriority__actions">
            <button
              type="button"
              className="btn btn--primary"
              onClick={onSave}
              disabled={!valid || save.kind === "saving"}
              aria-disabled={!valid || save.kind === "saving"}
              title={
                valid
                  ? undefined
                  : "The org default can never be empty — the platform can’t resolve a model without it."
              }
              data-testid="modelpriority-save"
            >
              {save.kind === "saving" ? "Saving…" : "Save org default"}
            </button>

            {!valid && (
              <p
                className="muted settings-modelpriority__guardrail"
                role="note"
                data-testid="modelpriority-guardrail"
              >
                🔒 Choose a default model to enable Save — the org default can
                never be empty, so the platform can always resolve a model
                (ISI-4430 fail-closed).
              </p>
            )}
            {save.kind === "ok" && (
              <span
                className="state state--ok"
                role="status"
                data-testid="modelpriority-saved"
              >
                Saved.
              </span>
            )}
            {save.kind === "error" && (
              <span
                className="state state--error"
                role="alert"
                data-testid="modelpriority-save-error"
              >
                {save.message}
              </span>
            )}
          </div>
        </div>
      )}
    </section>
  );
}
