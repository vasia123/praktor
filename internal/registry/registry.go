package registry

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
	"github.com/mtzanidakis/praktor/internal/config"
	"github.com/mtzanidakis/praktor/internal/store"
)

type Registry struct {
	mu        sync.RWMutex
	store     *store.Store
	agents    map[string]config.AgentDefinition
	cfg       config.DefaultsConfig
	basePath  string
	allowFrom []int64
}

func New(s *store.Store, agents map[string]config.AgentDefinition, cfg config.DefaultsConfig, basePath string, allowFrom []int64) *Registry {
	return &Registry{
		store:     s,
		agents:    agents,
		cfg:       cfg,
		basePath:  basePath,
		allowFrom: allowFrom,
	}
}

// Update replaces the agent definitions and defaults, then syncs to the store.
func (r *Registry) Update(agents map[string]config.AgentDefinition, defaults config.DefaultsConfig) error {
	r.mu.Lock()
	r.agents = agents
	r.cfg = defaults
	r.mu.Unlock()

	return r.Sync()
}

func (r *Registry) Sync() error {
	// Ensure users from allow_from exist
	if err := r.syncUsers(); err != nil {
		return fmt.Errorf("sync users: %w", err)
	}

	// Determine admin user for YAML agent migration
	adminUserID := r.findAdminUserID()

	ids := make([]string, 0, len(r.agents))
	for name, def := range r.agents {
		ids = append(ids, name)

		a := &store.Agent{
			ID:          name,
			Name:        name,
			Description: def.Description,
			Model:       def.Model,
			Image:       def.Image,
			Workspace:   def.Workspace,
			ClaudeMD:    def.ClaudeMD,
			UserID:      adminUserID,
		}
		if a.Workspace == "" {
			a.Workspace = name
		}

		if err := r.store.SaveAgent(a); err != nil {
			return fmt.Errorf("save agent %s: %w", name, err)
		}

		if err := r.ensureDirectories(a.Workspace); err != nil {
			return fmt.Errorf("ensure directories for %s: %w", name, err)
		}
	}

	// Only delete agents not in YAML if YAML agents are defined
	// (otherwise user-created agents would be wiped)
	if len(ids) > 0 {
		if err := r.store.DeleteAgentsNotIn(ids); err != nil {
			return fmt.Errorf("delete stale agents: %w", err)
		}
	}

	return r.ensureGlobalDirectory()
}

// syncUsers seeds users from allow_from only when the DB has no users.
// First user becomes admin. Once users exist in DB, allow_from is ignored.
func (r *Registry) syncUsers() error {
	if len(r.allowFrom) == 0 {
		return nil
	}

	userCount, err := r.store.UserCount()
	if err != nil {
		return err
	}
	if userCount > 0 {
		return nil // DB has users, skip seeding
	}

	for i, telegramID := range r.allowFrom {
		id := fmt.Sprintf("%d", telegramID)
		u := &store.User{
			ID:         id,
			Username:   id,
			IsAdmin:    i == 0,
			Status:     "approved",
			TelegramID: telegramID,
		}
		if err := r.store.CreateUser(u); err != nil {
			return fmt.Errorf("create user %s: %w", id, err)
		}
		slog.Info("seeded user from allow_from", "id", id, "is_admin", i == 0)
	}
	return nil
}

func (r *Registry) findAdminUserID() string {
	users, err := r.store.ListUsers()
	if err != nil || len(users) == 0 {
		return ""
	}
	for _, u := range users {
		if u.IsAdmin {
			return u.ID
		}
	}
	return users[0].ID
}

func (r *Registry) Get(agentID string) (*store.Agent, error) {
	return r.store.GetAgent(agentID)
}

func (r *Registry) List() ([]store.Agent, error) {
	return r.store.ListAgents()
}

func (r *Registry) ListByUser(userID string) ([]store.Agent, error) {
	return r.store.ListAgentsByUser(userID)
}

func (r *Registry) GetDefinition(agentID string) (config.AgentDefinition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.agents[agentID]
	return def, ok
}

// GetAgentByUserAndName looks up an agent by user ID and name.
func (r *Registry) GetAgentByUserAndName(userID, name string) (*store.Agent, error) {
	return r.store.GetAgentByUserAndName(userID, name)
}

// CreateAgentForUser creates a new agent for the given user.
func (r *Registry) CreateAgentForUser(userID, name, description, model, systemPrompt string) (*store.Agent, error) {
	id := uuid.New().String()
	a := &store.Agent{
		ID:           id,
		Name:         name,
		Description:  description,
		Model:        model,
		Workspace:    fmt.Sprintf("user-%s", userID), // all agents share user workspace
		UserID:       userID,
		SystemPrompt: systemPrompt,
	}

	if err := r.store.SaveAgent(a); err != nil {
		return nil, fmt.Errorf("save agent: %w", err)
	}
	return a, nil
}

// UpdateAgentForUser updates an existing agent's properties.
func (r *Registry) UpdateAgentForUser(agentID, name, description, model, systemPrompt string) error {
	a, err := r.store.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("agent not found: %s", agentID)
	}
	if name != "" {
		a.Name = name
	}
	if description != "" {
		a.Description = description
	}
	a.Model = model
	a.SystemPrompt = systemPrompt
	return r.store.SaveAgent(a)
}

// DeleteAgentForUser deletes an agent by ID.
func (r *Registry) DeleteAgentForUser(agentID string) error {
	return r.store.DeleteAgent(agentID)
}

func (r *Registry) ResolveModel(agentID string) string {
	// Check DB agent first
	ag, err := r.store.GetAgent(agentID)
	if err == nil && ag != nil && ag.Model != "" {
		return ag.Model
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if def, ok := r.agents[agentID]; ok && def.Model != "" {
		return def.Model
	}
	return r.cfg.Model
}

func (r *Registry) ResolveImage(agentID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if def, ok := r.agents[agentID]; ok && def.Image != "" {
		return def.Image
	}
	return r.cfg.Image
}

func (r *Registry) GetClaudeMD(agentID string) (string, error) {
	r.mu.RLock()
	def, hasDef := r.agents[agentID]
	r.mu.RUnlock()

	// Check config-specified path first
	if hasDef && def.ClaudeMD != "" {
		path := filepath.Join(r.basePath, def.ClaudeMD)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "", nil
			}
			return "", err
		}
		return string(data), nil
	}

	// Default: look in agent workspace dir
	workspace := agentID
	if hasDef && def.Workspace != "" {
		workspace = def.Workspace
	}
	path := filepath.Join(r.basePath, workspace, "CLAUDE.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

func (r *Registry) GetGlobalClaudeMD() (string, error) {
	path := filepath.Join(r.basePath, "global", "CLAUDE.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

func (r *Registry) AgentDescriptions() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	descs := make(map[string]string, len(r.agents))
	for name, def := range r.agents {
		descs[name] = def.Description
	}
	return descs
}

// AgentDescriptionsByUser returns agent descriptions for a specific user from DB.
func (r *Registry) AgentDescriptionsByUser(userID string) map[string]string {
	agents, err := r.store.ListAgentsByUser(userID)
	if err != nil {
		return nil
	}
	descs := make(map[string]string, len(agents))
	for _, a := range agents {
		descs[a.Name] = a.Description
	}
	return descs
}

func (r *Registry) AgentPath(workspace string) string {
	return filepath.Join(r.basePath, workspace)
}

func (r *Registry) GlobalPath() string {
	return filepath.Join(r.basePath, "global")
}

func (r *Registry) DefaultsConfig() config.DefaultsConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

func (r *Registry) Store() *store.Store {
	return r.store
}

func (r *Registry) ensureDirectories(workspace string) error {
	dir := filepath.Join(r.basePath, workspace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create agent dir: %w", err)
	}

	claudeMD := filepath.Join(dir, "CLAUDE.md")
	if _, err := os.Stat(claudeMD); os.IsNotExist(err) {
		if err := os.WriteFile(claudeMD, []byte("# Agent Memory\n\nThis file stores context for this agent.\n"), 0o644); err != nil {
			return fmt.Errorf("create CLAUDE.md: %w", err)
		}
	}

	agentMD := filepath.Join(dir, "AGENT.md")
	if _, err := os.Stat(agentMD); os.IsNotExist(err) {
		if err := os.WriteFile(agentMD, []byte(agentMDTemplate), 0o644); err != nil {
			return fmt.Errorf("create AGENT.md: %w", err)
		}
	}
	return nil
}

func (r *Registry) GetAgentMD(agentID string) (string, error) {
	r.mu.RLock()
	def, hasDef := r.agents[agentID]
	r.mu.RUnlock()

	workspace := agentID
	if hasDef && def.Workspace != "" {
		workspace = def.Workspace
	}
	path := filepath.Join(r.basePath, workspace, "AGENT.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

func (r *Registry) SaveAgentMD(agentID, content string) error {
	r.mu.RLock()
	def, hasDef := r.agents[agentID]
	r.mu.RUnlock()

	workspace := agentID
	if hasDef && def.Workspace != "" {
		workspace = def.Workspace
	}
	path := filepath.Join(r.basePath, workspace, "AGENT.md")
	return os.WriteFile(path, []byte(content), 0o644)
}

func (r *Registry) GetUserMD() (string, error) {
	path := filepath.Join(r.basePath, "global", "USER.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

func (r *Registry) SaveUserMD(content string) error {
	path := filepath.Join(r.basePath, "global", "USER.md")
	return os.WriteFile(path, []byte(content), 0o644)
}

const agentMDTemplate = `# Agent Identity

## Name
(Agent display name)

## Vibe
(Personality, communication style)

## Expertise
(Areas of specialization)
`

const userMDTemplate = `# User Profile

## Name
(Your full name)

## Preferred Name
(What you'd like to be called)

## Pronouns
(e.g. he/him, she/her, they/them)

## Timezone
(e.g. Europe/Athens)

## Notes
(Anything else you'd like agents to know about you)

## Interests
(Topics, hobbies, areas of expertise)
`

func (r *Registry) ensureGlobalDirectory() error {
	dir := filepath.Join(r.basePath, "global")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create global dir: %w", err)
	}

	claudeMD := filepath.Join(dir, "CLAUDE.md")
	if _, err := os.Stat(claudeMD); os.IsNotExist(err) {
		defaultContent := "# Global Instructions\n\nThis file is loaded by all agents.\n"
		if err := os.WriteFile(claudeMD, []byte(defaultContent), 0o644); err != nil {
			return fmt.Errorf("create global CLAUDE.md: %w", err)
		}
	}

	userMD := filepath.Join(dir, "USER.md")
	if _, err := os.Stat(userMD); os.IsNotExist(err) {
		if err := os.WriteFile(userMD, []byte(userMDTemplate), 0o644); err != nil {
			return fmt.Errorf("create USER.md: %w", err)
		}
	}

	return nil
}
