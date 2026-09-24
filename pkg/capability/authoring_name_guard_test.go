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
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/K8squad/K8squad/pkg/controller/team"
)

// TestAuthoringServerNamePinnedToProvisioner pins the S2 gate's built-in
// server name to the S1 provisioner's canonical constant. capability keeps its
// own unexported copy to avoid a lib→controller import at runtime; this
// test-only import makes the two impossible to drift apart. If S1 ever renames
// the provisioned MCPServer, this fails loudly instead of the gate silently
// looking up a server that no longer exists (which would deny-by-default and
// break authoring for every granted Run).
func TestAuthoringServerNamePinnedToProvisioner(t *testing.T) {
	assert.Equal(t, team.AuthoringMCPServerName, authoringMCPServerName)
}
