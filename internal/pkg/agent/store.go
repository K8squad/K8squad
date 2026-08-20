package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Agent describes a registered backup-DevOps agent and its runtime state.
type Agent struct {
	ID           string
	Capabilities []string
	Status       string
	CreatedAt    time.Time
	LastActivity time.Time
}

// Store is the canonical name for the in-memory agent store used by the
// backup-DevOps controllers.
type Store = MemoryStore

// MemoryStore is a concurrency-safe in-memory registry of agents.
type MemoryStore struct {
	mu     sync.RWMutex
	agents map[string]*Agent
}

// NewMemoryStore creates an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{agents: make(map[string]*Agent)}
}

// RegisterAgent adds an agent with the given capabilities.
// Registering an existing agent returns an error.
func (s *MemoryStore) RegisterAgent(id string, capabilities []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.agents[id]; exists {
		return fmt.Errorf("agent %s already registered", id)
	}
	now := time.Now()
	s.agents[id] = &Agent{
		ID:           id,
		Capabilities: capabilities,
		Status:       "active",
		CreatedAt:    now,
		LastActivity: now,
	}
	return nil
}

// RemoveAgent deletes an agent. Removing an unknown agent returns an error.
func (s *MemoryStore) RemoveAgent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.agents[id]; !exists {
		return fmt.Errorf("agent %s not found", id)
	}
	delete(s.agents, id)
	return nil
}

// AgentExists reports whether an agent with the given ID is registered.
func (s *MemoryStore) AgentExists(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.agents[id]
	return exists
}

// GetAgent returns the Agent for the given ID.
func (s *MemoryStore) GetAgent(id string) (*Agent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, exists := s.agents[id]
	if !exists {
		return nil, fmt.Errorf("agent %s not found", id)
	}
	cp := *a
	return &cp, nil
}

// GetAgentCapabilities returns the capabilities of the given agent,
// or nil when the agent is unknown.
func (s *MemoryStore) GetAgentCapabilities(id string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, exists := s.agents[id]
	if !exists {
		return nil
	}
	return append([]string(nil), a.Capabilities...)
}

// GetAgentStatus returns the status of the given agent,
// or "" when the agent is unknown.
func (s *MemoryStore) GetAgentStatus(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, exists := s.agents[id]
	if !exists {
		return ""
	}
	return a.Status
}

// UpdateAgentStatus sets the status of a registered agent.
func (s *MemoryStore) UpdateAgentStatus(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, exists := s.agents[id]
	if !exists {
		return fmt.Errorf("agent %s not found", id)
	}
	a.Status = status
	a.LastActivity = time.Now()
	return nil
}

// ListAvailableAgents returns the IDs of all registered agents.
func (s *MemoryStore) ListAvailableAgents() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.agents))
	for id := range s.agents {
		ids = append(ids, id)
	}
	return ids
}

// ExecuteAgent dispatches an operation to an agent. It verifies the agent
// exists and supports the operation, records the activity, and honors the
// context deadline while the operation runs.
func (s *MemoryStore) ExecuteAgent(ctx context.Context, agentID, operationID string, params map[string]interface{}) error {
	s.mu.RLock()
	a, exists := s.agents[agentID]
	s.mu.RUnlock()
	if !exists {
		return fmt.Errorf("agent %s not found", agentID)
	}

	supported := false
	for _, cap := range a.Capabilities {
		if cap == operationID {
			supported = true
			break
		}
	}
	if !supported {
		return fmt.Errorf("agent %s does not support operation %s", agentID, operationID)
	}

	// Real work would happen here; wait until the context allows it to start.
	select {
	case <-ctx.Done():
		return fmt.Errorf("execution of %s on %s cancelled: %w", operationID, agentID, ctx.Err())
	case <-time.After(time.Millisecond):
	}

	s.mu.Lock()
	a.LastActivity = time.Now()
	s.mu.Unlock()
	return nil
}

// InitializeStore seeds the store with the default backup-DevOps agents.
func (s *MemoryStore) InitializeStore() error {
	defaults := map[string][]string{
		"backup-devops-agent":     {"backup-infrastructure"},
		"disaster-recovery-agent": {"restore-disaster"},
		"config-sync-agent":       {"sync-configuration"},
	}
	for id, capabilities := range defaults {
		if err := s.RegisterAgent(id, capabilities); err != nil {
			return err
		}
	}
	return nil
}
