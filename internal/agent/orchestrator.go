package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mtzanidakis/praktor/internal/config"
	"github.com/mtzanidakis/praktor/internal/container"
	"github.com/mtzanidakis/praktor/internal/natsbus"
	"github.com/mtzanidakis/praktor/internal/registry"
	"github.com/mtzanidakis/praktor/internal/schedule"
	"github.com/mtzanidakis/praktor/internal/store"
	"github.com/mtzanidakis/praktor/internal/vault"
	"github.com/nats-io/nats.go"
)

// SwarmCoordinator is the interface the orchestrator uses to handle swarm IPC.
type SwarmCoordinator interface {
	GetSwarmChatTopic(containerAgentID string) (swarmID, chatTopic string, ok bool)
	PublishSwarmChat(topic, from, content string) error
}

type Orchestrator struct {
	bus        *natsbus.Bus
	client     *natsbus.Client
	containers *container.Manager
	store      *store.Store
	registry   *registry.Registry
	vault      *vault.Vault
	cfg        config.DefaultsConfig
	sessions   *SessionTracker
	queues     map[string]*AgentQueue
	lastMeta   map[string]map[string]string // agentID → last message meta
	mu         sync.RWMutex
	listeners        []OutputListener
	fileListeners    []FileListener
	taskListeners    []TaskCreatedListener
	telegramHandler  TelegramActionHandler
	listenerMu       sync.RWMutex
	swarmCoord SwarmCoordinator
}

type OutputListener func(agentID, content string, meta map[string]string)
type FileListener func(agentID string, chatID int64, data []byte, name, mimeType, caption string)
type TaskCreatedListener func(task store.ScheduledTask)

// TelegramAction represents a Telegram API action requested by an agent via IPC.
type TelegramAction struct {
	Type    string          `json:"type"`
	ChatID  int64           `json:"chat_id"`
	Payload json.RawMessage `json:"payload"`
}

// TelegramActionResult is the response from executing a Telegram action.
type TelegramActionResult struct {
	MessageID int             `json:"message_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// TelegramActionHandler processes Telegram actions on behalf of agents.
type TelegramActionHandler func(ctx context.Context, action TelegramAction) TelegramActionResult

type IPCCommand struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func NewOrchestrator(bus *natsbus.Bus, ctr *container.Manager, s *store.Store, reg *registry.Registry, cfg config.DefaultsConfig, v *vault.Vault) *Orchestrator {
	o := &Orchestrator{
		bus:        bus,
		containers: ctr,
		store:      s,
		registry:   reg,
		vault:      v,
		cfg:        cfg,
		sessions:   NewSessionTracker(),
		queues:     make(map[string]*AgentQueue),
		lastMeta:   make(map[string]map[string]string),
	}

	client, err := natsbus.NewClient(bus)
	if err != nil {
		slog.Error("orchestrator nats client failed", "error", err)
		return o
	}
	o.client = client

	// Subscribe to all agent output
	_, _ = client.Subscribe("agent.*.output", func(msg *nats.Msg) {
		o.handleAgentOutput(msg)
	})

	// Subscribe to all IPC commands
	_, _ = client.Subscribe("host.ipc.*", func(msg *nats.Msg) {
		o.handleIPC(msg)
	})

	return o
}

// SetSwarmCoordinator sets the swarm coordinator for handling swarm IPC commands.
func (o *Orchestrator) SetSwarmCoordinator(sc SwarmCoordinator) {
	o.swarmCoord = sc
}

// OnTelegramAction registers the handler for Telegram actions from agents.
func (o *Orchestrator) OnTelegramAction(handler TelegramActionHandler) {
	o.telegramHandler = handler
}

// resolveChatID returns the chat ID from the payload string, falling back to the agent's last known chat.
func (o *Orchestrator) resolveChatID(agentID, chatIDStr string) (int64, error) {
	if chatIDStr != "" {
		return strconv.ParseInt(chatIDStr, 10, 64)
	}
	meta := o.getLastMeta(agentID)
	if meta != nil && meta["chat_id"] != "" {
		return strconv.ParseInt(meta["chat_id"], 10, 64)
	}
	return 0, fmt.Errorf("no chat_id available for agent %s", agentID)
}

// UpdateDefaults replaces the defaults config used for new containers.
func (o *Orchestrator) UpdateDefaults(cfg config.DefaultsConfig) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cfg = cfg
}

func (o *Orchestrator) OnOutput(listener OutputListener) {
	o.listenerMu.Lock()
	defer o.listenerMu.Unlock()
	o.listeners = append(o.listeners, listener)
}

func (o *Orchestrator) OnFile(listener FileListener) {
	o.listenerMu.Lock()
	defer o.listenerMu.Unlock()
	o.fileListeners = append(o.fileListeners, listener)
}

func (o *Orchestrator) OnTaskCreated(listener TaskCreatedListener) {
	o.listenerMu.Lock()
	defer o.listenerMu.Unlock()
	o.taskListeners = append(o.taskListeners, listener)
}

// ContainerAgentID returns the container/NATS agent ID for a given user.
// One container per user: "user-{userID}".
func ContainerAgentID(userID string) string {
	return "user-" + userID
}

func (o *Orchestrator) HandleMessage(ctx context.Context, agentID, text string, meta map[string]string) error {
	// Ensure agent exists
	ag, err := o.registry.Get(agentID)
	if err != nil {
		return fmt.Errorf("get agent: %w", err)
	}
	if ag == nil {
		return fmt.Errorf("agent not registered: %s", agentID)
	}

	// Determine per-user container ID
	userID := meta["user_id"]
	containerID := agentID // legacy: use agentID if no userID
	if userID != "" {
		containerID = ContainerAgentID(userID)
		// Include agent_name in meta for agent-runner to fetch config
		meta["agent_name"] = ag.Name
	}

	// Save incoming message
	sender := "user"
	if s, ok := meta["sender"]; ok {
		sender = s
	}
	msg := &store.Message{
		AgentID: agentID,
		Sender:  sender,
		Content: text,
	}
	_ = o.store.SaveMessage(msg)
	o.publishMessageEvent(msg)

	// Enqueue message (keyed by containerID for per-user container)
	q := o.getQueue(containerID)
	q.Enqueue(QueuedMessage{
		AgentID:     agentID,
		ContainerID: containerID,
		UserID:      userID,
		Text:        text,
		Meta:        meta,
	})

	// Process queue
	go o.processQueue(ctx, containerID)

	return nil
}

func (o *Orchestrator) getQueue(agentID string) *AgentQueue {
	o.mu.Lock()
	defer o.mu.Unlock()

	q, ok := o.queues[agentID]
	if !ok {
		q = NewAgentQueue(agentID)
		o.queues[agentID] = q
	}
	return q
}

func (o *Orchestrator) processQueue(ctx context.Context, containerID string) {
	q := o.getQueue(containerID)

	if !q.TryLock() {
		return // Already processing
	}
	defer q.Unlock()

	for {
		msg, ok := q.Dequeue()
		if !ok {
			return
		}

		if err := o.executeMessage(ctx, containerID, msg); err != nil {
			slog.Error("execute message failed", "container", containerID, "error", err)
		}
	}
}

func (o *Orchestrator) executeMessage(ctx context.Context, containerID string, msg QueuedMessage) error {
	agentID := msg.AgentID
	userID := msg.UserID

	// Resolve agent config from registry
	def, hasDef := o.registry.GetDefinition(agentID)

	ag, err := o.registry.Get(agentID)
	if err != nil || ag == nil {
		return fmt.Errorf("get agent: %w", err)
	}

	// Determine workspace: per-user workspace if userID is set
	workspace := ag.Workspace
	if userID != "" {
		workspace = "user-" + userID
	}

	// Ensure container is running (keyed by containerID, e.g. "user-123")
	info := o.containers.GetRunning(containerID)
	if info == nil {
		// Capture NATS client count before starting so we can detect when agent connects
		clientsBefore := o.bus.NumClients()
		slog.Info("starting user container", "container", containerID, "agent", agentID, "nats_clients_before", clientsBefore)

		opts := container.AgentOpts{
			AgentID:   containerID,
			UserID:    userID,
			Workspace: workspace,
			Model:     o.registry.ResolveModel(agentID),
			Image:     o.registry.ResolveImage(agentID),
			NATSUrl:   o.bus.AgentNATSURL(),
		}
		if hasDef {
			opts.Env = cloneMap(def.Env)
			opts.AllowedTools = def.AllowedTools
			opts.NixEnabled = def.NixEnabled
		}

		o.resolveSecrets(&opts, agentID, def, hasDef)
		o.resolveExtensions(&opts, agentID)

		info, err = o.containers.StartAgent(ctx, opts)
		if err != nil {
			return fmt.Errorf("start agent: %w", err)
		}

		// Wait for agent to connect to NATS by watching client count
		deadline := time.After(30 * time.Second)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

	waitLoop:
		for {
			select {
			case <-deadline:
				slog.Warn("agent ready timeout, sending anyway", "container", containerID, "nats_clients", o.bus.NumClients())
				break waitLoop
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				current := o.bus.NumClients()
				if current > clientsBefore {
					// Give the agent a moment to set up subscriptions after connecting
					time.Sleep(500 * time.Millisecond)
					slog.Info("agent container ready", "container", containerID, "nats_clients", current)
					break waitLoop
				}
			}
		}

		now := time.Now()
		o.sessions.Set(containerID, &Session{
			ID:          info.ID,
			AgentID:     containerID,
			ContainerID: info.ID,
			Status:      "running",
			StartedAt:   now,
			LastActive:  now,
		})

		o.publishAgentStartEvent(containerID)
	}

	// Send message to container via NATS
	payload := map[string]string{
		"text":    msg.Text,
		"agentID": containerID,
	}
	for k, v := range msg.Meta {
		payload[k] = v
	}
	// Always include agent_name and user_id in payload
	if ag.Name != "" {
		payload["agent_name"] = ag.Name
	}
	if userID != "" {
		payload["user_id"] = userID
	}

	// Store meta so output handler can route responses back (keyed by containerID)
	o.mu.Lock()
	o.lastMeta[containerID] = msg.Meta
	o.mu.Unlock()

	data, _ := json.Marshal(payload)
	topic := natsbus.TopicAgentInput(containerID)
	slog.Info("publishing message to agent", "container", containerID, "agent", agentID, "topic", topic)
	if err := o.client.Publish(topic, data); err != nil {
		return fmt.Errorf("publish message: %w", err)
	}
	o.sessions.Touch(containerID)
	return o.client.Flush()
}

func (o *Orchestrator) RouteQuery(ctx context.Context, agentID string, message string) (string, error) {
	// Ensure the agent container is running
	info := o.containers.GetRunning(agentID)
	if info == nil {
		ag, err := o.registry.Get(agentID)
		if err != nil || ag == nil {
			return "", fmt.Errorf("agent not found: %s", agentID)
		}

		clientsBefore := o.bus.NumClients()
		opts := container.AgentOpts{
			AgentID:   agentID,
			Workspace: ag.Workspace,
			Model:     o.registry.ResolveModel(agentID),
			Image:     o.registry.ResolveImage(agentID),
			NATSUrl:   o.bus.AgentNATSURL(),
		}
		def, hasDef := o.registry.GetDefinition(agentID)
		if hasDef {
			opts.Env = cloneMap(def.Env)
			opts.NixEnabled = def.NixEnabled
		}

		o.resolveSecrets(&opts, agentID, def, hasDef)
		o.resolveExtensions(&opts, agentID)

		info, err = o.containers.StartAgent(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("start agent for routing: %w", err)
		}

		// Wait for NATS connection
		deadline := time.After(30 * time.Second)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
	waitLoop:
		for {
			select {
			case <-deadline:
				break waitLoop
			case <-ctx.Done():
				return "", ctx.Err()
			case <-ticker.C:
				if o.bus.NumClients() > clientsBefore {
					time.Sleep(500 * time.Millisecond)
					break waitLoop
				}
			}
		}

		now := time.Now()
		o.sessions.Set(agentID, &Session{
			ID:          info.ID,
			AgentID:     agentID,
			ContainerID: info.ID,
			Status:      "running",
			StartedAt:   now,
			LastActive:  now,
		})

		o.publishAgentStartEvent(agentID)
	}

	o.sessions.Touch(agentID)

	// Send routing query via NATS request-reply
	topic := natsbus.TopicAgentRoute(agentID)
	data, _ := json.Marshal(map[string]string{"text": message})
	resp, err := o.client.Request(topic, data, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("route query: %w", err)
	}

	var result struct {
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		// Try plain text response
		return strings.TrimSpace(string(resp.Data)), nil
	}
	return strings.TrimSpace(result.Agent), nil
}

func (o *Orchestrator) handleAgentOutput(msg *nats.Msg) {
	// Extract agentID from topic: agent.{agentID}.output
	topic := msg.Subject
	var agentID string
	if _, err := fmt.Sscanf(topic, "agent.%s", &agentID); err != nil {
		return
	}
	// Remove trailing ".output"
	if len(agentID) > 7 && agentID[len(agentID)-7:] == ".output" {
		agentID = agentID[:len(agentID)-7]
	}

	var output struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		return
	}

	o.sessions.Touch(agentID)

	if output.Type == "result" {
		content := o.redactSecrets(agentID, output.Content)

		agentMsg := &store.Message{
			AgentID: agentID,
			Sender:  "agent",
			Content: content,
		}
		_ = o.store.SaveMessage(agentMsg)
		o.publishMessageEvent(agentMsg)

		// Get metadata from the latest queued message for this agent
		meta := o.getLastMeta(agentID)

		o.listenerMu.RLock()
		for _, l := range o.listeners {
			l(agentID, content, meta)
		}
		o.listenerMu.RUnlock()
	}
}

func (o *Orchestrator) getLastMeta(agentID string) map[string]string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.lastMeta[agentID]
}

func (o *Orchestrator) handleIPC(msg *nats.Msg) {
	var cmd IPCCommand
	if err := json.Unmarshal(msg.Data, &cmd); err != nil {
		slog.Warn("invalid IPC command", "error", err)
		o.respondIPC(msg, map[string]any{"error": "invalid command"})
		return
	}

	// Extract agentID from subject: host.ipc.{agentID}
	agentID := msg.Subject
	if idx := len("host.ipc."); idx < len(agentID) {
		agentID = agentID[idx:]
	}

	slog.Info("IPC command received", "type", cmd.Type, "agent", agentID)

	switch cmd.Type {
	case "create_task":
		o.ipcCreateTask(msg, agentID, cmd.Payload)
	case "list_tasks":
		o.ipcListTasks(msg, agentID)
	case "update_task":
		o.ipcUpdateTask(msg, cmd.Payload)
	case "delete_task":
		o.ipcDeleteTask(msg, cmd.Payload)
	case "read_user_md":
		o.ipcReadUserMD(msg)
	case "update_user_md":
		o.ipcUpdateUserMD(msg, cmd.Payload)
	case "swarm_message":
		o.ipcSwarmMessage(msg, agentID, cmd.Payload)
	case "get_agent_config":
		o.ipcGetAgentConfig(msg, cmd.Payload)
	case "extension_status":
		o.ipcExtensionStatus(msg, agentID, cmd.Payload)
	case "send_file":
		o.ipcSendFile(msg, agentID, cmd.Payload)
	case "tg_send_message", "tg_reply", "tg_edit_message", "tg_delete_message",
		"tg_forward_message", "tg_send_photo_url", "tg_send_sticker",
		"tg_send_voice", "tg_send_video_note", "tg_send_animation",
		"tg_send_poll", "tg_set_reaction", "tg_pin_message", "tg_unpin_message",
		"tg_get_chat_info":
		o.ipcTelegramAction(msg, agentID, cmd.Type, cmd.Payload)
	default:
		slog.Warn("unknown IPC command", "type", cmd.Type)
		o.respondIPC(msg, map[string]any{"error": "unknown command: " + cmd.Type})
	}
}

func (o *Orchestrator) respondIPC(msg *nats.Msg, data any) {
	resp, err := json.Marshal(data)
	if err != nil {
		slog.Error("failed to marshal IPC response", "error", err)
		return
	}
	if err := msg.Respond(resp); err != nil {
		slog.Error("failed to respond to IPC", "error", err)
	}
}

func (o *Orchestrator) ipcCreateTask(msg *nats.Msg, agentID string, payload json.RawMessage) {
	var req struct {
		Name     string `json:"name"`
		Schedule string `json:"schedule"`
		Prompt   string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		o.respondIPC(msg, map[string]any{"error": "invalid payload"})
		return
	}
	if req.Name == "" || req.Schedule == "" || req.Prompt == "" {
		o.respondIPC(msg, map[string]any{"error": "name, schedule, and prompt are required"})
		return
	}

	normalized, err := schedule.NormalizeSchedule(req.Schedule)
	if err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("invalid schedule: %v", err)})
		return
	}

	// Extract user ID from container agent ID (format: "user-{userID}")
	userID := ""
	if strings.HasPrefix(agentID, "user-") {
		userID = strings.TrimPrefix(agentID, "user-")
	}

	t := &store.ScheduledTask{
		ID:          uuid.New().String(),
		AgentID:     agentID,
		Name:        req.Name,
		Schedule:    normalized,
		Prompt:      req.Prompt,
		ContextMode: "isolated",
		Status:      "active",
		UserID:      userID,
		NextRunAt:   schedule.CalculateNextRun(normalized),
	}

	if err := o.store.SaveTask(t); err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("save failed: %v", err)})
		return
	}

	slog.Info("task created via IPC", "id", t.ID, "name", t.Name, "agent", agentID, "user_id", userID)
	o.respondIPC(msg, map[string]any{"ok": true, "id": t.ID})

	// Notify listeners about recurring task creation
	s, _ := schedule.ParseSchedule(normalized)
	if s != nil && s.Kind != "once" {
		o.listenerMu.RLock()
		for _, l := range o.taskListeners {
			l(*t)
		}
		o.listenerMu.RUnlock()
	}
}

func (o *Orchestrator) ipcListTasks(msg *nats.Msg, agentID string) {
	tasks, err := o.store.ListTasksForAgent(agentID)
	if err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("list failed: %v", err)})
		return
	}

	type taskEntry struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Schedule string `json:"schedule"`
		Prompt   string `json:"prompt"`
		Status   string `json:"status"`
	}
	out := make([]taskEntry, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskEntry{
			ID:       t.ID,
			Name:     t.Name,
			Schedule: t.Schedule,
			Prompt:   t.Prompt,
			Status:   t.Status,
		})
	}
	o.respondIPC(msg, map[string]any{"ok": true, "tasks": out})
}

func (o *Orchestrator) ipcUpdateTask(msg *nats.Msg, payload json.RawMessage) {
	var req struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Schedule string `json:"schedule"`
		Prompt   string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.ID == "" {
		o.respondIPC(msg, map[string]any{"error": "id is required"})
		return
	}

	t, err := o.store.GetTask(req.ID)
	if err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("task not found: %v", err)})
		return
	}

	if req.Name != "" {
		t.Name = req.Name
	}
	if req.Prompt != "" {
		t.Prompt = req.Prompt
	}
	if req.Schedule != "" {
		normalized, err := schedule.NormalizeSchedule(req.Schedule)
		if err != nil {
			o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("invalid schedule: %v", err)})
			return
		}
		t.Schedule = normalized
		t.NextRunAt = schedule.CalculateNextRun(normalized)
	}

	if err := o.store.SaveTask(t); err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("save failed: %v", err)})
		return
	}

	slog.Info("task updated via IPC", "id", t.ID, "name", t.Name)
	o.respondIPC(msg, map[string]any{"ok": true, "id": t.ID})
}

func (o *Orchestrator) ipcDeleteTask(msg *nats.Msg, payload json.RawMessage) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.ID == "" {
		o.respondIPC(msg, map[string]any{"error": "id is required"})
		return
	}
	if err := o.store.DeleteTask(req.ID); err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("delete failed: %v", err)})
		return
	}
	slog.Info("task deleted via IPC", "id", req.ID)
	o.respondIPC(msg, map[string]any{"ok": true})
}

func (o *Orchestrator) ipcReadUserMD(msg *nats.Msg) {
	content, err := o.registry.GetUserMD()
	if err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("read failed: %v", err)})
		return
	}
	o.respondIPC(msg, map[string]any{"ok": true, "content": content})
}

func (o *Orchestrator) ipcUpdateUserMD(msg *nats.Msg, payload json.RawMessage) {
	var req struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		o.respondIPC(msg, map[string]any{"error": "invalid payload"})
		return
	}
	if err := o.registry.SaveUserMD(req.Content); err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("save failed: %v", err)})
		return
	}
	slog.Info("user profile updated via IPC")
	o.respondIPC(msg, map[string]any{"ok": true})
}

func (o *Orchestrator) ipcSwarmMessage(msg *nats.Msg, agentID string, payload json.RawMessage) {
	if o.swarmCoord == nil {
		o.respondIPC(msg, map[string]any{"error": "swarm coordinator not available"})
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Content == "" {
		o.respondIPC(msg, map[string]any{"error": "content is required"})
		return
	}

	swarmID, chatTopic, ok := o.swarmCoord.GetSwarmChatTopic(agentID)
	if !ok {
		o.respondIPC(msg, map[string]any{"error": "agent is not in a swarm"})
		return
	}

	if err := o.swarmCoord.PublishSwarmChat(chatTopic, agentID, req.Content); err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("publish failed: %v", err)})
		return
	}

	slog.Info("swarm chat message sent via IPC", "agent", agentID, "swarm", swarmID)
	o.respondIPC(msg, map[string]any{"ok": true})
}

func (o *Orchestrator) ipcExtensionStatus(msg *nats.Msg, agentID string, payload json.RawMessage) {
	// Accept the payload as-is (marketplaces: string[], plugins: {name, enabled}[])
	if err := o.store.SetExtensionStatus(agentID, string(payload)); err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("save failed: %v", err)})
		return
	}

	slog.Info("extension status updated via IPC", "agent", agentID)
	o.respondIPC(msg, map[string]any{"ok": true})
}

func (o *Orchestrator) ipcSendFile(msg *nats.Msg, agentID string, payload json.RawMessage) {
	var req struct {
		Name     string `json:"name"`
		Data     string `json:"data"`
		MimeType string `json:"mime_type"`
		Caption  string `json:"caption"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		o.respondIPC(msg, map[string]any{"error": "invalid payload"})
		return
	}
	if req.Name == "" || req.Data == "" {
		o.respondIPC(msg, map[string]any{"error": "name and data are required"})
		return
	}

	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("base64 decode failed: %v", err)})
		return
	}

	meta := o.getLastMeta(agentID)
	chatIDStr := ""
	if meta != nil {
		chatIDStr = meta["chat_id"]
	}
	if chatIDStr == "" {
		o.respondIPC(msg, map[string]any{"error": "no chat_id available for this agent"})
		return
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		o.respondIPC(msg, map[string]any{"error": "invalid chat_id"})
		return
	}

	o.listenerMu.RLock()
	listeners := o.fileListeners
	o.listenerMu.RUnlock()

	for _, l := range listeners {
		l(agentID, chatID, data, req.Name, req.MimeType, req.Caption)
	}

	slog.Info("file sent via IPC", "agent", agentID, "name", req.Name, "size", len(data), "mime", req.MimeType)
	o.respondIPC(msg, map[string]any{"ok": true})
}

func (o *Orchestrator) ipcGetAgentConfig(msg *nats.Msg, payload json.RawMessage) {
	var req struct {
		UserID    string `json:"user_id"`
		AgentName string `json:"agent_name"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		o.respondIPC(msg, map[string]any{"error": "invalid payload"})
		return
	}
	if req.UserID == "" || req.AgentName == "" {
		o.respondIPC(msg, map[string]any{"error": "user_id and agent_name are required"})
		return
	}

	ag, err := o.registry.GetAgentByUserAndName(req.UserID, req.AgentName)
	if err != nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("lookup failed: %v", err)})
		return
	}
	if ag == nil {
		o.respondIPC(msg, map[string]any{"error": fmt.Sprintf("agent %q not found for user %s", req.AgentName, req.UserID)})
		return
	}

	result := map[string]any{
		"ok":            true,
		"model":         ag.Model,
		"system_prompt": ag.SystemPrompt,
	}

	// Check YAML definition for additional config (allowed_tools, extensions)
	if def, ok := o.registry.GetDefinition(ag.ID); ok {
		if len(def.AllowedTools) > 0 {
			result["allowed_tools"] = def.AllowedTools
		}
	}

	o.respondIPC(msg, result)
}

func (o *Orchestrator) publishMessageEvent(msg *store.Message) {
	if o.client == nil {
		return
	}

	role := "user"
	if msg.Sender == "agent" {
		role = "assistant"
	}

	now := time.Now()
	timeStr := msg.CreatedAt.Format("15:04")
	if msg.CreatedAt.IsZero() {
		timeStr = now.Format("15:04")
	}

	event := map[string]any{
		"type":      "message",
		"agent_id":  msg.AgentID,
		"timestamp": now.UTC().Format(time.RFC3339),
		"data": map[string]any{
			"id":   msg.ID,
			"role": role,
			"text": msg.Content,
			"time": timeStr,
		},
	}

	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	topic := natsbus.TopicEventsAgent(msg.AgentID)
	_ = o.client.Publish(topic, data)
}

func (o *Orchestrator) EnsureAgent(ctx context.Context, agentID string) error {
	// Already running — nothing to do
	if info := o.containers.GetRunning(agentID); info != nil {
		return nil
	}

	def, hasDef := o.registry.GetDefinition(agentID)
	ag, err := o.registry.Get(agentID)
	if err != nil || ag == nil {
		return fmt.Errorf("agent not found: %s", agentID)
	}

	clientsBefore := o.bus.NumClients()
	slog.Info("starting agent", "agent", agentID, "nats_clients_before", clientsBefore)

	opts := container.AgentOpts{
		AgentID:   agentID,
		UserID:    ag.UserID,
		Workspace: ag.Workspace,
		Model:     o.registry.ResolveModel(agentID),
		Image:     o.registry.ResolveImage(agentID),
		NATSUrl:   o.bus.AgentNATSURL(),
	}
	if hasDef {
		opts.Env = cloneMap(def.Env)
		opts.AllowedTools = def.AllowedTools
		opts.NixEnabled = def.NixEnabled
	}
	o.resolveSecrets(&opts, agentID, def, hasDef)
	o.resolveExtensions(&opts, agentID)

	info, err := o.containers.StartAgent(ctx, opts)
	if err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

waitLoop:
	for {
		select {
		case <-deadline:
			slog.Warn("agent ready timeout", "agent", agentID, "nats_clients", o.bus.NumClients())
			break waitLoop
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if o.bus.NumClients() > clientsBefore {
				time.Sleep(500 * time.Millisecond)
				slog.Info("agent container ready", "agent", agentID, "nats_clients", o.bus.NumClients())
				break waitLoop
			}
		}
	}

	now := time.Now()
	o.sessions.Set(agentID, &Session{
		ID:          info.ID,
		AgentID:     agentID,
		ContainerID: info.ID,
		Status:      "running",
		StartedAt:   now,
		LastActive:  now,
	})
	o.publishAgentStartEvent(agentID)
	return nil
}

// EnsureUserContainer ensures a per-user container is running.
func (o *Orchestrator) EnsureUserContainer(ctx context.Context, userID string) error {
	containerID := ContainerAgentID(userID)
	return o.EnsureAgent(ctx, containerID)
}

// AbortSession sends an abort control command to a running agent,
// terminating the active Claude query without stopping the container.
// If chatID is non-zero, only the specified chat's query is aborted.
func (o *Orchestrator) AbortSession(ctx context.Context, agentID string, chatID int64) error {
	if chatID == 0 {
		// Global abort — drain pending messages
		if q := o.getQueue(agentID); q != nil {
			q.Clear()
		}
	}
	if o.containers.GetRunning(agentID) == nil {
		return nil
	}
	topic := natsbus.TopicAgentControl(agentID)
	cmd := map[string]string{"command": "abort"}
	if chatID != 0 {
		cmd["chat_id"] = strconv.FormatInt(chatID, 10)
	}
	data, _ := json.Marshal(cmd)
	_, err := o.client.Request(topic, data, 5*time.Second)
	return err
}

// ClearSession sends a clear_session control command to a running agent,
// resetting its conversation context without stopping the container.
// If chatID is non-zero, only the specified chat's session is cleared.
func (o *Orchestrator) ClearSession(ctx context.Context, agentID string, chatID int64) error {
	if o.containers.GetRunning(agentID) == nil {
		return nil
	}
	topic := natsbus.TopicAgentControl(agentID)
	cmd := map[string]string{"command": "clear_session"}
	if chatID != 0 {
		cmd["chat_id"] = strconv.FormatInt(chatID, 10)
	}
	data, _ := json.Marshal(cmd)
	_, err := o.client.Request(topic, data, 5*time.Second)
	return err
}

func (o *Orchestrator) StopAgent(ctx context.Context, agentID string) error {
	o.sessions.Remove(agentID)
	err := o.containers.StopAgent(ctx, agentID)
	if err == nil {
		o.publishAgentStopEvent(agentID, "manual")
	}
	return err
}

func (o *Orchestrator) StartIdleReaper(ctx context.Context) {
	if o.cfg.IdleTimeout == 0 {
		return
	}

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			idle := o.sessions.ListIdle(o.cfg.IdleTimeout)
			for _, agentID := range idle {
				slog.Info("stopping idle agent", "agent", agentID, "timeout", o.cfg.IdleTimeout)
				if err := o.StopAgent(ctx, agentID); err != nil {
					slog.Error("failed to stop idle agent", "agent", agentID, "error", err)
					continue
				}
				o.publishIdleStopEvent(agentID)
			}
		}
	}
}

// StartNixGC runs nix-collect-garbage -d once per day at a random time
// in all running agent containers that have nix_enabled.
func (o *Orchestrator) StartNixGC(ctx context.Context) {
	for {
		// Sleep for a random duration between 0 and 24 hours
		delay := time.Duration(rand.Int64N(int64(24 * time.Hour)))
		slog.Info("nix-collect-garbage scheduled", "in", delay.Round(time.Minute))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		agents, err := o.registry.List()
		if err != nil {
			slog.Error("nix-gc: failed to list agents", "error", err)
			continue
		}

		for _, ag := range agents {
			def, ok := o.registry.GetDefinition(ag.ID)
			if !ok || !def.NixEnabled {
				continue
			}

			if err := o.EnsureAgent(ctx, ag.ID); err != nil {
				slog.Warn("nix-gc: failed to start agent", "agent", ag.ID, "error", err)
				continue
			}

			output, err := o.containers.Exec(ctx, ag.ID, []string{"nix-collect-garbage", "-d"})
			if err != nil {
				slog.Warn("nix-collect-garbage failed", "agent", ag.ID, "error", err)
				continue
			}

			// Log last non-empty line
			lines := strings.Split(strings.TrimSpace(output), "\n")
			if len(lines) > 0 {
				slog.Info("nix-collect-garbage: ["+ag.ID+"] "+lines[len(lines)-1])
			}
		}
	}
}

func (o *Orchestrator) publishAgentStartEvent(agentID string) {
	if o.client == nil {
		return
	}

	event := map[string]any{
		"type":      "agent_started",
		"agent_id":  agentID,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	_ = o.client.Publish(natsbus.TopicEventsAgent(agentID), data)
}

func (o *Orchestrator) publishAgentStopEvent(agentID, reason string) {
	if o.client == nil {
		return
	}

	event := map[string]any{
		"type":      "agent_stopped",
		"agent_id":  agentID,
		"reason":    reason,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	_ = o.client.Publish(natsbus.TopicEventsAgent(agentID), data)
}

func (o *Orchestrator) publishIdleStopEvent(agentID string) {
	o.publishAgentStopEvent(agentID, "idle_timeout")
}

func (o *Orchestrator) ListRunning(ctx context.Context) ([]container.ContainerInfo, error) {
	return o.containers.ListRunning(ctx)
}

func (o *Orchestrator) ReadVolumeFile(ctx context.Context, workspace, filePath, image string) (string, error) {
	return o.containers.ReadVolumeFile(ctx, workspace, filePath, image)
}

func (o *Orchestrator) WriteVolumeFile(ctx context.Context, workspace, filePath, content, image string) error {
	return o.containers.WriteVolumeFile(ctx, workspace, filePath, content, image)
}

func (o *Orchestrator) WriteVolumeBytes(ctx context.Context, workspace, filePath string, data []byte, image string) error {
	return o.containers.WriteVolumeBytes(ctx, workspace, filePath, data, image)
}

func (o *Orchestrator) ExecInAgent(ctx context.Context, agentID string, cmd []string) (string, error) {
	return o.containers.Exec(ctx, agentID, cmd)
}

func (o *Orchestrator) ipcTelegramAction(msg *nats.Msg, agentID, ipcType string, payload json.RawMessage) {
	if o.telegramHandler == nil {
		o.respondIPC(msg, map[string]any{"error": "telegram handler not available"})
		return
	}

	// Parse chat_id from payload; fall back to agent's last known chat
	var generic struct {
		ChatID string `json:"chat_id"`
	}
	_ = json.Unmarshal(payload, &generic)

	chatID, err := o.resolveChatID(agentID, generic.ChatID)
	if err != nil {
		o.respondIPC(msg, map[string]any{"error": err.Error()})
		return
	}

	// Strip "tg_" prefix for action type
	actionType := strings.TrimPrefix(ipcType, "tg_")

	result := o.telegramHandler(context.Background(), TelegramAction{
		Type:    actionType,
		ChatID:  chatID,
		Payload: payload,
	})

	if result.Error != "" {
		o.respondIPC(msg, map[string]any{"error": result.Error})
		return
	}

	resp := map[string]any{"ok": true}
	if result.MessageID != 0 {
		resp["message_id"] = result.MessageID
	}
	if result.Data != nil {
		resp["data"] = json.RawMessage(result.Data)
	}
	o.respondIPC(msg, resp)
}
