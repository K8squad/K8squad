package observability_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Red-team test for the general cardinality CI gate
// (scripts/ci/obs-cardinality-gate.sh, story 13.6 / ISI-3122; obs-plan §5.6).
//
// The gate must:
//   - Exit 0 on the real tree and on any metric whose labels are all in the
//     bounded-cardinality allowlist.
//   - Exit non-zero when a metric declares an out-of-allowlist label, a
//     forbidden unbounded identifier, or a non-literal (unverifiable) label list.
//
// These tests verify the gate behavior, not the allowlist content. The gate is
// resolved relative to the repo root (this package lives at
// internal/observability, two levels down).

const cardinalityGate = "scripts/ci/obs-cardinality-gate.sh"

func repoRoot() string { return filepath.Join("..", "..") }

// runGate invokes the gate scanning the given file/dir and returns its exit code.
func runGate(t *testing.T, scan string) int {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", cardinalityGate, "--scan", scan)
	cmd.Dir = repoRoot() // gate path is repo-root-relative; scan arg is absolute.
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		t.Logf("gate output:\n%s", out)
		return exitErr.ExitCode()
	}
	t.Fatalf("runGate: unexpected error: %v\n%s", err, out)
	return -1
}

// writeMetric writes a temp Go file declaring one Prometheus *Vec with the given
// literal label list (pass a raw expression like `[]string{"run_id"}` or a bare
// variable name to exercise the non-literal guard).
func writeMetric(t *testing.T, labelArg string) string {
	t.Helper()
	src := "package x\n\nvar m = prometheus.NewCounterVec(prometheus.CounterOpts{Name: \"ksquad_x_total\"}, " + labelArg + ")\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "metric.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("writeMetric: %v", err)
	}
	return path
}

func TestGateAcceptsAllowlistedLabels(t *testing.T) {
	// project/agent/role are the 13.9 bounded dims — must pass.
	if code := runGate(t, writeMetric(t, `[]string{"project", "agent", "role"}`)); code != 0 {
		t.Errorf("gate must PASS for allowlisted labels, got exit %d", code)
	}
}

func TestGateRejectsForbiddenIdentifier(t *testing.T) {
	// run_id is a forbidden unbounded identifier (obs-plan §5.6).
	if code := runGate(t, writeMetric(t, `[]string{"outcome", "run_id"}`)); code == 0 {
		t.Error("gate must FAIL when a metric declares the forbidden label run_id")
	}
}

func TestGateRejectsUnknownLabel(t *testing.T) {
	// tenant is neither bounded-enum nor sanctioned — out of allowlist.
	if code := runGate(t, writeMetric(t, `[]string{"tenant"}`)); code == 0 {
		t.Error("gate must FAIL for an out-of-allowlist label (tenant)")
	}
}

func TestGateRejectsNonLiteralLabelList(t *testing.T) {
	// A variable label list defeats static analysis — fail closed.
	if code := runGate(t, writeMetric(t, `labelKeys`)); code == 0 {
		t.Error("gate must FAIL when the label list is a non-literal expression")
	}
}
