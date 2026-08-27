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

package capability

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadMCPConfig reads and validates the projected MCP IR document
// (K8SQUAD_MCP_CONFIG) — the shim entrypoint's startup step. Fail-closed:
// a set-but-unreadable or malformed IR aborts the shim rather than
// serving a Run with a silently missing capability envelope.
func LoadMCPConfig(path string) ([]Endpoint, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the path comes from the operator's own pod env, not Run input
	if err != nil {
		return nil, fmt.Errorf("read mcp config %s: %w", path, err)
	}
	var doc irDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse mcp config %s: %w", path, err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("mcp config %s: unsupported IR version %d (shim understands 1)", path, doc.Version)
	}
	return doc.Endpoints, nil
}
