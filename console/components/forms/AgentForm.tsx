"use client";

// components/forms/AgentForm.tsx — E0 keystone (ISI-3670, AD-1, FR-8).
//
// View-only extraction of the Agent inputs previously inline in ComposeScreen's KindFields, over the
// already-pure lib/compose.ts model. Keeps the guided <ModelSelector> (Story B, ISI-3555) as the
// model control. E3-S3 (ISI-3681) will extend this form with fallbackModel + credentialClass; this
// component is the single home for that extension.

import type { AgentForm as AgentFormFields, FieldErrors } from "@/lib/compose";
import { Field } from "../compose/fields";
import { ModelSelector } from "../compose/ModelSelector";

interface AgentFormProps {
  form: AgentFormFields;
  errors: FieldErrors;
  patch: (p: Record<string, unknown>) => void;
}

export function AgentForm({ form, errors, patch }: AgentFormProps) {
  return (
    <div className="compose__grid">
      <Field label="Project" hint="the squad this Agent composes within" error={errors["project"]}>
        <input
          value={form.project}
          onChange={(e) => patch({ project: e.target.value })}
          aria-invalid={!!errors["project"]}
        />
      </Field>
      <Field label="Name" hint="DNS-1123 label (lowercase, digits, '-')" error={errors["name"]}>
        <input
          value={form.name}
          onChange={(e) => patch({ name: e.target.value })}
          aria-invalid={!!errors["name"]}
          placeholder="my-resource"
        />
      </Field>
      <Field label="Runtime ref" error={errors["runtimeRef.name"]}>
        <input
          value={form.runtimeRef}
          onChange={(e) => patch({ runtimeRef: e.target.value })}
          aria-invalid={!!errors["runtimeRef.name"]}
          placeholder="name or namespace/name"
        />
      </Field>
      <Field label="Role ref" error={errors["roleRef.name"]}>
        <input
          value={form.roleRef}
          onChange={(e) => patch({ roleRef: e.target.value })}
          aria-invalid={!!errors["roleRef.name"]}
        />
      </Field>
      <ModelSelector
        model={form.model}
        modelEndpointRef={form.modelEndpointRef}
        byoEnabled={form.byoEnabled}
        errors={errors}
        patch={patch}
      />
      <Field label="Credential Secret ref" hint="name or name/key" error={errors["credentialSecretRef.name"]}>
        <input
          value={form.credentialSecretRef}
          onChange={(e) => patch({ credentialSecretRef: e.target.value })}
          aria-invalid={!!errors["credentialSecretRef.name"]}
        />
      </Field>
      <Field label="Skill refs" hint="optional — one name (or namespace/name) per line">
        <textarea
          rows={3}
          value={form.skillRefs}
          onChange={(e) => patch({ skillRefs: e.target.value })}
        />
      </Field>
    </div>
  );
}
