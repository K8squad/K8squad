// Guard test for the schema-migration rollout gate (ISI-7109). The bug (found in
// ISI-7104): the ISI-7088 observability operator image rolled to k8squad-test and
// began serving terminal-settle reconciles against coord.comment.kind/.run_id
// BEFORE 0042_comment_kind had applied, producing ~7 minutes of hard 42703
// "column does not exist" Reconciler errors until the migration self-healed.
//
// The fix renders a Helm pre-install/pre-upgrade hook Job (ksquad-migrate) that
// runs the apiserver image with --migrate-only to completion BEFORE the operator
// and apiserver Deployments roll. These tests pin the hook contract: it renders
// by default with the right hook/weight/delete-policy annotations and the
// --migrate-only arg, it is suppressible via controlPlane.migrations.enabled, and
// on the dev inline-DSN path the chart-owned Secret leads the hook sequence so the
// Job can read DATABASE_URL on a fresh install.
//
// Reuses render() from nats_wiring_test.go, which skips when `helm` is absent.
package helm

import (
	"strings"
	"testing"
)

// migrateJobDoc returns the ksquad-migrate Job document from a chart render, or ""
// if no such Job is present.
func migrateJobDoc(out string) string {
	for _, doc := range strings.Split(out, "\n---") {
		if strings.Contains(doc, "kind: Job") && strings.Contains(doc, "name: ksquad-migrate") {
			return doc
		}
	}
	return ""
}

func TestMigrateHookRendersByDefault(t *testing.T) {
	out, err := render(t, "--set", "controlPlane.nats.persistence.storageClassName=nfs-csi")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	job := migrateJobDoc(out)
	if job == "" {
		t.Fatalf("migration hook Job ksquad-migrate not rendered by default:\n%s", out)
	}
	for _, want := range []string{
		`"helm.sh/hook": pre-install,pre-upgrade`, // gate both install and upgrade
		`"helm.sh/hook-weight": "-5"`,             // run before the Deployments roll
		`"helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded`,
		"- --migrate-only",                        // migrate-then-exit entrypoint
		"image: ghcr.io/k8squad/ksquad-apiserver", // reuses the apiserver image (embeds migrations)
		"name: DATABASE_URL",                      // same DSN the apiserver reads
		"restartPolicy: Never",
	} {
		if !strings.Contains(job, want) {
			t.Errorf("migration hook Job missing %q:\n%s", want, job)
		}
	}
}

func TestMigrateHookSuppressible(t *testing.T) {
	out, err := render(t,
		"--set", "controlPlane.nats.persistence.storageClassName=nfs-csi",
		"--set", "controlPlane.migrations.enabled=false")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	if job := migrateJobDoc(out); job != "" {
		t.Errorf("controlPlane.migrations.enabled=false must suppress the Job, got:\n%s", job)
	}
}

// TestMigrateHookSecretOrdering guards the fresh-install dev path: the inline-DSN
// Secret must carry an EARLIER hook weight than the Job (-10 < -5) so it exists
// before the pre-install hook reads DATABASE_URL. render() uses the inline DSN,
// so the chart-owned Secret is the one under test here.
func TestMigrateHookSecretOrdering(t *testing.T) {
	out, err := render(t, "--set", "controlPlane.nats.persistence.storageClassName=nfs-csi")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	var secret string
	for _, doc := range strings.Split(out, "\n---") {
		if strings.Contains(doc, "kind: Secret") && strings.Contains(doc, "name: k8squad-database") {
			secret = doc
			break
		}
	}
	if secret == "" {
		t.Fatalf("chart-owned dev DSN Secret not rendered on the inline-DSN path:\n%s", out)
	}
	for _, want := range []string{
		`"helm.sh/hook": pre-install,pre-upgrade`,
		`"helm.sh/hook-weight": "-10"`, // leads the Job (-5)
		`"helm.sh/hook-delete-policy": before-hook-creation`,
	} {
		if !strings.Contains(secret, want) {
			t.Errorf("dev DSN Secret missing hook annotation %q (must lead the migrate Job):\n%s", want, secret)
		}
	}
}
