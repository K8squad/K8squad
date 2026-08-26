package apiserver

import "testing"

// TestReaderPodConfig_DefaultOff: the 8.7f reader-pod flag is off by default, so the projected
// readerpod.Config degrades to snapshot-only — no opt-in, no pod.
func TestReaderPodConfig_DefaultOff(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.BuildReaderPodEnabled {
		t.Error("BuildReaderPodEnabled must default to false (degrade-by-default, 8.7f)")
	}
	if rp := cfg.ReaderPodConfig(); rp.Enabled {
		t.Error("projected readerpod.Config.Enabled must be false by default")
	}
}

// TestReaderPodConfig_EnvOptIn: the env opt-in flips the flag and carries the reader image through
// to the projected readerpod.Config.
func TestReaderPodConfig_EnvOptIn(t *testing.T) {
	t.Setenv("KSQUAD_BUILD_READER_POD_ENABLED", "true")
	t.Setenv("KSQUAD_BUILD_READER_POD_IMAGE", "ghcr.io/k8squad/build-reader:v1")

	cfg := DefaultConfig()
	cfg.ApplyEnvOverrides()

	if !cfg.BuildReaderPodEnabled {
		t.Fatal("env opt-in did not enable BuildReaderPodEnabled")
	}
	rp := cfg.ReaderPodConfig()
	if !rp.Enabled || rp.ReaderImage != "ghcr.io/k8squad/build-reader:v1" {
		t.Errorf("projected config = %+v, want Enabled=true image=ghcr.io/k8squad/build-reader:v1", rp)
	}
}

// TestReaderPodConfig_EnvExplicitOff: an explicit "false"/"0" keeps the flag off.
func TestReaderPodConfig_EnvExplicitOff(t *testing.T) {
	t.Setenv("KSQUAD_BUILD_READER_POD_ENABLED", "false")
	cfg := DefaultConfig()
	cfg.ApplyEnvOverrides()
	if cfg.BuildReaderPodEnabled {
		t.Error(`KSQUAD_BUILD_READER_POD_ENABLED="false" should keep the flag off`)
	}
}
