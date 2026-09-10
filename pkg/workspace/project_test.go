/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workspace

import (
	"strings"
	"testing"
)

// TestProjectPVCName: the per-Project claim name is deterministic, fits the
// DNS-1123 cap even for over-long Project names, and two long names sharing
// a prefix still disambiguate through the hash suffix (ISI-4127).
func TestProjectPVCName(t *testing.T) {
	if got := ProjectPVCName("widget"); got != "workspace-project-widget" {
		t.Fatalf("ProjectPVCName(widget) = %q", got)
	}
	// Deterministic.
	first := ProjectPVCName("widget")
	if first != ProjectPVCName("widget") {
		t.Fatal("ProjectPVCName is not deterministic")
	}

	long := strings.Repeat("a", 60)
	got := ProjectPVCName(long)
	if len(got) > 63 {
		t.Fatalf("over-long name produced %d-char PVC name %q", len(got), got)
	}
	// Same 44-char kept prefix, distinct tails → distinct hashes → distinct names.
	other := strings.Repeat("a", 59) + "b"
	if ProjectPVCName(long) == ProjectPVCName(other) {
		t.Fatal("truncated names collided; hash disambiguation failed")
	}
}
