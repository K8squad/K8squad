package covgate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCovGateRatchet is the ratchet mode's teeth (ISI-2852 interim L5 posture).
// Every subtest pins a GREEN/RED verdict the same way TestCovGate pins target
// mode, so the ratchet cannot be filed off unnoticed either:
//   - stop failing on a below-baseline package → ratchet_below_baseline flips GREEN.
//   - let unknown packages pass silently → ratchet_unbaselined_package flips GREEN.
//   - make the boundary exclusive → ratchet_at_baseline_exact flips RED.
//   - drop the ErrNoProfile guard in EvaluateRatchet → ratchet_empty_profile stops failing.
//   - round floors UP in WriteRatchetBaseline → ratchet_write_floors_down fails
//     (a display-rounded 89.6 measured as 89.55 would false-RED the next run).
func TestCovGateRatchet(t *testing.T) {
	baseline := RatchetBaseline{
		mod + "pkg/warmpool": 89.5,
		mod + "pkg/coord":    9.0,
		mod + "internal/x":   0.0,
	}

	// -- every package at/above its entry -> GREEN --
	t.Run("ratchet_all_clear", func(t *testing.T) {
		pkgs := []PackageCoverage{
			{Package: mod + "pkg/warmpool", Percent: 90.0, Statements: 100},
			{Package: mod + "pkg/coord", Percent: 9.05, Statements: 100},
			{Package: mod + "internal/x", Percent: 0.0, Statements: 5},
		}
		ok, fails, stale, err := EvaluateRatchet(pkgs, baseline)
		if err != nil || !ok {
			t.Fatalf("expected GREEN, got ok=%v err=%v fails=%v", ok, err, fails)
		}
		if len(stale) != 0 {
			t.Fatalf("expected no stale entries, got %v", stale)
		}
	})

	// -- the tooth: a package dips below its pinned entry -> RED --
	t.Run("ratchet_below_baseline", func(t *testing.T) {
		pkgs := []PackageCoverage{
			{Package: mod + "pkg/warmpool", Percent: 89.4, Statements: 100}, // 89.4 < 89.5
		}
		ok, fails, _, err := EvaluateRatchet(pkgs, baseline)
		if err != nil || ok {
			t.Fatalf("expected RED, got ok=%v err=%v", ok, err)
		}
		if len(fails) != 1 || fails[0].Package != mod+"pkg/warmpool" || fails[0].Floor != 89.5 {
			t.Fatalf("expected warmpool@89.5 failure, got %v", fails)
		}
	})

	// -- no-silent-green tooth: a package in the profile but NOT in the baseline -> RED --
	t.Run("ratchet_unbaselined_package", func(t *testing.T) {
		pkgs := []PackageCoverage{
			{Package: mod + "pkg/brandnew", Percent: 95.0, Statements: 10}, // covered, but unknown
		}
		ok, fails, _, err := EvaluateRatchet(pkgs, baseline)
		if err != nil || ok {
			t.Fatalf("expected RED for unbaselined package, got ok=%v err=%v", ok, err)
		}
		if len(fails) != 1 || !fails[0].Unbaselined {
			t.Fatalf("expected one Unbaselined failure, got %v", fails)
		}
		if !strings.Contains(fails[0].String(), "no ratchet baseline entry") {
			t.Fatalf("failure text must name the remedy, got: %s", fails[0])
		}
	})

	// -- boundary: exactly at baseline PASSES (inclusive, matching Evaluate) --
	t.Run("ratchet_at_baseline_exact", func(t *testing.T) {
		pkgs := []PackageCoverage{
			{Package: mod + "pkg/warmpool", Percent: 89.5, Statements: 100},
			{Package: mod + "pkg/coord", Percent: 9.0, Statements: 100},
			{Package: mod + "internal/x", Percent: 0.0, Statements: 5}, // 0.0 entry is a valid floor
		}
		ok, fails, _, err := EvaluateRatchet(pkgs, baseline)
		if err != nil || !ok {
			t.Fatalf("expected GREEN at exact baseline, got ok=%v err=%v fails=%v", ok, err, fails)
		}
	})

	// -- a deleted package (baseline entry with no profile appearance) is a
	//    notice, not a failure --
	t.Run("ratchet_stale_entry_is_notice", func(t *testing.T) {
		pkgs := []PackageCoverage{
			{Package: mod + "pkg/warmpool", Percent: 90.0, Statements: 100},
		}
		ok, fails, stale, err := EvaluateRatchet(pkgs, baseline)
		if err != nil || !ok {
			t.Fatalf("expected GREEN despite stale entries, got ok=%v err=%v fails=%v", ok, err, fails)
		}
		// pkg/coord + internal/x are both absent now.
		if len(stale) != 2 {
			t.Fatalf("expected 2 stale entries, got %v", stale)
		}
	})

	// -- can't-measure = build FAILURE, in ratchet mode too --
	t.Run("ratchet_empty_profile", func(t *testing.T) {
		_, _, _, err := EvaluateRatchet(nil, baseline)
		if !errors.Is(err, ErrNoProfile) {
			t.Fatalf("empty profile must fail with ErrNoProfile, got %v", err)
		}
	})
}

// TestRatchetBaselineRoundTrip pins the JSON contract: Write produces sorted,
// 1dp-floored, deterministic output that Load reads back exactly.
func TestRatchetBaselineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")

	pkgs := []PackageCoverage{
		{Package: mod + "pkg/zeta", Percent: 91.17, Statements: 100},
		{Package: mod + "pkg/alpha", Percent: 45.0, Statements: 40},
		{Package: mod + "pkg/mid", Percent: 0.04, Statements: 25},
	}
	if err := WriteRatchetBaseline(path, pkgs); err != nil {
		t.Fatalf("WriteRatchetBaseline: %v", err)
	}

	got, err := LoadRatchetBaseline(path)
	if err != nil {
		t.Fatalf("LoadRatchetBaseline: %v", err)
	}
	want := RatchetBaseline{
		mod + "pkg/zeta":  91.1, // 91.17 floors DOWN to 91.1, never up
		mod + "pkg/alpha": 45.0,
		mod + "pkg/mid":   0.0, // 0.04 floors to 0.0
	}
	if len(got) != len(want) {
		t.Fatalf("round-trip lost entries: got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("round-trip mismatch for %s: got %v want %v", k, got[k], v)
		}
	}

	// Deterministic + sorted: rewriting identical input yields identical bytes.
	first, _ := os.ReadFile(path)
	if err := WriteRatchetBaseline(path, pkgs); err != nil {
		t.Fatalf("second WriteRatchetBaseline: %v", err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("baseline output must be deterministic:\n%s\n---\n%s", first, second)
	}
	raw := strings.TrimSpace(string(first))
	if !strings.HasPrefix(raw, "{") || !strings.HasSuffix(raw, "}") {
		t.Fatalf("baseline must be a bare JSON object, got: %s", raw[:1])
	}
}

// TestRatchetWriteFloorsDown is the anti-false-RED tooth: a package measured
// 89.55% (displayed "89.6%") must pin at 89.5 — pinning 89.6 would false-RED
// the very next identical run.
func TestRatchetWriteFloorsDown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	pkgs := []PackageCoverage{{Package: mod + "pkg/warmpool", Percent: 89.55, Statements: 1000}}
	if err := WriteRatchetBaseline(path, pkgs); err != nil {
		t.Fatalf("WriteRatchetBaseline: %v", err)
	}
	got, err := LoadRatchetBaseline(path)
	if err != nil {
		t.Fatalf("LoadRatchetBaseline: %v", err)
	}
	if got[mod+"pkg/warmpool"] != 89.5 {
		t.Fatalf("89.55%% must floor to 89.5, got %v", got[mod+"pkg/warmpool"])
	}
}
