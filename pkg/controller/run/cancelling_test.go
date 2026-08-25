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

package run

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/stretchr/testify/assert"
)

// TestCancellingProjection (3.3, ISI-2884): the durable cancelling step
// projects the transitional Canceling phase with a Cancelling Ready reason —
// the console's kill feedback ("Canceling") is the phase the durable step
// commands, and the terminal cancelled keeps its distinct reason.
func TestCancellingProjection(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC))

	got := ProjectStatus(api.RunStatus{}, reconcile.StepCancelling, 7, now)
	assert.Equal(t, api.RunPhaseCanceling, got.Phase, "cancelling step projects Canceling")
	cond := conditionOf(got, ConditionReady)
	assert.Equal(t, "False", string(cond.Status))
	assert.Equal(t, "Cancelling", cond.Reason)
	assert.Equal(t, int64(7), cond.ObservedGeneration)

	// Bridge symmetry: PhaseOf maps the machine phase onto the CRD enum.
	assert.Equal(t, api.RunPhaseCanceling, PhaseOf(reconcile.StepCancelling))
}

// TestCancelTerminalProjection: the finished kill stays terminal Cancelled —
// the finish transition's projection is stable across requeues (idempotent
// projection, byte-identical status on a no-op pass).
func TestCancelTerminalProjection(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC))
	got := ProjectStatus(api.RunStatus{}, reconcile.StepCancelled, 1, now)
	assert.Equal(t, api.RunPhaseCancelled, got.Phase)
	assert.Equal(t, "Cancelled", conditionOf(got, ConditionReady).Reason)
}

func conditionOf(s api.RunStatus, t string) metav1.Condition {
	for _, c := range s.Conditions {
		if c.Type == t {
			return c
		}
	}
	return metav1.Condition{}
}
