"use client";

// ResetConfirmDialog — the focus-trapped confirm for `/reset` (ISI-5674 WS-D,
// mocks ISI-5605 §3 panel ④, a11y notes). Copy is verbatim from the spec:
//
//   Reset this discussion?
//   This clears the visible transcript. History is preserved for audit.
//   [ Cancel ]  [ Reset ]
//
// Semantics per ADR-0030 Option 2: a reset is destructive-but-REVERSIBLE (it
// appends a reset marker, never a hard delete), so the primary Reset button uses
// the amber warning accent (discussion.css `.ksq-reset-dialog__confirm`), NOT the
// red danger accent. Accessibility (spec §Accessibility): a `role="dialog"`
// `aria-modal` surface; focus is trapped; DEFAULT focus is Cancel — never Reset —
// so an accidental Enter cancels rather than destroys; Escape cancels.

import { useEffect, useRef } from "react";

export interface ResetConfirmDialogProps {
  /** Cancel / dismiss (also fired on Escape and backdrop click). */
  onCancel: () => void;
  /** Confirm the reset — runs the WS-B endpoint in the parent. */
  onConfirm: () => void;
  /** The reset round-trip is in flight — buttons disable to prevent a double-run. */
  busy?: boolean;
}

export function ResetConfirmDialog({
  onCancel,
  onConfirm,
  busy,
}: ResetConfirmDialogProps) {
  const cancelRef = useRef<HTMLButtonElement | null>(null);
  const dialogRef = useRef<HTMLDivElement | null>(null);

  // Default focus lands on Cancel (spec a11y: Reset is never the default focus,
  // so Enter never destroys by accident).
  useEffect(() => {
    cancelRef.current?.focus();
  }, []);

  // Focus trap + Escape-to-cancel. Tab/Shift+Tab cycle within the dialog's two
  // focusable buttons; Escape cancels.
  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onCancel();
      return;
    }
    if (e.key !== "Tab") return;
    const root = dialogRef.current;
    if (!root) return;
    const focusable = Array.from(
      root.querySelectorAll<HTMLElement>(
        'button:not([disabled]), [href], input, [tabindex]:not([tabindex="-1"])',
      ),
    );
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    const active = document.activeElement as HTMLElement | null;
    if (e.shiftKey && active === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    }
  };

  return (
    <div
      className="ksq-reset-dialog__backdrop"
      data-testid="reset-dialog-backdrop"
      onMouseDown={(e) => {
        // Backdrop (not the dialog body) click cancels.
        if (e.target === e.currentTarget) onCancel();
      }}
    >
      <div
        ref={dialogRef}
        className="ksq-reset-dialog"
        data-testid="reset-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="ksq-reset-dialog-title"
        aria-describedby="ksq-reset-dialog-body"
        onKeyDown={onKeyDown}
      >
        <h2 className="ksq-reset-dialog__title" id="ksq-reset-dialog-title">
          Reset this discussion?
        </h2>
        <p className="ksq-reset-dialog__body" id="ksq-reset-dialog-body">
          This clears the visible transcript. History is preserved for audit.
        </p>
        <div className="ksq-reset-dialog__actions">
          <button
            ref={cancelRef}
            type="button"
            className="ksq-reset-dialog__cancel"
            data-testid="reset-dialog-cancel"
            onClick={onCancel}
            disabled={busy}
          >
            Cancel
          </button>
          <button
            type="button"
            className="ksq-reset-dialog__confirm"
            data-testid="reset-dialog-confirm"
            onClick={onConfirm}
            disabled={busy}
          >
            {busy ? "Resetting…" : "Reset"}
          </button>
        </div>
      </div>
    </div>
  );
}
