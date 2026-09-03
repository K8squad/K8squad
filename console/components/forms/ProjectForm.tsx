"use client";

// components/forms/ProjectForm.tsx — E0 keystone (ISI-3670, AD-1, FR-8).
//
// View-only extraction of the Project inputs previously inline in ComposeScreen's KindFields, over
// the already-pure lib/compose.ts model. Reuses the shared Field primitive.

import type { ProjectForm as ProjectFormFields, FieldErrors } from "@/lib/compose";
import { Field } from "../compose/fields";

interface ProjectFormProps {
  form: ProjectFormFields;
  errors: FieldErrors;
  patch: (p: Record<string, unknown>) => void;
}

export function ProjectForm({ form, errors, patch }: ProjectFormProps) {
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
      <Field label="Repo URL" error={errors["repo.url"]}>
        <input
          value={form.repoUrl}
          onChange={(e) => patch({ repoUrl: e.target.value })}
          aria-invalid={!!errors["repo.url"]}
          placeholder="https://github.com/org/repo"
        />
      </Field>
      <Field label="Repo ref" hint="optional — branch / tag / SHA">
        <input
          value={form.repoRef}
          onChange={(e) => patch({ repoRef: e.target.value })}
          placeholder="main"
        />
      </Field>
      <Field label="Egress policy ref" hint="optional — name or namespace/name">
        <input
          value={form.egressPolicyRef}
          onChange={(e) => patch({ egressPolicyRef: e.target.value })}
        />
      </Field>
      <Field label="Goals" hint="one goal per line">
        <textarea
          rows={3}
          value={form.goals}
          onChange={(e) => patch({ goals: e.target.value })}
          placeholder={"Ship the checkout flow\nHarden the payment path"}
        />
      </Field>
    </div>
  );
}
