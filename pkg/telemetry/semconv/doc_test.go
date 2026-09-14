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

package ksqsemconv_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	ksqsemconv "github.com/K8squad/K8squad/pkg/telemetry/semconv"
)

var updateDocs = flag.Bool("update-semconv-docs", false, "regenerate docs/observability/semantic-conventions.md from the registry")

// docPath is the committed markdown reference, relative to this package.
const docPath = "../../../docs/observability/semantic-conventions.md"

// TestDocInSync keeps docs/observability/semantic-conventions.md byte-equal to
// the registry-derived Render() output. Run with -update-semconv-docs after
// changing registry.go to regenerate it.
func TestDocInSync(t *testing.T) {
	want := ksqsemconv.Render()

	if *updateDocs {
		if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
			t.Fatalf("mkdir docs dir: %v", err)
		}
		if err := os.WriteFile(docPath, []byte(want), 0o644); err != nil {
			t.Fatalf("write doc: %v", err)
		}
		t.Logf("regenerated %s", docPath)
		return
	}

	got, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v (run: go test ./pkg/telemetry/semconv/ -run TestDocInSync -update-semconv-docs)", docPath, err)
	}
	if string(got) != want {
		t.Errorf("%s is out of sync with the registry.\nRegenerate: go test ./pkg/telemetry/semconv/ -run TestDocInSync -update-semconv-docs", docPath)
	}
}
