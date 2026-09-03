"use client";

// components/forms/TeamForm.tsx — E0 keystone (ISI-3670, AD-1, FR-8).
//
// View-only extraction of the Team inputs previously inline in ComposeScreen's KindFields, over the
// already-pure lib/compose.ts model. Mounted identically by every create surface (Launchpad E1,
// template review E2, empty-states E6, Compose E5) so all forms render identical, correct fields.
// Reuses the shared Field primitive (components/compose/fields.tsx) — no new form-state library.

import type { TeamForm as TeamFormFields, FieldErrors } from "@/lib/compose";
import { Field } from "../compose/fields";

interface TeamFormProps {
  form: TeamFormFields;
  errors: FieldErrors;
  patch: (p: Record<string, unknown>) => void;
}

export function TeamForm({ form, errors, patch }: TeamFormProps) {
  return (
    <div className="compose__grid">
      <Field label="Name" hint="DNS-1123 label (lowercase, digits, '-')" error={errors["name"]}>
        <input
          value={form.name}
          onChange={(e) => patch({ name: e.target.value })}
          aria-invalid={!!errors["name"]}
          placeholder="my-resource"
        />
      </Field>
      <Field label="Namespace strategy" hint="optional — defaults to perTeam">
        <input
          value={form.namespaceStrategy}
          onChange={(e) => patch({ namespaceStrategy: e.target.value })}
          placeholder="perTeam"
        />
      </Field>
    </div>
  );
}
