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

package reconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCancellingStepClassifies (3.3, ISI-2884): the operator-kill transitional
// step is RESUMABLE (a driver crash mid-teardown re-enters and finishes — AC5),
// never terminal, and projects onto the transitional Canceling phase.
func TestCancellingStepClassifies(t *testing.T) {
	assert.Equal(t, ClassResumable, Classify(StepCancelling),
		"cancelling must classify resumable: a crashed teardown re-enters it")
	assert.False(t, IsTerminal(StepCancelling), "cancelling is transitional, not absorbing")
	assert.Equal(t, PhaseCanceling, PhaseOf(StepCancelling),
		"cancelling projects onto the §8 transitional Canceling phase")
}

// TestCancellingNotOnHappyPath: the machine's linear drive never enters
// cancelling — only the explicit fence-first kill transitions do (3.3 is a
// caller decision like the Failed retry edge, not an automatic edge).
func TestCancellingNotOnHappyPath(t *testing.T) {
	for _, s := range happyPath {
		assert.NotEqual(t, StepCancelling, s, "cancelling must not be machine-driven")
	}
}
