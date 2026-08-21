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

package rundrive

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// MockClaims implements the Claims interface for testing
type MockClaims struct {
	mock.Mock
}

func (m *MockClaims) State(ctx context.Context, workItemID string) (ClaimState, bool, error) {
	args := m.Called(ctx, workItemID)
	return args.Get(0).(ClaimState), args.Bool(1), args.Error(2)
}

func (m *MockClaims) LapsUsed(ctx context.Context, runID string) (int, error) {
	args := m.Called(ctx, runID)
	return args.Int(0), args.Error(1)
}

func (m *MockClaims) RetryEnter(ctx context.Context, workItemID, runID string, fromFence int64) (int64, bool, error) {
	args := m.Called(ctx, workItemID, runID, fromFence)
	return args.Int64(0), args.Bool(1), args.Error(2)
}

func (m *MockClaims) FailEnter(ctx context.Context, workItemID, runID string, fromFence int64) (bool, error) {
	args := m.Called(ctx, workItemID, runID, fromFence)
	return args.Bool(0), args.Error(1)
}

func (m *MockClaims) CancelFinish(ctx context.Context, workItemID, runID string, fromFence int64) (bool, error) {
	args := m.Called(ctx, workItemID, runID, fromFence)
	return args.Bool(0), args.Error(1)
}

func (m *MockClaims) CancelDue(ctx context.Context) ([]string, error) {
	args := m.Called(ctx)
	return args.Get(0).([]string), args.Error(1)
}

func (m *MockClaims) RequeuePaused(ctx context.Context, workItemID string) (bool, error) {
	args := m.Called(ctx, workItemID)
	return args.Bool(0), args.Error(1)
}

// MockSandboxReleaser implements the SandboxReleaser interface for testing
type MockSandboxReleaser struct {
	mock.Mock
}

func (m *MockSandboxReleaser) Release(ctx context.Context, runID string) error {
	return m.Called(ctx, runID).Error(0)
}

func TestCircuitBreakerLogic(t *testing.T) {
	// Create a fake client
	fakeClient := fake.NewFakeClient()
	
	// Create mock dependencies
	mockClaims := &MockClaims{}
	mockSandbox := &MockSandboxReleaser{}
	
	// Create driver
	driver := NewDriver(fakeClient, mockClaims, nil, nil)
	driver.Now = func() time.Time { return time.Now() }
	
	// Test run
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-namespace",
			UID:       "test-uid",
		},
		Spec: v1alpha1.RunSpec{
			WorkItemRef: "test-work-item",
		},
	}
	
	t.Run("Circuit breaker not activated initially", func(t *testing.T) {
		assert.False(t, driver.isCircuitBreakerActivated("test-uid"))
	})
	
	t.Run("Activate circuit breaker after consecutive failures", func(t *testing.T) {
		// Simulate consecutive failures
		driver.consecutiveFailures["test-uid"] = CircuitBreakerMaxConsecutiveFailures
		
		// Call activate circuit breaker
		result, err := driver.activateCircuitBreaker(context.Background(), "test-uid", "test-namespace/test-run", 2, retryEventTypeTransient)
		
		assert.NoError(t, err)
		assert.Equal(t, time.Duration(CircuitBreakerPauseDuration), result.RequeueAfter)
		assert.True(t, driver.isCircuitBreakerActivated("test-uid"))
		
		// Check that pause time is set in the future
		pauseTime := driver.circuitBreakerPauses["test-uid"]
		assert.True(t, pauseTime.After(time.Now()))
	})
	
	t.Run("Circuit breaker blocks retries when active", func(t *testing.T) {
		// Ensure circuit breaker is active
		assert.True(t, driver.isCircuitBreakerActivated("test-uid"))
		
		// Try to retry - should be blocked by circuit breaker
		cs := ClaimState{
			Step:  reconcile.StepDispatching,
			Fence: 1,
		}
		
		// Mock claims to expect RetryEnter call (should not be called due to circuit breaker)
		mockClaims.On("LapsUsed", context.Background(), "test-uid").Return(2, nil)
		mockClaims.On("RetryEnter", context.Background(), "test-work-item", "test-uid", int64(1)).Return(int64(2), true, nil)
		
		result, err := driver.retryOrFail(context.Background(), run, cs)
		
		// Should return pause duration due to circuit breaker
		assert.NoError(t, err)
		assert.Equal(t, time.Duration(CircuitBreakerPauseDuration), result.RequeueAfter)
		
		// Verify RetryEnter was NOT called
		mockClaims.AssertNotCalled(t, "RetryEnter")
	})
	
	t.Run("Circuit breaker resets after pause period", func(t *testing.T) {
		// Set pause time in the past
		driver.circuitBreakerPauses["test-uid"] = time.Now().Add(-time.Hour)
		
		// Check that circuit breaker is considered expired
		assert.False(t, driver.isCircuitBreakerActivated("test-uid"))
		
		// Reset should work
		driver.resetCircuitBreaker("test-uid")
		assert.False(t, driver.isCircuitBreakerActivated("test-uid"))
		assert.Equal(t, 0, driver.consecutiveFailures["test-uid"])
	})
}

func TestRetryEventLogging(t *testing.T) {
	fakeClient := fake.NewFakeClient()
	driver := NewDriver(fakeClient, nil, nil, nil)
	driver.Now = func() time.Time { return time.Now() }
	
	t.Run("Log retry event", func(t *testing.T) {
		initialCount := len(driver.retryEvents)
		
		nextRetryTime := time.Now().Add(5 * time.Minute)
		driver.logRetryEvent(context.Background(), "test-uid", retryEventTypeTransient, "test_error", 3, true, &nextRetryTime)
		
		assert.Equal(t, initialCount+1, len(driver.retryEvents))
		
		event := driver.retryEvents[len(driver.retryEvents)-1]
		assert.Equal(t, "test-uid", event.RunID)
		assert.Equal(t, retryEventTypeTransient, event.EventType)
		assert.Equal(t, "test_error", event.ErrorType)
		assert.Equal(t, 3, event.Lap)
		assert.True(t, event.Success)
		assert.True(t, event.WillRetry)
		assert.Equal(t, nextRetryTime, *event.NextRetryAt)
	})
	
	t.Run("Cleanup old events", func(t *testing.T) {
		// Add more events than we want to keep
		for i := 0; i < 10; i++ {
			driver.logRetryEvent(context.Background(), "test-uid", retryEventTypeNormal, "error", i, true, nil)
		}
		
		assert.Greater(t, len(driver.retryEvents), 5)
		
		driver.CleanupRetryEvents(5)
		assert.Equal(t, 5, len(driver.retryEvents))
	})
}

func TestMaxRetriesWithDefaults(t *testing.T) {
	fakeClient := fake.NewFakeClient()
	driver := NewDriver(fakeClient, nil, nil, nil)
	
	t.Run("No retry policy", func(t *testing.T) {
		run := &v1alpha1.Run{}
		maxRetries := driver.getMaxRetriesWithDefaults(run)
		assert.Equal(t, 0, maxRetries)
	})
	
	t.Run("Normal retry policy", func(t *testing.T) {
		run := &v1alpha1.Run{
			Spec: v1alpha1.RunSpec{
				RetryPolicy: &v1alpha1.RetryPolicy{
					MaxRetries: func(i int32) *int32 { return &i }(5),
				},
			},
		}
		maxRetries := driver.getMaxRetriesWithDefaults(run)
		assert.Equal(t, 5, maxRetries)
	})
	
	t.Run("High retry policy clamped to safety limit", func(t *testing.T) {
		run := &v1alpha1.Run{
			Spec: v1alpha1.RunSpec{
				RetryPolicy: &v1alpha1.RetryPolicy{
					MaxRetries: func(i int32) *int32 { return &i }(100),
				},
			},
		}
		maxRetries := driver.getMaxRetriesWithDefaults(run)
		assert.Equal(t, 20, maxRetries) // Should be clamped to safety limit
	})
}

func TestErrorCategorization(t *testing.T) {
	fakeClient := fake.NewFakeClient()
	driver := NewDriver(fakeClient, nil, nil, nil)
	
	t.Run("Transient error for active steps", func(t *testing.T) {
		errorType := driver.categorizeError(reconcile.StepDispatching)
		assert.Equal(t, retryEventTypeTransient, errorType)
	})
	
	t.Run("Permanent error for failed steps", func(t *testing.T) {
		errorType := driver.categorizeError(reconcile.StepFailed)
		assert.Equal(t, retryEventTypePermanent, errorType)
	})
	
	t.Run("Normal error for unknown steps", func(t *testing.T) {
		errorType := driver.categorizeError(reconcile.StepPending)
		assert.Equal(t, retryEventTypeNormal, errorType)
	})
}

func TestConsecutiveFailureTracking(t *testing.T) {
	fakeClient := fake.NewFakeClient()
	driver := NewDriver(fakeClient, nil, nil, nil)
	
	t.Run("Track consecutive failures", func(t *testing.T) {
		assert.Equal(t, 0, driver.consecutiveFailures["test-uid"])
		
		driver.consecutiveFailures["test-uid"] = 3
		assert.Equal(t, 3, driver.consecutiveFailures["test-uid"])
		
		failures := driver.GetConsecutiveFailures()
		assert.Equal(t, 3, failures["test-uid"])
	})
}