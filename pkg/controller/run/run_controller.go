package run

import (
	"context"
	"fmt"
	"time"

	"github.com/K8squad/K8squad/internal/pkg/agent"
	"github.com/K8squad/K8squad/internal/pkg/config"
	"github.com/K8squad/K8squad/pkg/controller"
)

// Controller manages the execution of backup DevOps operations
type Controller struct {
	config        *config.Config
	agentExecutor *controller.AgentExecutor
}

// NewController creates a new run controller instance
func NewController(cfg *config.Config, agentStore *agent.Store) *Controller {
	agentExecutor := controller.NewAgentExecutor(agentStore, 30*time.Minute)
	return &Controller{
		config:        cfg,
		agentExecutor: agentExecutor,
	}
}

// ExecuteRun executes a backup DevOps operation with real execution capabilities
func (rc *Controller) ExecuteRun(ctx context.Context, operationID string, params map[string]interface{}) error {
	startTime := time.Now()

	// Log the real execution start
	fmt.Printf("[BACKUP-DEVOPS] Starting real execution of operation %s at %s\n", operationID, startTime.Format(time.RFC3339))

	// Validate operation parameters
	if err := rc.validateOperationParams(operationID, params); err != nil {
		return fmt.Errorf("operation validation failed: %w", err)
	}

	// Get the appropriate agent for the operation
	agentID, err := rc.getAgentForOperation(operationID)
	if err != nil {
		return fmt.Errorf("agent selection failed: %w", err)
	}

	// Execute the operation with real agent executor
	err = rc.agentExecutor.ExecuteRun(ctx, agentID, operationID, params)
	if err != nil {
		// Log the real execution failure
		fmt.Printf("[BACKUP-DEVOPS] Real execution failed for operation %s: %v\n", operationID, err)
		return fmt.Errorf("real agent execution failed: %w", err)
	}

	// Log successful real execution
	duration := time.Since(startTime)
	fmt.Printf("[BACKUP-DEVOPS] Real execution completed successfully for operation %s in %v\n", operationID, duration)

	return nil
}

// validateOperationParams validates the parameters for an operation
func (rc *Controller) validateOperationParams(operationID string, params map[string]interface{}) error {
	// Implement real parameter validation for backup operations
	switch operationID {
	case "backup-infrastructure":
		return rc.validateBackupParams(params)
	case "restore-disaster":
		return rc.validateRestoreParams(params)
	case "sync-configuration":
		return rc.validateSyncParams(params)
	default:
		return fmt.Errorf("unknown operation type: %s", operationID)
	}
}

// validateBackupParams validates backup operation parameters
func (rc *Controller) validateBackupParams(params map[string]interface{}) error {
	required := []string{"source", "target", "backup-type"}
	for _, field := range required {
		if _, exists := params[field]; !exists {
			return fmt.Errorf("missing required parameter: %s", field)
		}
	}
	return nil
}

// validateRestoreParams validates restore operation parameters
func (rc *Controller) validateRestoreParams(params map[string]interface{}) error {
	required := []string{"backup-file", "target-environment", "restore-mode"}
	for _, field := range required {
		if _, exists := params[field]; !exists {
			return fmt.Errorf("missing required parameter: %s", field)
		}
	}
	return nil
}

// validateSyncParams validates sync operation parameters
func (rc *Controller) validateSyncParams(params map[string]interface{}) error {
	required := []string{"source-config", "target-system", "sync-type"}
	for _, field := range required {
		if _, exists := params[field]; !exists {
			return fmt.Errorf("missing required parameter: %s", field)
		}
	}
	return nil
}

// getAgentForOperation selects the appropriate agent for an operation
func (rc *Controller) getAgentForOperation(operationID string) (string, error) {
	// Real agent selection logic based on operation type
	agentMapping := map[string]string{
		"backup-infrastructure": "backup-devops-agent",
		"restore-disaster":      "disaster-recovery-agent",
		"sync-configuration":    "config-sync-agent",
	}

	agentID, exists := agentMapping[operationID]
	if !exists {
		return "", fmt.Errorf("no agent available for operation: %s", operationID)
	}

	return agentID, nil
}
