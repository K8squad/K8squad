"use client";

import { ComposeForm, FieldErrors } from "@/lib/compose";
import { Field } from "./fields";

interface TeamFormProps {
  cf: ComposeForm & { kind: "teams" };
  errors: FieldErrors;
  patch: (p: Record<string, unknown>) => void;
}

export function TeamForm({ cf, errors, patch }: TeamFormProps) {
  const nameField = (
    <Field label="Name" hint="DNS-1123 label (lowercase, digits, '-')" error={errors["name"]}>
      <input
        value={cf.form.name}
        onChange={(e) => patch({ name: e.target.value })}
        aria-invalid={!!errors["name"]}
        placeholder="my-resource"
      />
    </Field>
  );

  return (
    <div className="compose__grid">
      {nameField}
      <Field label="Namespace strategy" hint="optional — defaults to perTeam">
        <input
          value={cf.form.namespaceStrategy}
          onChange={(e) => patch({ namespaceStrategy: e.target.value })}
          placeholder="perTeam"
        />
      </Field>
    </div>
  );
}