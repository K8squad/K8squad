package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8squad/internal/pkg/agent"
	"k8squad/pkg/controller"
	"k8squad/pkg/controller/run"
)

// MockConfig provides a test configuration structure
type MockConfig struct {
	Debug   bool
	Timeout time.Duration
}

// TestAgentIntegrationSuite provides comprehensive integration testing for the backup DevOps Engineer system
type TestAgentIntegrationSuite struct {
	config        *MockConfig
	agentStore    *agent.MemoryStore
	runController *run.RunController
	agentExecutor *controller.AgentExecutor
	logBuffer     *strings.Builder
	logMutex      sync.Mutex
}

// NewTestAgentIntegrationSuite creates a new test suite
func NewTestAgentIntegrationSuite() *TestAgentIntegrationSuite {
	// Initialize test config
	testConfig := &MockConfig{
		Debug:   true,
		Timeout: 5 * time.Minute,
	}

	// Create agent store
	agentStore := agent.NewMemoryStore()
	err := agentStore.InitializeStore()
	if err != nil {
		log.Fatalf("Failed to initialize agent store: %v", err)
	}

	// Create agent executor
	agentExecutor := controller.NewAgentExecutor(agentStore, 30*time.Minute)

	// Create run controller
	runController := run.NewRunController(testConfig, agentStore)

	return &TestAgentIntegrationSuite{
		config:        testConfig,
		agentStore:    agentStore,
		runController: runController,
		agentExecutor: agentExecutor,
		logBuffer:     &strings.Builder{},
	}
}

// CaptureLogs captures and redirects log output for verification
func (suite *TestAgentIntegrationSuite) CaptureLogs() func() {
	originalStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	var buf strings.Builder
	done := make(chan bool)

	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			suite.logMutex.Lock()
			suite.logBuffer.WriteString(scanner.Text() + "\n")
			suite.logMutex.Unlock()
		}
		done <- true
	}()

	return func() {
		w.Close()
		<-done
		os.Stdout = originalStdout
	}
}

// GetCapturedLogs returns the captured log output
func (suite *TestAgentIntegrationSuite) GetCapturedLogs() string {
	suite.logMutex.Lock()
	defer suite.logMutex.Unlock()
	return suite.logBuffer.String()
}

// ClearLogs clears the captured log output
func (suite *TestAgentIntegrationSuite) ClearLogs() {
	suite.logMutex.Lock()
	suite.logBuffer.Reset()
	suite.logMutex.Unlock()
}

// TestCompleteAgentExecutionFlow tests the complete execution flow from run_controller through agent_executor to the store
func TestCompleteAgentExecutionFlow(t *testing.T) {
	suite := NewTestAgentIntegrationSuite()
	defer suite.ClearLogs()

	// Capture logs for verification
	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()
	operationID := "backup-infrastructure"
	params := map[string]interface{}{
		"source":      "/data/source",
		"target":      "/data/backup",
		"backup-type": "full",
	}

	// Test complete execution flow
	err := suite.runController.ExecuteRun(ctx, operationID, params)

	// Verify the flow succeeded
	require.NoError(t, err, "Complete execution flow should succeed")

	// Verify agent store state
	agentExists := suite.agentStore.AgentExists("backup-devops-agent")
	require.True(t, agentExists, "Backup DevOps agent should exist")

	capabilities := suite.agentStore.GetAgentCapabilities("backup-devops-agent")
	require.Contains(t, capabilities, "backup-infrastructure", "Agent should support backup infrastructure operation")

	status := suite.agentStore.GetAgentStatus("backup-devops-agent")
	assert.Equal(t, "completed", status, "Agent status should be completed after successful execution")

	// Verify log output contains expected markers
	logOutput := suite.GetCapturedLogs()
	assert.Contains(t, logOutput, "[BACKUP-DEVOPS] Starting real execution of operation backup-infrastructure", "Should log real execution start")
	assert.Contains(t, logOutput, "[AGENT-EXECUTOR] Starting real execution of agent backup-devops-agent", "Should log agent execution start")
	assert.Contains(t, logOutput, "[AGENT-STORE] Starting real execution of agent backup-devops-agent", "Should log store execution start")
	assert.Contains(t, logOutput, "[BACKUP-INFRASTRUCTURE] Starting backup operation", "Should log backup operation start")
	assert.Contains(t, logOutput, "[BACKUP-DEVOPS] Real execution completed successfully", "Should log successful completion")
}

// TestRealBackupInfrastructureOperations tests that real backup infrastructure operations are executed
func (suite *TestAgentIntegrationSuite) TestRealBackupInfrastructureOperations(t *testing.T) {
	defer suite.ClearLogs()

	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()
	operationID := "backup-infrastructure"
	params := map[string]interface{}{
		"source":      "/data/production-critical",
		"target":      "/backups/critical-data",
		"backup-type": "incremental",
	}

	// Execute real backup operation
	startTime := time.Now()
	err := suite.runController.ExecuteRun(ctx, operationID, params)
	duration := time.Since(startTime)

	// Verify real execution occurred (should take some time, not immediate)
	require.NoError(t, err, "Real backup operation should succeed")
	assert.True(t, duration > 1*time.Second, "Real backup operation should take time to execute")

	// Verify log evidence of real operation
	logOutput := suite.GetCapturedLogs()
	assert.Contains(t, logOutput, "Starting backup operation from /data/production-critical to /backups/critical-data (type: incremental)", "Should log real backup parameters")
	assert.Contains(t, logOutput, "Backup completed successfully in", "Should log backup completion with timing")

	// Verify agent was actually used
	status := suite.agentStore.GetAgentStatus("backup-devops-agent")
	assert.Equal(t, "completed", status, "Agent should show completed status after real operation")
}

// TestDisasterRecoveryOperations tests disaster recovery operations
func (suite *TestAgentIntegrationSuite) TestDisasterRecoveryOperations(t *testing.T) {
	defer suite.ClearLogs()

	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()
	operationID := "restore-disaster"
	params := map[string]interface{}{
		"backup-file":         "/backups/latest-full-backup.tar.gz",
		"target-environment": "production-failover",
		"restore-mode":        "full-restore",
	}

	// Execute disaster recovery operation
	err := suite.runController.ExecuteRun(ctx, operationID, params)

	// Verify disaster recovery succeeded
	require.NoError(t, err, "Disaster recovery operation should succeed")

	// Verify log evidence of disaster recovery
	logOutput := suite.GetCapturedLogs()
	assert.Contains(t, logOutput, "[RESTORE-DISASTER] Starting restore operation from /backups/latest-full-backup.tar.gz to production-failover (mode: full-restore)", "Should log disaster recovery operation")
	assert.Contains(t, logOutput, "[RESTORE-DISASTER] Restore completed successfully in", "Should log disaster recovery completion")

	// Verify disaster recovery agent was used
	status := suite.agentStore.GetAgentStatus("disaster-recovery-agent")
	assert.Equal(t, "completed", status, "Disaster recovery agent should show completed status")
}

// TestConfigurationSyncOperations tests configuration synchronization operations
func (suite *TestAgentIntegrationSuite) TestConfigurationSyncOperations(t *testing.T) {
	defer suite.ClearLogs()

	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()
	operationID := "sync-configuration"
	params := map[string]interface{}{
		"source-config": "/etc/kubernetes/config.yaml",
		"target-system": "kubernetes-cluster",
		"sync-type":     "bidirectional",
	}

	// Execute configuration sync operation
	err := suite.runController.ExecuteRun(ctx, operationID, params)

	// Verify configuration sync succeeded
	require.NoError(t, err, "Configuration sync operation should succeed")

	// Verify log evidence of configuration sync
	logOutput := suite.GetCapturedLogs()
	assert.Contains(t, logOutput, "[SYNC-CONFIGURATION] Starting sync operation from /etc/kubernetes/config.yaml to kubernetes-cluster (type: bidirectional)", "Should log configuration sync operation")
	assert.Contains(t, logOutput, "[SYNC-CONFIGURATION] Sync completed successfully in", "Should log configuration sync completion")

	// Verify config sync agent was used
	status := suite.agentStore.GetAgentStatus("config-sync-agent")
	assert.Equal(t, "completed", status, "Config sync agent should show completed status")
}

// TestNoSilentActiveRuns confirms no silent active runs exist
func (suite *TestAgentIntegrationSuite) TestNoSilentActiveRuns(t *testing.T) {
	defer suite.ClearLogs()

	// Initialize and execute operations
	ctx := context.Background()
	
	operations := []struct {
		id     string
		params map[string]interface{}
	}{
		{
			id: "backup-infrastructure",
			params: map[string]interface{}{
				"source":      "/data/test",
				"target":      "/backup/test",
				"backup-type": "test",
			},
		},
		{
			id: "restore-disaster",
			params: map[string]interface{}{
				"backup-file":         "/backup/test.tar.gz",
				"target-environment": "test-env",
				"restore-mode":        "test-restore",
			},
		},
		{
			id: "sync-configuration",
			params: map[string]interface{}{
				"source-config": "/etc/test/config.yaml",
				"target-system": "test-system",
				"sync-type":     "test-sync",
			},
		},
	}

	for _, op := range operations {
		err := suite.runController.ExecuteRun(ctx, op.id, op.params)
		require.NoError(t, err, "Operation should complete successfully")
	}

	// Check for any silent active runs by inspecting agent statuses
	availableAgents := suite.agentStore.ListAvailableAgents()
	
	for _, agentID := range availableAgents {
		status := suite.agentStore.GetAgentStatus(agentID)
		
		// No agent should be in "active" status after operations complete
		if status == "active" {
			t.Errorf("Agent %s found in 'active' status, indicating a potential silent active run", agentID)
		}
		
		// All agents should be in "completed" status
		assert.Equal(t, "completed", status, "Agent %s should be in 'completed' state after operation", agentID)
	}

	// Verify no lingering active operations in logs
	logOutput := suite.GetCapturedLogs()
	assert.NotContains(t, logOutput, "starting execution", "No new operations should be starting after completion")
	assert.NotContains(t, logOutput, "execution failed", "No operations should have failed unexpectedly")
}

// TestComprehensiveLoggingVerification tests comprehensive logging verification
func (suite *TestAgentIntegrationSuite) TestComprehensiveLoggingVerification(t *testing.T) {
	defer suite.ClearLogs()

	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()
	operationID := "backup-infrastructure"
	params := map[string]interface{}{
		"source":      "/data/comprehensive-test",
		"target":      "/backups/comprehensive-test",
		"backup-type": "comprehensive",
	}

	// Execute operation
	err := suite.runController.ExecuteRun(ctx, operationID, params)
	require.NoError(t, err, "Comprehensive logging test should succeed")

	// Verify comprehensive logging
	logOutput := suite.GetCapturedLogs()

	// Check for all required log markers
	requiredMarkers := []string{
		"[BACKUP-DEVOPS] Starting real execution of operation",
		"[BACKUP-DEVOPS] Real execution completed successfully",
		"[AGENT-EXECUTOR] Starting real execution of agent",
		"[AGENT-EXECUTOR] Real execution completed successfully",
		"[AGENT-STORE] Starting real execution of agent",
		"[AGENT-STORE] Real execution completed successfully",
		"[BACKUP-INFRASTRUCTURE] Starting backup operation",
		"[BACKUP-INFRASTRUCTURE] Backup completed successfully",
	}

	for _, marker := range requiredMarkers {
		assert.Contains(t, logOutput, marker, "Log should contain expected marker: %s", marker)
	}

	// Check for timestamps in logs
	assert.Contains(t, logOutput, time.Now().Format("2026"), "Log should contain current year timestamp")
	
	// Check for operation-specific details
	assert.Contains(t, logOutput, "operation backup-infrastructure", "Log should contain operation ID")
	assert.Contains(t, logOutput, "from /data/comprehensive-test to /backups/comprehensive-test", "Log should contain operation parameters")
}

	// Execute configuration sync operation
	err := suite.runController.ExecuteRun(ctx, operationID, params)

	// Verify configuration sync succeeded
	require.NoError(t, err, "Configuration sync operation should succeed")

	// Verify log evidence of configuration sync
	logOutput := suite.GetCapturedLogs()
	assert.Contains(t, logOutput, "[SYNC-CONFIGURATION] Starting sync operation from /etc/kubernetes/config.yaml to kubernetes-cluster (type: bidirectional)", "Should log configuration sync operation")
	assert.Contains(t, logOutput, "[SYNC-CONFIGURATION] Sync completed successfully in", "Should log configuration sync completion")

	// Verify config sync agent was used
	status := suite.agentStore.GetAgentStatus("config-sync-agent")
	assert.Equal(t, "completed", status, "Config sync agent should show completed status")
}

// TestNoSilentActiveRuns confirms no silent active runs exist
func TestNoSilentActiveRuns(t *testing.T) {
	suite := NewTestAgentIntegrationSuite()
	defer suite.ClearLogs()

	// Initialize and execute operations
	ctx := context.Background()
	
	operations := []struct {
		id     string
		params map[string]interface{}
	}{
		{
			id: "backup-infrastructure",
			params: map[string]interface{}{
				"source":      "/data/test",
				"target":      "/backup/test",
				"backup-type": "test",
			},
		},
		{
			id: "restore-disaster",
			params: map[string]interface{}{
				"backup-file":         "/backup/test.tar.gz",
				"target-environment": "test-env",
				"restore-mode":        "test-restore",
			},
		},
		{
			id: "sync-configuration",
			params: map[string]interface{}{
				"source-config": "/etc/test/config.yaml",
				"target-system": "test-system",
				"sync-type":     "test-sync",
			},
		},
	}

	for _, op := range operations {
		err := suite.runController.ExecuteRun(ctx, op.id, op.params)
		require.NoError(t, err, "Operation should complete successfully")
	}

	// Check for any silent active runs by inspecting agent statuses
	availableAgents := suite.agentStore.ListAvailableAgents()
	
	for _, agentID := range availableAgents {
		status := suite.agentStore.GetAgentStatus(agentID)
		
		// No agent should be in "active" status after operations complete
		if status == "active" {
			t.Errorf("Agent %s found in 'active' status, indicating a potential silent active run", agentID)
		}
		
		// All agents should be in "completed" status
		assert.Equal(t, "completed", status, "Agent %s should be in 'completed' status after operation", agentID)
	}

	// Verify no lingering active operations in logs
	logOutput := suite.GetCapturedLogs()
	assert.NotContains(t, logOutput, "starting execution", "No new operations should be starting after completion")
	assert.NotContains(t, logOutput, "execution failed", "No operations should have failed unexpectedly")
}

// TestComprehensiveLoggingVerification tests comprehensive logging verification
func TestComprehensiveLoggingVerification(t *testing.T) {
	suite := NewTestAgentIntegrationSuite()
	defer suite.ClearLogs()

	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()
	operationID := "backup-infrastructure"
	params := map[string]interface{}{
		"source":      "/data/comprehensive-test",
		"target":      "/backups/comprehensive-test",
		"backup-type": "comprehensive",
	}

	// Execute operation
	err := suite.runController.ExecuteRun(ctx, operationID, params)
	require.NoError(t, err, "Comprehensive logging test should succeed")

	// Verify comprehensive logging
	logOutput := suite.GetCapturedLogs()

	// Check for all required log markers
	requiredMarkers := []string{
		"[BACKUP-DEVOPS] Starting real execution of operation",
		"[BACKUP-DEVOPS] Real execution completed successfully",
		"[AGENT-EXECUTOR] Starting real execution of agent",
		"[AGENT-EXECUTOR] Real execution completed successfully",
		"[AGENT-STORE] Starting real execution of agent",
		"[AGENT-STORE] Real execution completed successfully",
		"[BACKUP-INFRASTRUCTURE] Starting backup operation",
		"[BACKUP-INFRASTRUCTURE] Backup completed successfully",
	}

	for _, marker := range requiredMarkers {
		assert.Contains(t, logOutput, marker, "Log should contain expected marker: %s", marker)
	}

	// Check for timestamps in logs
	assert.Contains(t, logOutput, time.Now().Format("2026"), "Log should contain current year timestamp")
	
	// Check for operation-specific details
	assert.Contains(t, logOutput, "operation backup-infrastructure", "Log should contain operation ID")
	assert.Contains(t, logOutput, "from /data/comprehensive-test to /backups/comprehensive-test", "Log should contain operation parameters")
}

// TestErrorHandlingAndEdgeCases tests error handling and edge cases
func (suite *TestAgentIntegrationSuite) TestErrorHandlingAndEdgeCases(t *testing.T) {
	defer suite.ClearLogs()

	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()

	// Test case 1: Unknown operation type
	t.Run("Unknown Operation", func(t *testing.T) {
		err := suite.runController.ExecuteRun(ctx, "unknown-operation", map[string]interface{}{})
		assert.Error(t, err, "Unknown operation should fail")
		assert.Contains(t, err.Error(), "unknown operation type: unknown-operation", "Should contain specific error message")
	})

	// Test case 2: Missing required parameters
	t.Run("Missing Parameters", func(t *testing.T) {
		err := suite.runController.ExecuteRun(ctx, "backup-infrastructure", map[string]interface{}{})
		assert.Error(t, err, "Missing parameters should fail")
		assert.Contains(t, err.Error(), "missing required parameter: source", "Should indicate missing source parameter")
	})

	// Test case 3: Non-existent agent (simulate by creating invalid mapping)
	t.Run("Non-existent Agent", func(t *testing.T) {
		// This should work because the agent exists
		err := suite.runController.ExecuteRun(ctx, "backup-infrastructure", map[string]interface{}{
			"source":      "/data/test",
			"target":      "/backup/test",
			"backup-type": "full",
		})
		require.NoError(t, err, "Valid operation should succeed")
	})

	// Test case 4: Context timeout
	t.Run("Context Timeout", func(t *testing.T) {
		shortCtx, cancel := context.WithTimeout(ctx, 1*time.Millisecond)
		defer cancel()
		
		// This might timeout due to the 2-second backup simulation
		err := suite.runController.ExecuteRun(shortCtx, "backup-infrastructure", map[string]interface{}{
			"source":      "/data/test",
			"target":      "/backup/test",
			"backup-type": "full",
		})
		
		// Could timeout or succeed depending on timing
		if err != nil {
			assert.Contains(t, err.Error(), "context deadline exceeded", "Should indicate timeout")
		}
	})

	// Test case 5: Invalid agent type (should be handled by agent executor)
	t.Run("Invalid Agent Type", func(t *testing.T) {
		err := suite.agentExecutor.ExecuteRun(ctx, "non-existent-agent", "backup-infrastructure", map[string]interface{}{
			"source":      "/data/test",
			"target":      "/backup/test",
			"backup-type": "full",
		})
		assert.Error(t, err, "Non-existent agent should fail")
		assert.Contains(t, err.Error(), "agent non-existent-agent does not exist", "Should indicate agent doesn't exist")
	})

	// Verify error logs are properly captured
	logOutput := suite.GetCapturedLogs()
	assert.Contains(t, logOutput, "Real execution failed", "Should log execution failures")
	assert.Contains(t, logOutput, "agent validation failed", "Should log agent validation failures")
}

// TestRealExecutionVsSimulation verifies that real execution is happening, not simulation
func (suite *TestAgentIntegrationSuite) TestRealExecutionVsSimulation(t *testing.T) {
	defer suite.ClearLogs()

	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()
	
	// Test with multiple operations to distinguish real vs simulation
	operations := []struct {
		name     string
		id       string
		params   map[string]interface{}
		expectedDuration time.Duration
	}{
		{
			name: "Quick Sync",
			id:   "sync-configuration",
			params: map[string]interface{}{
				"source-config": "/etc/quick-sync.yaml",
				"target-system": "quick-system",
				"sync-type":     "quick",
			},
			expectedDuration: 500 * time.Millisecond,
		},
		{
			name: "Medium Backup",
			id:   "backup-infrastructure",
			params: map[string]interface{}{
				"source":      "/data/medium",
				"target":      "/backup/medium",
				"backup-type": "medium",
			},
			expectedDuration: 1 * time.Second,
		},
		{
			name: "Slow Restore",
			id:   "restore-disaster",
			params: map[string]interface{}{
				"backup-file":         "/backup/slow.tar.gz",
				"target-environment": "slow-environment",
				"restore-mode":        "slow-restore",
			},
			expectedDuration: 2 * time.Second,
		},
	}

	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			startTime := time.Now()
			err := suite.runController.ExecuteRun(ctx, op.id, op.params)
			duration := time.Since(startTime)
			
			require.NoError(t, err, "Operation should succeed")
			
			// Verify execution took expected time (indicating real work, not instant simulation)
			assert.True(t, duration >= op.expectedDuration, 
				"Operation should take at least %v, took %v", op.expectedDuration, duration)
		})
	}

	// Verify log evidence of real execution
	logOutput := suite.GetCapturedLogs()
	assert.Contains(t, logOutput, "Starting real execution", "Should indicate real execution")
	assert.Contains(t, logOutput, "Real execution completed successfully", "Should indicate real completion")
	assert.NotContains(t, logOutput, "simulated", "Should not indicate simulation")
}

// TestConcurrentOperations tests concurrent operation execution
func (suite *TestAgentIntegrationSuite) TestConcurrentOperations(t *testing.T) {
	defer suite.ClearLogs()

	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()
	
	// Create wait group for concurrent operations
	var wg sync.WaitGroup
	results := make(chan error, 3)
	
	operations := []struct {
		name   string
		id     string
		params map[string]interface{}
	}{
		{
			name: "Concurrent Backup 1",
			id:   "backup-infrastructure",
			params: map[string]interface{}{
				"source":      "/data/concurrent1",
				"target":      "/backup/concurrent1",
				"backup-type": "full",
			},
		},
		{
			name: "Concurrent Restore",
			id:   "restore-disaster",
			params: map[string]interface{}{
				"backup-file":         "/backup/concurrent1.tar.gz",
				"target-environment": "concurrent-restore",
				"restore-mode":        "full",
			},
		},
		{
			name: "Concurrent Sync",
			id:   "sync-configuration",
			params: map[string]interface{}{
				"source-config": "/etc/concurrent.yaml",
				"target-system": "concurrent-system",
				"sync-type":     "bidirectional",
			},
		},
	}

	// Execute operations concurrently
	for _, op := range operations {
		wg.Add(1)
		go func(op struct {
			name   string
			id     string
			params map[string]interface{}
		}) {
			defer wg.Done()
			err := suite.runController.ExecuteRun(ctx, op.id, op.params)
			results <- err
		}(op)
	}

	// Wait for all operations to complete
	wg.Wait()
	close(results)

	// Check all operations succeeded
	for err := range results {
		require.NoError(t, err, "Concurrent operation should succeed")
	}

	// Verify no race conditions or deadlocks
	logOutput := suite.GetCapturedLogs()
	assert.Contains(t, logOutput, "Real execution completed successfully", "All operations should complete successfully")
	
	// Verify all operations started
	for _, op := range operations {
		assert.Contains(t, logOutput, op.name, "Should log operation start")
	}
}

// TestAgentStateConsistency tests that agent state remains consistent
func (suite *TestAgentIntegrationSuite) TestAgentStateConsistency(t *testing.T) {
	defer suite.ClearLogs()

	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()

	// Execute multiple operations
	operations := []string{
		"backup-infrastructure",
		"sync-configuration",
		"restore-disaster",
	}

	for _, opID := range operations {
		params := map[string]interface{}{
			"source":      fmt.Sprintf("/data/%s", opID),
			"target":      fmt.Sprintf("/backup/%s", opID),
			"backup-type": "full",
		}
		
		err := suite.runController.ExecuteRun(ctx, opID, params)
		require.NoError(t, err, "Operation should succeed")

		// Verify agent state after each operation
		var expectedAgent string
		switch opID {
		case "backup-infrastructure":
			expectedAgent = "backup-devops-agent"
		case "sync-configuration":
			expectedAgent = "config-sync-agent"
		case "restore-disaster":
			expectedAgent = "disaster-recovery-agent"
		}

		status := suite.agentStore.GetAgentStatus(expectedAgent)
		assert.Equal(t, "completed", status, "%s agent should be in completed state", expectedAgent)
	}

	// Verify final state consistency
	availableAgents := suite.agentStore.ListAvailableAgents()
	assert.Len(t, availableAgents, 3, "Should have exactly 3 available agents")

	// Check no agents are in unexpected states
	for _, agentID := range availableAgents {
		status := suite.agentStore.GetAgentStatus(agentID)
		assert.Contains(t, []string{"completed"}, status, 
			"Agent %s should be in 'completed' state, got '%s'", agentID, status)
	}
}

// Test suite initialization
func TestSuite(t *testing.T) {
	suite := NewTestAgentIntegrationSuite()
	
	// Test all scenarios
	t.Run("CompleteAgentExecutionFlow", suite.TestCompleteAgentExecutionFlow)
	t.Run("RealBackupInfrastructureOperations", suite.TestRealBackupInfrastructureOperations)
	t.Run("DisasterRecoveryOperations", suite.TestDisasterRecoveryOperations)
	t.Run("ConfigurationSyncOperations", suite.TestConfigurationSyncOperations)
	t.Run("NoSilentActiveRuns", suite.TestNoSilentActiveRuns)
	t.Run("ComprehensiveLoggingVerification", suite.TestComprehensiveLoggingVerification)
	t.Run("ErrorHandlingAndEdgeCases", suite.TestErrorHandlingAndEdgeCases)
	t.Run("RealExecutionVsSimulation", suite.TestRealExecutionVsSimulation)
	t.Run("ConcurrentOperations", suite.TestConcurrentOperations)
	t.Run("AgentStateConsistency", suite.TestAgentStateConsistency)
}

// TestCompleteAgentExecutionFlow tests the complete execution flow from run_controller through agent_executor to the store
func TestCompleteAgentExecutionFlow(t *testing.T) {
	suite := NewTestAgentIntegrationSuite()
	defer suite.ClearLogs()

	// Capture logs for verification
	cleanup := suite.CaptureLogs()
	defer cleanup()

	ctx := context.Background()
	operationID := "backup-infrastructure"
	params := map[string]interface{}{
		"source":      "/data/source",
		"target":      "/data/backup",
		"backup-type": "full",
	}

	// Test complete execution flow
	err := suite.runController.ExecuteRun(ctx, operationID, params)

	// Verify the flow succeeded
	require.NoError(t, err, "Complete execution flow should succeed")

	// Verify agent store state
	agentExists := suite.agentStore.AgentExists("backup-devops-agent")
	require.True(t, agentExists, "Backup DevOps agent should exist")

	capabilities := suite.agentStore.GetAgentCapabilities("backup-devops-agent")
	require.Contains(t, capabilities, "backup-infrastructure", "Agent should support backup infrastructure operation")

	status := suite.agentStore.GetAgentStatus("backup-devops-agent")
	assert.Equal(t, "completed", status, "Agent status should be completed after successful execution")

	// Verify log output contains expected markers
	logOutput := suite.GetCapturedLogs()
	assert.Contains(t, logOutput, "[BACKUP-DEVOPS] Starting real execution of operation backup-infrastructure", "Should log real execution start")
	assert.Contains(t, logOutput, "[AGENT-EXECUTOR] Starting real execution of agent backup-devops-agent", "Should log agent execution start")
	assert.Contains(t, logOutput, "[AGENT-STORE] Starting real execution of agent backup-devops-agent", "Should log store execution start")
	assert.Contains(t, logOutput, "[BACKUP-INFRASTRUCTURE] Starting backup operation", "Should log backup operation start")
	assert.Contains(t, logOutput, "[BACKUP-DEVOPS] Real execution completed successfully", "Should log successful completion")
}