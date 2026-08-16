//go:build kpi

package buildbrowser_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// KPI-2: NFR-OBS3 firewall — red-team test.
//
// The firewall gate (scripts/ci/obs-nfr-obs3-firewall.sh) must:
//   - Exit 0 for a legitimate metering allowlist (no buildbrowser entries).
//   - Exit non-zero when any ksquad.buildbrowser.* series is present.
//
// These tests verify the gate itself, not the allowlist content.
// Pending: ISI-2168 OBS-BB5 (scripts/ci/obs-nfr-obs3-firewall.sh present).
// ---------------------------------------------------------------------------

const firewallScript = "scripts/ci/obs-nfr-obs3-firewall.sh"

// writeTempAllowlist writes a temporary Go file with the given metric names
// in a const slice, mimicking the shape of internal/observability/metering_allowlist.go.
func writeTempAllowlist(t *testing.T, series []string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("package observability\n\nvar meteringAllowlist = []string{\n")
	for _, s := range series {
		sb.WriteString("\t\"")
		sb.WriteString(s)
		sb.WriteString("\",\n")
	}
	sb.WriteString("}\n")

	dir := t.TempDir()
	path := filepath.Join(dir, "metering_allowlist.go")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("writeTempAllowlist: %v", err)
	}
	return path
}

// runFirewallGate invokes the CI gate against the given allowlist file.
// Returns the exit code (0 = pass, non-zero = fail).
func runFirewallGate(t *testing.T, allowlistPath string) int {
	t.Helper()
	cmd := exec.Command("bash", firewallScript, "--allowlist", allowlistPath)
	// firewallScript is repo-root-relative, but `go test` runs with the working
	// directory set to this package (internal/observability/buildbrowser). Run
	// from the repo root so the script path resolves. allowlistPath is absolute
	// (t.TempDir), so the --allowlist arg is unaffected.
	cmd.Dir = filepath.Join("..", "..", "..")
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		t.Fatalf("runFirewallGate: unexpected error: %v", err)
	}
	return 0
}

// TestNFROBS3GateRejectsBuildbrowserInMeteringAllowlist is the red-team case:
// adding a ksquad.buildbrowser.* series to the metering allowlist must cause
// the CI gate to exit non-zero (fail the build).
func TestNFROBS3GateRejectsBuildbrowserInMeteringAllowlist(t *testing.T) {
	tmp := writeTempAllowlist(t, []string{
		"ksquad.agent.tokens",            // legitimate — must not trigger
		"ksquad.buildbrowser.read.total", // FORBIDDEN — must trigger
	})
	exitCode := runFirewallGate(t, tmp)
	if exitCode == 0 {
		t.Error("firewall gate must FAIL (exit non-zero) when ksquad.buildbrowser.* appears in the metering allowlist — NFR-OBS3 red-team failed")
	}
}

// TestNFROBS3GateAcceptsLegitimateAllowlist verifies the gate does not
// false-positive on a clean allowlist.
func TestNFROBS3GateAcceptsLegitimateAllowlist(t *testing.T) {
	tmp := writeTempAllowlist(t, []string{
		"ksquad.agent.tokens",
		"ksquad.run.duration",
		"ksquad.sandbox.claim.duration",
		"ksquad.coord.workitem.blocked",
	})
	exitCode := runFirewallGate(t, tmp)
	if exitCode != 0 {
		t.Errorf("firewall gate must PASS (exit 0) for a clean metering allowlist, got exit %d", exitCode)
	}
}
