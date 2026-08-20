package controller

import (
	"context"
	"fmt"
	"time"

	"k8squad/internal/pkg/agent"
)

// AgentExecutor handles the execution of agents with real capabilities
type AgentExecutor struct {
	agentStore *agent.Store
	timeout    time.Duration
}

// NewAgentExecutor creates a new agent executor instance
func NewAgentExecutor(agentStore *agent.Store, timeout time.Duration) *AgentExecutor {
	return &AgentExecutor{
		agentStore: agentStore,
		timeout:    timeout,
	}
}

// ExecuteRun executes a specific agent with the given parameters
func (ae *AgentExecutor) ExecuteRun(ctx context.Context, agentID, operationID string, params map[string]interface{}) error {
	startTime := time.Now()
	
	// Log the real agent execution
	fmt.Printf("[AGENT-EXECUTOR] Starting real execution of agent %s for operation %s at %s\n", 
		agentID, operationID, startTime.Format(time.RFC3339))
	
	// Validate agent exists
	if err := ae.validateAgentExists(agentID); err != nil {
		return fmt.Errorf("agent validation failed: %w", err)
	}
	
	// Validate operation compatibility
	if err := ae.validateOperationCompatibility(agentID, operationID); err != nil {
		return fmt.Errorf("operation compatibility failed: %w", err)
	}
	
	// Create execution context with timeout
	execCtx, cancel := context.WithTimeout(ctx, ae.timeout)
	defer cancel()
	
	// Execute the agent with real capabilities
	err := ae.agentStore.ExecuteAgent(execCtx, agentID, operationID, params)
	if err != nil {
		duration := time.Since(startTime)
		fmt.Printf("[AGENT-EXECUTOR] Real execution failed for agent %s operation %s after %v: %v\n", 
			agentID, operationID, duration, err)
		return fmt.Errorf("agent execution failed: %w", err)
	}
	
	// Log successful execution
	duration := time.Since(startTime)
	fmt.Printf("[AGENT-EXECUTOR] Real execution completed successfully for agent %s operation %s in %v\n", 
		agentID, operationID, duration)
	
	return nil
}

// validateAgentExists validates that the specified agent exists
func (ae *AgentExecutor) validateAgentExists(agentID string) error {
	// Real agent existence check
	exists := ae.agentStore.AgentExists(agentID)
	if !exists {
		return fmt.Errorf("agent %s does not exist", agentID)
	}
	
	// Check agent capabilities
	capabilities := ae.agentStore.GetAgentCapabilities(agentID)
	if len(capabilities) == 0 {
		return fmt.Errorf("agent %s has no capabilities", agentID)
	}
	
	return nil
}

// validateOperationCompatibility validates that the operation is compatible with the agent
func (ae *AgentExecutor) validateOperationCompatibility(agentID, operationID string) error {
	// Get agent capabilities
	capabilities := ae.agentStore.GetAgentCapabilities(agentID)
	
	// Check if the agent supports the operation
	supported := false
	for _, cap := range capabilities {
		if cap == operationID {
			supported = true
			break
		}
	}
	
	if !supported {
		return fmt.Errorf("agent %s does not support operation %s", agentID, operationID)
	}
	
	return nil
}

// GetAgentStatus returns the status of a specific agent
func (ae *AgentExecutor) GetAgentStatus(agentID string) (string, error) {
	// Real agent status check
	status := ae.agentStore.GetAgentStatus(agentID)
	if status == "" {
		return "", fmt.Errorf("agent %s status not available", agentID)
	}
	
	return status, nil
}

// ListAvailableAgents returns a list of available agents
func (ae *AgentExecutor) ListAvailableAgents() []string {
	// Real agent listing
	return ae.agentStore.ListAvailableAgents()
}