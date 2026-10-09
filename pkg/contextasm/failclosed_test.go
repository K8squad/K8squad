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

package contextasm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsFailClosed pins the ISI-5543 circuit-breaker classifier: the two
// deterministic fail-closed assembly/budget sentinels are non-retryable, every
// other error is retryable, and the verdict survives the deep wrap chain the
// error travels from ApplyBudget back to the driver (assembler → buildTask →
// a2a submit → effects). A false positive would park a transiently-failing item
// forever; a false negative re-opens the runaway re-dispatch loop.
func TestIsFailClosed(t *testing.T) {
	t.Run("must-include overflow is fail-closed", func(t *testing.T) {
		assert.True(t, IsFailClosed(ErrMustIncludeExceedsWindow))
	})
	t.Run("over-window budget tier is fail-closed", func(t *testing.T) {
		assert.True(t, IsFailClosed(ErrBudgetAboveWindow))
	})
	t.Run("deeply wrapped sentinel is still fail-closed", func(t *testing.T) {
		// The exact wrapping depth the live error travels: ApplyBudget's own %w,
		// then buildTask, a2a.Submit, and the effects fail(), each adding a %w.
		wrapped := fmt.Errorf("rundrive: effects for run x: %w",
			fmt.Errorf("coord.ProdEffects.Dispatch: submit: %w",
				fmt.Errorf("a2a: build task for run r: %w",
					fmt.Errorf("rundrive: assemble system context for run r: %w",
						fmt.Errorf("%w: must-include 65597 tokens > window 65467", ErrMustIncludeExceedsWindow)))))
		assert.True(t, IsFailClosed(wrapped))
	})
	t.Run("a transient error is retryable", func(t *testing.T) {
		assert.False(t, IsFailClosed(errors.New("connection refused")))
		assert.False(t, IsFailClosed(fmt.Errorf("read Agent: %w", errors.New("etcdserver: leader changed"))))
	})
	t.Run("nil is retryable (not fail-closed)", func(t *testing.T) {
		assert.False(t, IsFailClosed(nil))
	})
}
