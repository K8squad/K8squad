import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { ResetConfirmDialog } from "@/components/discussion/ResetConfirmDialog";

afterEach(cleanup);

// ISI-5674 WS-D — the /reset confirm (mocks §3 ④, a11y notes).

describe("<ResetConfirmDialog>", () => {
  it("renders the spec copy and both actions", () => {
    render(<ResetConfirmDialog onCancel={vi.fn()} onConfirm={vi.fn()} />);
    expect(screen.getByText("Reset this discussion?")).toBeTruthy();
    expect(
      screen.getByText(
        "This clears the visible transcript. History is preserved for audit.",
      ),
    ).toBeTruthy();
    expect(screen.getByTestId("reset-dialog-cancel").textContent).toBe("Cancel");
    expect(screen.getByTestId("reset-dialog-confirm").textContent).toBe("Reset");
  });

  it("is a labelled modal dialog", () => {
    render(<ResetConfirmDialog onCancel={vi.fn()} onConfirm={vi.fn()} />);
    const dialog = screen.getByTestId("reset-dialog");
    expect(dialog.getAttribute("role")).toBe("dialog");
    expect(dialog.getAttribute("aria-modal")).toBe("true");
  });

  it("focuses Cancel on mount, not Reset (Enter never destroys by accident)", () => {
    render(<ResetConfirmDialog onCancel={vi.fn()} onConfirm={vi.fn()} />);
    expect(document.activeElement).toBe(screen.getByTestId("reset-dialog-cancel"));
  });

  it("Cancel and Reset fire their callbacks", () => {
    const onCancel = vi.fn();
    const onConfirm = vi.fn();
    render(<ResetConfirmDialog onCancel={onCancel} onConfirm={onConfirm} />);
    fireEvent.click(screen.getByTestId("reset-dialog-confirm"));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByTestId("reset-dialog-cancel"));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("Escape cancels", () => {
    const onCancel = vi.fn();
    render(<ResetConfirmDialog onCancel={onCancel} onConfirm={vi.fn()} />);
    fireEvent.keyDown(screen.getByTestId("reset-dialog"), { key: "Escape" });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("a backdrop click cancels; a dialog-body click does not", () => {
    const onCancel = vi.fn();
    render(<ResetConfirmDialog onCancel={onCancel} onConfirm={vi.fn()} />);
    fireEvent.mouseDown(screen.getByTestId("reset-dialog"));
    expect(onCancel).not.toHaveBeenCalled();
    fireEvent.mouseDown(screen.getByTestId("reset-dialog-backdrop"));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("disables both buttons while busy", () => {
    render(<ResetConfirmDialog onCancel={vi.fn()} onConfirm={vi.fn()} busy />);
    expect(
      (screen.getByTestId("reset-dialog-cancel") as HTMLButtonElement).disabled,
    ).toBe(true);
    const confirm = screen.getByTestId(
      "reset-dialog-confirm",
    ) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    expect(confirm.textContent).toBe("Resetting…");
  });
});
