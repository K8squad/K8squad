// Guard test for the bundled NATS/JetStream event bus (ISI-3508). Renders the
// chart with `helm template` and asserts the two behaviours the ticket requires:
//   - controlPlane.enabled + a DSN + a NATS storage class ALONE yields a complete
//     event plane (NATS StatefulSet + event-relay auto-wired to the bundled bus).
//   - a bundled bus with NO storage class fails fast (§16.2 — never the cluster
//     default), and an external eventRelay.natsUrl wins over the derived one.
//
// Skips (never fails) when the `helm` binary is absent, matching this repo's
// skip-with-reason convention for tool/service-gated lanes.
package helm

import (
	"os/exec"
	"strings"
	"testing"
)

func render(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm binary not on PATH; skipping chart-render guard")
	}
	base := []string{"template", "t", ".",
		"--set", "controlPlane.enabled=true",
		"--set", "controlPlane.database.dsn=postgres://u@h/db"}
	out, err := exec.Command("helm", append(base, args...)...).CombinedOutput()
	return string(out), err
}

func TestNatsBundledPlaneComplete(t *testing.T) {
	out, err := render(t, "--set", "controlPlane.nats.persistence.storageClassName=nfs-csi")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"kind: StatefulSet",        // NATS bus rendered
		"name: ksquad-nats",        // client Service + StatefulSet
		"name: ksquad-event-relay", // relay auto-enabled by nats.enabled
		`value: "nats://ksquad-nats.k8squad-system.svc:4222"`, // derived RELAY_NATS_URL
	} {
		if !strings.Contains(out, want) {
			t.Errorf("complete plane missing %q in render", want)
		}
	}
}

func TestNatsRequiresStorageClass(t *testing.T) {
	out, err := render(t) // nats.enabled defaults true, no storage class
	if err == nil {
		t.Fatalf("expected fail-fast without a NATS storage class, got success:\n%s", out)
	}
	if !strings.Contains(out, "storageClassName is REQUIRED") {
		t.Errorf("expected §16.2 storage-class fail-fast, got:\n%s", out)
	}
}

func TestExternalNatsUrlWins(t *testing.T) {
	out, err := render(t,
		"--set", "controlPlane.nats.enabled=false",
		"--set", "controlPlane.eventRelay.enabled=true",
		"--set", "controlPlane.eventRelay.natsUrl=nats://ext.example:4222")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `value: "nats://ext.example:4222"`) {
		t.Errorf("external eventRelay.natsUrl did not win in render:\n%s", out)
	}
	if strings.Contains(out, "kind: StatefulSet") {
		t.Errorf("nats.enabled=false must not render a NATS StatefulSet")
	}
}
