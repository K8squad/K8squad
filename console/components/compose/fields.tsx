// components/compose/fields.tsx — shared field primitives for the Compose surface (story 8.5).
//
// Extracted from ComposeScreen so the guided ModelSelector (Story B, ISI-3555) renders labels,
// hints and errors identically to every other compose field. Pure presentation — no state.

import type { ReactNode } from "react";

/** A labeled form field: label + control + a mutually-exclusive hint/error line. */
export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: string;
  hint?: string;
  error?: string;
  children: ReactNode;
}) {
  return (
    <label className="compose__field">
      <span>{label}</span>
      {children}
      {hint && !error && <em className="field-hint">{hint}</em>}
      {error && <em className="field-error">{error}</em>}
    </label>
  );
}
