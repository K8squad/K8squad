package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStore_RegisterAgent(t *testing.T) {
	store := NewMemoryStore()

	err := store.RegisterAgent("test-agent", []string{"capability1", "capability2"})
	require.NoError(t, err)

	assert.True(t, store.AgentExists("test-agent"))
	assert.Equal(t, []string{"capability1", "capability2"}, store.GetAgentCapabilities("test-agent"))
}

func TestMemoryStore_AgentExists(t *testing.T) {
	store := NewMemoryStore()

	assert.False(t, store.AgentExists("non-existent-agent"))

	store.RegisterAgent("test-agent", []string{})
	assert.True(t, store.AgentExists("test-agent"))
}

func TestMemoryStore_GetAgentCapabilities(t *testing.T) {
	store := NewMemoryStore()

	assert.Empty(t, store.GetAgentCapabilities("non-existent-agent"))

	capabilities := []string{"backup", "restore", "sync"}
	store.RegisterAgent("test-agent", capabilities)
	assert.Equal(t, capabilities, store.GetAgentCapabilities("test-agent"))
}

func TestMemoryStore_GetAgentStatus(t *testing.T) {
	store := NewMemoryStore()

	assert.Equal(t, "", store.GetAgentStatus("non-existent-agent"))

	store.RegisterAgent("test-agent", []string{})
	assert.Equal(t, "active", store.GetAgentStatus("test-agent"))
}

func TestMemoryStore_ListAvailableAgents(t *testing.T) {
	store := NewMemoryStore()

	assert.Empty(t, store.ListAvailableAgents())

	store.RegisterAgent("agent1", []string{})
	store.RegisterAgent("agent2", []string{})
	assert.ElementsMatch(t, []string{"agent1", "agent2"}, store.ListAvailableAgents())
}

func TestMemoryStore_GetAgent(t *testing.T) {
	store := NewMemoryStore()

	_, err := store.GetAgent("non-existent-agent")
	assert.Error(t, err)

	capabilities := []string{"backup", "restore"}
	store.RegisterAgent("test-agent", capabilities)

	agent, err := store.GetAgent("test-agent")
	require.NoError(t, err)
	assert.Equal(t, "test-agent", agent.ID)
	assert.Equal(t, capabilities, agent.Capabilities)
	assert.Equal(t, "active", agent.Status)
	assert.WithinDuration(t, time.Now(), agent.CreatedAt, 1*time.Second)
	assert.WithinDuration(t, time.Now(), agent.LastActivity, 1*time.Second)
}

func TestMemoryStore_RemoveAgent(t *testing.T) {
	store := NewMemoryStore()

	err := store.RemoveAgent("non-existent-agent")
	assert.Error(t, err)

	store.RegisterAgent("test-agent", []string{})
	assert.True(t, store.AgentExists("test-agent"))

	err = store.RemoveAgent("test-agent")
	require.NoError(t, err)
	assert.False(t, store.AgentExists("test-agent"))
}

func TestMemoryStore_UpdateAgentStatus(t *testing.T) {
	store := NewMemoryStore()

	err := store.UpdateAgentStatus("non-existent-agent", "running")
	assert.Error(t, err)

	store.RegisterAgent("test-agent", []string{})

	err = store.UpdateAgentStatus("test-agent", "running")
	require.NoError(t, err)
	assert.Equal(t, "running", store.GetAgentStatus("test-agent"))
}

func TestMemoryStore_InitializeStore(t *testing.T) {
	store := NewMemoryStore()

	err := store.InitializeStore()
	require.NoError(t, err)

	assert.True(t, store.AgentExists("backup-devops-agent"))
	assert.True(t, store.AgentExists("disaster-recovery-agent"))
	assert.True(t, store.AgentExists("config-sync-agent"))

	assert.Contains(t, store.GetAgentCapabilities("backup-devops-agent"), "backup-infrastructure")
	assert.Contains(t, store.GetAgentCapabilities("disaster-recovery-agent"), "restore-disaster")
	assert.Contains(t, store.GetAgentCapabilities("config-sync-agent"), "sync-configuration")
}
