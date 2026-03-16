package telegram

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mtzanidakis/praktor/internal/agent"
	"github.com/mtzanidakis/praktor/internal/config"
	"github.com/mtzanidakis/praktor/internal/natsbus"
	"github.com/mtzanidakis/praktor/internal/registry"
	"github.com/mtzanidakis/praktor/internal/router"
	"github.com/mtzanidakis/praktor/internal/schedule"
	"github.com/mtzanidakis/praktor/internal/store"
	"github.com/mtzanidakis/praktor/internal/swarm"
	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
	"github.com/nats-io/nats.go"
)

type Bot struct {
	bot        *telego.Bot
	handler    *th.BotHandler
	orch       *agent.Orchestrator
	router     *router.Router
	store      *store.Store
	cfg        config.TelegramConfig
	cancel     context.CancelFunc
	swarmCoord *swarm.Coordinator
	registry   *registry.Registry
	bus        *natsbus.Bus

	// Track chat_id → agentID mapping for responses
	chatAgentMu sync.RWMutex
	chatAgent   map[int64]string // chatID → agentID that last handled a message

	// Track Telegram message_id → agentID so replies route to the right agent
	msgAgentMu sync.RWMutex
	msgAgent   map[int]string // messageID → agentID

	// Track swarm → chat_id for result delivery
	swarmChatMu sync.RWMutex
	swarmChat   map[string]int64 // swarmID → chatID
}

func NewBot(cfg config.TelegramConfig, orch *agent.Orchestrator, rtr *router.Router, sc *swarm.Coordinator, reg *registry.Registry, bus *natsbus.Bus, s *store.Store) (*Bot, error) {
	bot, err := telego.NewBot(cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	b := &Bot{
		bot:        bot,
		orch:       orch,
		router:     rtr,
		store:      s,
		cfg:        cfg,
		swarmCoord: sc,
		registry:   reg,
		bus:        bus,
		chatAgent:  make(map[int64]string),
		msgAgent:   make(map[int]string),
		swarmChat:  make(map[string]int64),
	}

	// Register bot commands with Telegram so they appear in the menu
	_ = bot.SetMyCommands(context.Background(), &telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "agents", Description: "List available agents"},
			{Command: "newagent", Description: "Create a new agent"},
			{Command: "delagent", Description: "Delete an agent"},
			{Command: "project", Description: "List or switch projects"},
			{Command: "commands", Description: "Show available commands"},
			{Command: "start", Description: "Say hello to an agent"},
			{Command: "stop", Description: "Abort the active agent run"},
			{Command: "reset", Description: "Reset conversation session"},
			{Command: "tasks", Description: "List and manage scheduled tasks"},
			{Command: "nix", Description: "Manage nix packages in agent container"},
		},
	})

	// Register output listener to send responses back to Telegram
	orch.OnOutput(func(agentID, content string, meta map[string]string) {
		// Try to get chat_id from meta
		chatIDStr := ""
		if meta != nil {
			chatIDStr = meta["chat_id"]
		}

		if chatIDStr == "" {
			// Fall back to looking up which chat last talked to this agent
			b.chatAgentMu.RLock()
			for cid, aid := range b.chatAgent {
				if aid == agentID {
					chatIDStr = strconv.FormatInt(cid, 10)
					break
				}
			}
			b.chatAgentMu.RUnlock()
		}

		if chatIDStr == "" {
			return
		}

		chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err != nil {
			return
		}

		// Prefix with agent name for attribution (skip for default agent)
		attributed := content
		if agentID != rtr.DefaultAgent() {
			attributed = fmt.Sprintf("_%s:_ %s", agentID, content)
		}

		// For scheduler results, add "My tasks" inline button
		if meta != nil && meta["sender"] == "scheduler" {
			keyboard := tu.InlineKeyboard(tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("📋 Мои задачи").WithCallbackData("tasks:list"),
			))
			if err := b.sendMessageWithKeyboard(context.Background(), chatID, attributed, keyboard); err != nil {
				slog.Error("failed to send telegram message", "chat", chatID, "error", err)
			}
		} else if err := b.sendAgentMessage(context.Background(), chatID, attributed, agentID); err != nil {
			slog.Error("failed to send telegram message", "chat", chatID, "error", err)
		}
	})

	// Register file listener to send files back to Telegram
	orch.OnFile(func(agentID string, chatID int64, data []byte, name, mimeType, caption string) {
		ctx := context.Background()
		if strings.HasPrefix(mimeType, "image/") {
			if err := b.SendPhoto(ctx, chatID, data, name, caption); err != nil {
				slog.Error("failed to send photo", "chat", chatID, "name", name, "error", err)
			}
		} else {
			if err := b.SendDocument(ctx, chatID, data, name, caption); err != nil {
				slog.Error("failed to send document", "chat", chatID, "name", name, "error", err)
			}
		}
	})

	// Register Telegram action handler for agent IPC
	orch.OnTelegramAction(func(ctx context.Context, action agent.TelegramAction) agent.TelegramActionResult {
		return b.handleTelegramAction(ctx, action)
	})

	// Notify user when a recurring task is created by an agent
	orch.OnTaskCreated(func(task store.ScheduledTask) {
		if task.UserID == "" {
			return
		}
		chatID, err := strconv.ParseInt(task.UserID, 10, 64)
		if err != nil {
			return
		}
		text := fmt.Sprintf("⏰ Создана задача: *%s*\nРасписание: `%s`\nАгент: `%s`",
			task.Name, schedule.FormatSchedule(task.Schedule), task.AgentID)
		keyboard := tu.InlineKeyboard(tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("📋 Мои задачи").WithCallbackData("tasks:list"),
		))
		if err := b.sendMessageWithKeyboard(context.Background(), chatID, text, keyboard); err != nil {
			slog.Error("failed to send task creation notification", "chat", chatID, "error", err)
		}
	})

	// Subscribe to swarm events for result delivery
	if bus != nil && sc != nil {
		client, cerr := natsbus.NewClient(bus)
		if cerr == nil {
			_, _ = client.Subscribe(natsbus.TopicEventsSwarm, func(msg *nats.Msg) {
				b.handleSwarmEvent(msg)
			})
		}
	}

	return b, nil
}

func (b *Bot) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel

	updates, err := b.bot.UpdatesViaLongPolling(ctx, nil)
	if err != nil {
		cancel()
		return fmt.Errorf("start long polling: %w", err)
	}

	handler, err := th.NewBotHandler(b.bot, updates)
	if err != nil {
		cancel()
		return fmt.Errorf("create handler: %w", err)
	}
	b.handler = handler

	// Command handlers — registered before the catch-all so they match first
	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		_, _, payload := tu.ParseCommandPayload(message.Text)
		b.cmdStart(ctx, message, payload)
		return nil
	}, th.CommandEqual("start"))

	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		_, _, payload := tu.ParseCommandPayload(message.Text)
		b.cmdStop(ctx, message.Chat.ID, payload)
		return nil
	}, th.CommandEqual("stop"))

	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		_, _, payload := tu.ParseCommandPayload(message.Text)
		b.cmdReset(ctx, message.Chat.ID, payload)
		return nil
	}, th.CommandEqual("reset"))

	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		b.cmdAgents(ctx, message.Chat.ID)
		return nil
	}, th.CommandEqual("agents"))

	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		b.cmdCommands(ctx, message.Chat.ID)
		return nil
	}, th.CommandEqual("commands"))

	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		b.cmdTasks(ctx, message.Chat.ID, message.From.ID)
		return nil
	}, th.CommandEqual("tasks"))

	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		_, _, payload := tu.ParseCommandPayload(message.Text)
		b.cmdPkg(ctx, message.Chat.ID, payload)
		return nil
	}, th.CommandEqual("nix"))

	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		_, _, payload := tu.ParseCommandPayload(message.Text)
		b.cmdNewAgent(ctx, message, payload)
		return nil
	}, th.CommandEqual("newagent"))

	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		_, _, payload := tu.ParseCommandPayload(message.Text)
		b.cmdDelAgent(ctx, message, payload)
		return nil
	}, th.CommandEqual("delagent"))

	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		if !b.allowedUser(message) {
			return nil
		}
		_, _, payload := tu.ParseCommandPayload(message.Text)
		b.cmdProject(ctx, message, payload)
		return nil
	}, th.CommandEqual("project"))

	// Callback query handler for inline buttons
	handler.HandleCallbackQuery(func(hctx *th.Context, query telego.CallbackQuery) error {
		b.handleCallbackQuery(ctx, query)
		return nil
	})

	// Catch-all for regular messages
	handler.HandleMessage(func(hctx *th.Context, message telego.Message) error {
		b.handleMessage(ctx, message)
		return nil
	})

	go handler.Start()

	<-ctx.Done()
	_ = handler.Stop()
	return nil
}

func (b *Bot) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.handler != nil {
		_ = b.handler.Stop()
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg telego.Message) {
	if !b.allowedUser(msg) {
		return
	}

	chatID := msg.Chat.ID
	userID := msg.From.ID

	// Extract text from message or caption
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	// Extract file attachment
	attachment := extractAttachment(msg)

	// Nothing to process
	if text == "" && attachment == nil {
		return
	}

	// File with no text — provide default prompt
	if text == "" && attachment != nil {
		text = fmt.Sprintf("I'm sending you a file: %s", attachment.Name)
	}

	senderID := strconv.FormatInt(userID, 10)
	chatIDStr := strconv.FormatInt(chatID, 10)

	// If the user is replying to an agent's message, route directly to that agent.
	var agentID, cleanedMessage string
	if msg.ReplyToMessage != nil {
		b.msgAgentMu.RLock()
		replyAgent, ok := b.msgAgent[msg.ReplyToMessage.MessageID]
		b.msgAgentMu.RUnlock()
		if ok {
			agentID = replyAgent
			cleanedMessage = text
			slog.Debug("routing via reply", "chat", chatID, "agent", agentID, "reply_to", msg.ReplyToMessage.MessageID)
		}
	}

	// Fall back to normal routing — use per-user routing
	if agentID == "" {
		var err error
		userIDStr := strconv.FormatInt(userID, 10)
		agentID, _, cleanedMessage, err = b.router.RouteForUser(ctx, userIDStr, text)
		if err != nil {
			// Fall back to global routing
			agentID, cleanedMessage, err = b.router.Route(ctx, text)
			if err != nil {
				slog.Error("routing failed", "error", err)
				_ = b.SendMessage(ctx, chatID, "Sorry, I couldn't route your message to an agent.")
				return
			}
		}
		if cleanedMessage == "" {
			cleanedMessage = text
		}
	}

	// Handle @swarm command
	if agentID == "swarm" {
		b.handleSwarmCommand(ctx, chatID, cleanedMessage)
		return
	}

	// Track which chat is talking to which agent
	b.chatAgentMu.Lock()
	b.chatAgent[chatID] = agentID
	b.chatAgentMu.Unlock()

	// Send thinking indicator
	_ = b.sendChatAction(ctx, chatID, "typing")

	// Handle file attachment: download and write to agent workspace
	if attachment != nil {
		data, err := b.downloadFile(ctx, attachment.FileID)
		if err != nil {
			slog.Error("file download failed", "file_id", attachment.FileID, "error", err)
			_ = b.SendMessage(ctx, chatID, "Sorry, I couldn't download the file.")
			return
		}

		// Resolve agent workspace and image
		ag, err := b.registry.Get(agentID)
		if err != nil || ag == nil {
			slog.Error("agent not found for file upload", "agent", agentID, "error", err)
			_ = b.SendMessage(ctx, chatID, "Sorry, I couldn't find the agent to deliver the file.")
			return
		}

		image := b.registry.ResolveImage(agentID)
		volumePath := fmt.Sprintf("uploads/%d_%s", time.Now().Unix(), attachment.Name)
		containerPath := "/workspace/agent/" + volumePath

		if err := b.orch.WriteVolumeBytes(ctx, ag.Workspace, volumePath, data, image); err != nil {
			slog.Error("file write to volume failed", "path", volumePath, "error", err)
			_ = b.SendMessage(ctx, chatID, "Sorry, I couldn't save the file to the agent workspace.")
			return
		}

		slog.Info("file received and saved", "agent", agentID, "name", attachment.Name, "size", len(data), "path", containerPath)

		cleanedMessage = fmt.Sprintf("%s\n\n[File received: %s (%s, %d bytes) saved to %s]",
			cleanedMessage, attachment.Name, attachment.MimeType, len(data), containerPath)
	}

	meta := map[string]string{
		"sender":    fmt.Sprintf("user:%s", senderID),
		"user_id":   senderID,
		"chat_id":   chatIDStr,
		"msg_id":    strconv.Itoa(msg.MessageID),
		"chat_type": string(msg.Chat.Type),
	}
	if msg.From != nil {
		if msg.From.Username != "" {
			meta["username"] = msg.From.Username
		}
		if msg.From.FirstName != "" {
			meta["first_name"] = msg.From.FirstName
		}
		if msg.From.LastName != "" {
			meta["last_name"] = msg.From.LastName
		}
	}
	if msg.Chat.Title != "" {
		meta["chat_title"] = msg.Chat.Title
	}
	if msg.ReplyToMessage != nil {
		meta["reply_to_msg_id"] = strconv.Itoa(msg.ReplyToMessage.MessageID)
		replyText := msg.ReplyToMessage.Text
		if len(replyText) > 200 {
			replyText = replyText[:200] + "..."
		}
		if replyText != "" {
			meta["reply_to_text"] = replyText
		}
	}

	if err := b.orch.HandleMessage(ctx, agentID, cleanedMessage, meta); err != nil {
		slog.Error("handle message failed", "agent", agentID, "error", err)
		_ = b.SendMessage(ctx, chatID, "Sorry, I encountered an error processing your message.")
	}
}

// attachment holds metadata about a file attached to a Telegram message.
type attachment struct {
	FileID   string
	Name     string
	MimeType string
}

// extractAttachment checks a Telegram message for file attachments and returns
// the first one found, or nil if there are no attachments.
func extractAttachment(msg telego.Message) *attachment {
	if msg.Document != nil {
		name := msg.Document.FileName
		if name == "" {
			name = "document"
		}
		mime := msg.Document.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		return &attachment{FileID: msg.Document.FileID, Name: name, MimeType: mime}
	}

	if len(msg.Photo) > 0 {
		// Use the largest photo (last element)
		photo := msg.Photo[len(msg.Photo)-1]
		return &attachment{FileID: photo.FileID, Name: "photo.jpg", MimeType: "image/jpeg"}
	}

	if msg.Audio != nil {
		name := msg.Audio.FileName
		if name == "" {
			name = "audio.mp3"
		}
		mime := msg.Audio.MimeType
		if mime == "" {
			mime = "audio/mpeg"
		}
		return &attachment{FileID: msg.Audio.FileID, Name: name, MimeType: mime}
	}

	if msg.Video != nil {
		name := msg.Video.FileName
		if name == "" {
			name = "video.mp4"
		}
		mime := msg.Video.MimeType
		if mime == "" {
			mime = "video/mp4"
		}
		return &attachment{FileID: msg.Video.FileID, Name: name, MimeType: mime}
	}

	if msg.Voice != nil {
		mime := msg.Voice.MimeType
		if mime == "" {
			mime = "audio/ogg"
		}
		return &attachment{FileID: msg.Voice.FileID, Name: "voice.ogg", MimeType: mime}
	}

	if msg.VideoNote != nil {
		return &attachment{FileID: msg.VideoNote.FileID, Name: "videonote.mp4", MimeType: "video/mp4"}
	}

	if msg.Animation != nil {
		name := msg.Animation.FileName
		if name == "" {
			name = "animation.mp4"
		}
		mime := msg.Animation.MimeType
		if mime == "" {
			mime = "video/mp4"
		}
		return &attachment{FileID: msg.Animation.FileID, Name: name, MimeType: mime}
	}

	return nil
}

// downloadFile downloads a file from Telegram by its FileID.
func (b *Bot) downloadFile(ctx context.Context, fileID string) ([]byte, error) {
	file, err := b.bot.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}

	url := b.bot.FileDownloadURL(file.FilePath)
	data, err := tu.DownloadFile(url)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}

	return data, nil
}

func (b *Bot) SendMessage(ctx context.Context, chatID int64, text string) error {
	_, err := b.sendMessage(ctx, chatID, text)
	return err
}

// sendMessage sends a (possibly chunked) message and returns the IDs of the sent Telegram messages.
func (b *Bot) sendMessage(ctx context.Context, chatID int64, text string) ([]int, error) {
	text = toTelegramMarkdown(text)
	chunks := chunkMessage(text, 4096)
	var ids []int
	for _, chunk := range chunks {
		msg := tu.Message(tu.ID(chatID), chunk)
		msg.ParseMode = telego.ModeMarkdown
		sent, err := b.bot.SendMessage(ctx, msg)
		if err != nil {
			// Markdown parsing can fail on unescaped characters;
			// retry as plain text so the message still gets delivered.
			msg.ParseMode = ""
			sent, err = b.bot.SendMessage(ctx, msg)
		}
		if err != nil {
			return ids, fmt.Errorf("send message: %w", err)
		}
		if sent != nil {
			ids = append(ids, sent.MessageID)
		}
	}
	return ids, nil
}

// sendAgentMessage sends a message and tracks the sent message IDs → agentID
// so that Telegram replies to these messages route back to the same agent.
// Keeps at most 1000 entries to bound memory usage.
func (b *Bot) sendAgentMessage(ctx context.Context, chatID int64, text, agentID string) error {
	ids, err := b.sendMessage(ctx, chatID, text)
	if err != nil {
		return err
	}
	b.msgAgentMu.Lock()
	for _, id := range ids {
		b.msgAgent[id] = agentID
	}
	// Evict old entries if map grows too large
	if len(b.msgAgent) > 1000 {
		for k := range b.msgAgent {
			delete(b.msgAgent, k)
			if len(b.msgAgent) <= 800 {
				break
			}
		}
	}
	b.msgAgentMu.Unlock()
	return nil
}

func (b *Bot) SendPhoto(ctx context.Context, chatID int64, data []byte, name, caption string) error {
	params := &telego.SendPhotoParams{
		ChatID: tu.ID(chatID),
		Photo:  telego.InputFile{File: tu.NameReader(bytes.NewReader(data), name)},
	}
	if caption != "" {
		params.Caption = caption
	}
	_, err := b.bot.SendPhoto(ctx, params)
	if err != nil {
		return fmt.Errorf("send photo: %w", err)
	}
	return nil
}

func (b *Bot) SendDocument(ctx context.Context, chatID int64, data []byte, name, caption string) error {
	params := &telego.SendDocumentParams{
		ChatID:   tu.ID(chatID),
		Document: telego.InputFile{File: tu.NameReader(bytes.NewReader(data), name)},
	}
	if caption != "" {
		params.Caption = caption
	}
	_, err := b.bot.SendDocument(ctx, params)
	if err != nil {
		return fmt.Errorf("send document: %w", err)
	}
	return nil
}

func (b *Bot) sendChatAction(ctx context.Context, chatID int64, action string) error {
	return b.bot.SendChatAction(ctx, tu.ChatAction(tu.ID(chatID), action))
}

// handleSwarmCommand parses the swarm syntax and launches a swarm.
//
// Syntax:
//   - agent1,agent2,agent3: task    -> fan-out, first agent = lead
//   - agent1>agent2>agent3: task    -> pipeline, last agent = lead
//   - agent1<>agent2,agent3: task   -> collaborative + independent
func (b *Bot) handleSwarmCommand(ctx context.Context, chatID int64, message string) {
	if b.swarmCoord == nil || b.registry == nil {
		_ = b.SendMessage(ctx, chatID, "Swarm support is not configured.")
		return
	}

	// Split at first ": " to get agents spec and task
	colonIdx := strings.Index(message, ": ")
	if colonIdx < 0 {
		_ = b.SendMessage(ctx, chatID, "Invalid swarm syntax. Use: `agent1,agent2: task` or `agent1>agent2: task` or `agent1<>agent2: task`")
		return
	}
	agentSpec := strings.TrimSpace(message[:colonIdx])
	task := strings.TrimSpace(message[colonIdx+2:])
	if task == "" {
		_ = b.SendMessage(ctx, chatID, "Task is required after the colon.")
		return
	}

	agents, synapses, leadAgent, err := b.parseSwarmSpec(agentSpec)
	if err != nil {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Invalid swarm spec: %s", err))
		return
	}

	req := swarm.SwarmRequest{
		Name:      fmt.Sprintf("Telegram Swarm"),
		LeadAgent: leadAgent,
		Agents:    agents,
		Synapses:  synapses,
		Task:      task,
	}

	_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Launching swarm with %d agents...", len(agents)))

	run, err := b.swarmCoord.RunSwarm(ctx, req)
	if err != nil {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Failed to launch swarm: %s", err))
		return
	}

	// Track which chat started this swarm
	b.swarmChatMu.Lock()
	b.swarmChat[run.ID] = chatID
	b.swarmChatMu.Unlock()
}

func (b *Bot) parseSwarmSpec(spec string) ([]swarm.SwarmAgent, []swarm.Synapse, string, error) {
	var agents []swarm.SwarmAgent
	var synapses []swarm.Synapse
	var leadAgent string
	seen := make(map[string]bool)

	addAgent := func(name string) error {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("empty agent name")
		}
		if seen[name] {
			return nil
		}
		if _, ok := b.registry.GetDefinition(name); !ok {
			return fmt.Errorf("unknown agent: %s", name)
		}
		seen[name] = true
		agents = append(agents, swarm.SwarmAgent{
			AgentID:   name,
			Role:      name,
			Workspace: name,
		})
		return nil
	}

	// Check for pipeline syntax (>)
	if strings.Contains(spec, ">") && !strings.Contains(spec, "<>") {
		parts := strings.Split(spec, ">")
		for _, p := range parts {
			if err := addAgent(p); err != nil {
				return nil, nil, "", err
			}
		}
		// Create pipeline synapses
		for i := 0; i < len(parts)-1; i++ {
			synapses = append(synapses, swarm.Synapse{
				From: strings.TrimSpace(parts[i]),
				To:   strings.TrimSpace(parts[i+1]),
			})
		}
		leadAgent = strings.TrimSpace(parts[len(parts)-1])
		return agents, synapses, leadAgent, nil
	}

	// Check for collaborative syntax (<>)
	// Split by comma first, then check each segment for <>
	segments := strings.Split(spec, ",")
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if strings.Contains(seg, "<>") {
			pair := strings.SplitN(seg, "<>", 2)
			a := strings.TrimSpace(pair[0])
			bName := strings.TrimSpace(pair[1])
			if err := addAgent(a); err != nil {
				return nil, nil, "", err
			}
			if err := addAgent(bName); err != nil {
				return nil, nil, "", err
			}
			synapses = append(synapses, swarm.Synapse{
				From:          a,
				To:            bName,
				Bidirectional: true,
			})
		} else {
			if err := addAgent(seg); err != nil {
				return nil, nil, "", err
			}
		}
	}

	// Default lead: first agent
	if len(agents) > 0 {
		leadAgent = agents[0].Role
	}

	return agents, synapses, leadAgent, nil
}

// allowedUser checks whether the message sender is in the allow list.
func (b *Bot) allowedUser(msg telego.Message) bool {
	if len(b.cfg.AllowFrom) == 0 {
		return true
	}
	for _, id := range b.cfg.AllowFrom {
		if id == msg.From.ID {
			return true
		}
	}
	slog.Warn("unauthorized telegram user", "user_id", msg.From.ID, "chat_id", msg.Chat.ID)
	return false
}

// resolveAgent returns the agent ID from payload or falls back to the last agent for the chat.
func (b *Bot) resolveAgent(chatID int64, payload string) string {
	if payload != "" {
		name := strings.Fields(payload)[0]
		return strings.TrimPrefix(name, "@")
	}
	b.chatAgentMu.RLock()
	defer b.chatAgentMu.RUnlock()
	return b.chatAgent[chatID]
}

func (b *Bot) cmdStart(ctx context.Context, msg telego.Message, payload string) {
	chatID := msg.Chat.ID
	agentID := ""
	if f := strings.Fields(payload); len(f) > 0 {
		agentID = strings.TrimPrefix(f[0], "@")
	}
	if agentID == "" {
		agentID = b.router.DefaultAgent()
	}

	b.chatAgentMu.Lock()
	b.chatAgent[chatID] = agentID
	b.chatAgentMu.Unlock()

	_ = b.sendChatAction(ctx, chatID, "typing")

	meta := map[string]string{
		"sender":    fmt.Sprintf("user:%d", msg.From.ID),
		"chat_id":   strconv.FormatInt(chatID, 10),
		"msg_id":    strconv.Itoa(msg.MessageID),
		"chat_type": string(msg.Chat.Type),
	}
	if msg.From != nil {
		if msg.From.Username != "" {
			meta["username"] = msg.From.Username
		}
		if msg.From.FirstName != "" {
			meta["first_name"] = msg.From.FirstName
		}
		if msg.From.LastName != "" {
			meta["last_name"] = msg.From.LastName
		}
	}
	if msg.Chat.Title != "" {
		meta["chat_title"] = msg.Chat.Title
	}
	if err := b.orch.HandleMessage(ctx, agentID, "Hello!", meta); err != nil {
		slog.Error("handle start failed", "agent", agentID, "error", err)
		_ = b.SendMessage(ctx, chatID, "Sorry, I encountered an error starting the conversation.")
	}
}

func (b *Bot) cmdStop(ctx context.Context, chatID int64, payload string) {
	agentID := b.resolveAgent(chatID, payload)
	if agentID == "" {
		_ = b.SendMessage(ctx, chatID, "Usage: /stop [agent]")
		return
	}
	if err := b.orch.AbortSession(ctx, agentID, chatID); err != nil {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Failed to stop *%s*: %s", agentID, err))
		return
	}
	_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Stopped *%s*.", agentID))
}

func (b *Bot) cmdReset(ctx context.Context, chatID int64, payload string) {
	agentID := b.resolveAgent(chatID, payload)
	if agentID == "" {
		_ = b.SendMessage(ctx, chatID, "Usage: /reset [agent]")
		return
	}
	if err := b.orch.ClearSession(ctx, agentID, chatID); err != nil {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Failed to clear session for *%s*: %s", agentID, err))
		return
	}
	_ = b.SendMessage(ctx, chatID, fmt.Sprintf("New session started for *%s*.", agentID))
}

func (b *Bot) cmdCommands(ctx context.Context, chatID int64) {
	text := "*Commands*\n\n" +
		"  /agents — List your agents\n" +
		"  /newagent <name> <description> — Create a new agent\n" +
		"  /delagent <name> — Delete an agent\n" +
		"  /tasks — List and manage scheduled tasks\n" +
		"  /project \\[name] — List or switch projects\n" +
		"  /commands — Show available commands\n" +
		"  /start \\[agent] — Say hello to an agent\n" +
		"  /stop \\[agent] — Abort the active agent run\n" +
		"  /reset \\[agent] — Reset conversation session\n" +
		"  /nix <action> \\[package] \\[@agent] — Manage nix packages\n" +
		"\n@agent\\_name prefix or smart routing for regular messages.\n" +
		"@swarm prefix for swarm orchestration."
	_ = b.SendMessage(ctx, chatID, text)
}

func (b *Bot) cmdAgents(ctx context.Context, chatID int64) {
	agents, err := b.store.ListAgents()
	if err != nil {
		_ = b.SendMessage(ctx, chatID, "Failed to list agents.")
		return
	}

	running, _ := b.orch.ListRunning(ctx)
	runningSet := make(map[string]bool, len(running))
	for _, c := range running {
		runningSet[c.AgentID] = true
	}

	msgStats, _ := b.store.GetAgentMessageStats()

	var sb strings.Builder
	sb.WriteString("*Agents*\n\n")
	for _, a := range agents {
		status := "stopped"
		if runningSet[a.ID] {
			status = "running"
		}
		// For per-user agents, check container by user-{userID}
		if a.UserID != "" {
			containerID := agent.ContainerAgentID(a.UserID)
			if runningSet[containerID] {
				status = "running"
			}
		}

		model := b.registry.ResolveModel(a.ID)

		sb.WriteString(fmt.Sprintf("*%s*", a.Name))
		if a.Description != "" {
			sb.WriteString(fmt.Sprintf(" — %s", a.Description))
		}
		sb.WriteString(fmt.Sprintf("\n  Status: `%s` | Model: `%s`", status, model))

		if def, ok := b.registry.GetDefinition(a.ID); ok && def.NixEnabled {
			sb.WriteString(" | Nix: `enabled`")
		}

		if stats, ok := msgStats[a.ID]; ok {
			sb.WriteString(fmt.Sprintf(" | Messages: %d", stats.MessageCount))
		}
		sb.WriteString("\n\n")
	}

	if len(agents) == 0 {
		sb.WriteString("No agents configured. Use /newagent to create one.")
	}

	_ = b.SendMessage(ctx, chatID, sb.String())
}

func (b *Bot) cmdNewAgent(ctx context.Context, msg telego.Message, payload string) {
	chatID := msg.Chat.ID
	userID := strconv.FormatInt(msg.From.ID, 10)

	args := strings.SplitN(payload, " ", 2)
	if len(args) == 0 || args[0] == "" {
		_ = b.SendMessage(ctx, chatID, "Usage: /newagent <name> \\[description]")
		return
	}

	name := args[0]
	description := ""
	if len(args) > 1 {
		description = args[1]
	}

	ag, err := b.registry.CreateAgentForUser(userID, name, description, "", "")
	if err != nil {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Failed to create agent: %s", err))
		return
	}

	_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Agent *%s* created. Use @%s to talk to it.", ag.Name, ag.Name))
}

func (b *Bot) cmdDelAgent(ctx context.Context, msg telego.Message, payload string) {
	chatID := msg.Chat.ID
	userID := strconv.FormatInt(msg.From.ID, 10)

	name := strings.TrimSpace(payload)
	if name == "" {
		_ = b.SendMessage(ctx, chatID, "Usage: /delagent <name>")
		return
	}

	ag, err := b.registry.GetAgentByUserAndName(userID, name)
	if err != nil || ag == nil {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Agent *%s* not found.", name))
		return
	}

	if err := b.registry.DeleteAgentForUser(ag.ID); err != nil {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Failed to delete agent: %s", err))
		return
	}

	_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Agent *%s* deleted.", name))
}

func (b *Bot) cmdProject(ctx context.Context, msg telego.Message, payload string) {
	chatID := msg.Chat.ID
	userID := strconv.FormatInt(msg.From.ID, 10)
	name := strings.TrimSpace(payload)

	containerID := agent.ContainerAgentID(userID)

	if name == "" {
		// List projects — send to agent for listing
		_ = b.orch.HandleMessage(ctx, containerID, "/project list", map[string]string{
			"user_id": userID,
			"chat_id": strconv.FormatInt(chatID, 10),
			"sender":  fmt.Sprintf("user:%s", userID),
		})
		return
	}

	// Switch project — send control command
	_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Switching to project *%s*...", name))
	// The project switch happens via the MCP tool in the agent
}

func (b *Bot) cmdPkg(ctx context.Context, chatID int64, payload string) {
	usage := "Usage: /nix <search|add|list|remove|upgrade> \\[package] \\[@agent]"

	args := strings.Fields(payload)
	if len(args) == 0 {
		_ = b.SendMessage(ctx, chatID, usage)
		return
	}

	// Extract @agent hint from args (scan from end)
	var agentHint string
	var cleanArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "@") {
			agentHint = strings.TrimPrefix(a, "@")
		} else {
			cleanArgs = append(cleanArgs, a)
		}
	}

	action := cleanArgs[0]
	agentID := agentHint
	if agentID == "" {
		agentID = b.router.DefaultAgent()
	}

	var cmd []string
	switch action {
	case "search":
		if len(cleanArgs) < 2 {
			_ = b.SendMessage(ctx, chatID, "Usage: /nix search <query> \\[@agent]")
			return
		}
		cmd = []string{"nix", "search", "--json", "--quiet", "nixpkgs", cleanArgs[1]}
	case "add", "install":
		if len(cleanArgs) < 2 {
			_ = b.SendMessage(ctx, chatID, "Usage: /nix add <package...> \\[@agent]")
			return
		}
		cmd = []string{"nix", "profile", "add"}
		for _, p := range cleanArgs[1:] {
			cmd = append(cmd, "nixpkgs#"+p)
		}
	case "list", "ls":
		cmd = []string{"nix", "profile", "list", "--json"}
	case "remove", "rm":
		if len(cleanArgs) < 2 {
			_ = b.SendMessage(ctx, chatID, "Usage: /nix remove <package...> \\[@agent]")
			return
		}
		cmd = append([]string{"nix", "profile", "remove"}, cleanArgs[1:]...)
	case "upgrade":
		cmd = []string{"nix", "profile", "upgrade", "--all"}
	default:
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Unknown action: %s\n%s", action, usage))
		return
	}

	// Ensure agent container is running
	if err := b.orch.EnsureAgent(ctx, agentID); err != nil {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Failed to start agent *%s*: %s", agentID, err))
		return
	}

	_ = b.sendChatAction(ctx, chatID, "typing")

	output, err := b.orch.ExecInAgent(ctx, agentID, cmd)
	if err != nil && output == "" {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("Failed: %s", err))
		return
	}

	// Parse JSON output
	switch action {
	case "list", "ls":
		output = parseNixProfileList(output)
	case "search":
		output = parseNixSearchResults(output)
	}

	if output == "" {
		output = "Done (no output)."
	}
	if len(output) > 3500 {
		output = output[:3500] + "\n... (truncated)"
	}

	_ = b.SendMessage(ctx, chatID, fmt.Sprintf("*%s* `%s`:\n```\n%s\n```", agentID, action, output))
}

// parseNixProfileList parses `nix profile list --json` output into a human-readable format.
func parseNixProfileList(jsonOutput string) string {
	var data struct {
		Elements map[string]struct {
			StorePaths []string `json:"storePaths"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(jsonOutput), &data); err != nil {
		return jsonOutput
	}

	var lines []string
	for name, elem := range data.Elements {
		var versions []string
		for _, p := range elem.StorePaths {
			// Store path format: /nix/store/<32-char hash>-<name>-<version>
			base := path.Base(p)
			if len(base) > 33 {
				afterHash := base[33:] // skip hash + dash
				prefix := name + "-"
				if strings.HasPrefix(afterHash, prefix) {
					versions = append(versions, afterHash[len(prefix):])
				} else {
					versions = append(versions, afterHash)
				}
			}
		}
		if len(versions) == 0 {
			versions = []string{"unknown"}
		}
		lines = append(lines, fmt.Sprintf("%s: %s", name, strings.Join(versions, ", ")))
	}
	if len(lines) == 0 {
		return "No packages installed."
	}
	return strings.Join(lines, "\n")
}

// parseNixSearchResults parses `nix search --json` output into a human-readable format.
func parseNixSearchResults(jsonOutput string) string {
	var data map[string]struct {
		Pname       string `json:"pname"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(jsonOutput), &data); err != nil {
		return jsonOutput
	}

	seen := make(map[string]bool)
	var results []string
	for _, entry := range data {
		key := entry.Pname + "\t" + entry.Version + "\t" + entry.Description
		if seen[key] {
			continue
		}
		seen[key] = true
		results = append(results, fmt.Sprintf("%s %s\n%s", entry.Pname, entry.Version, entry.Description))
	}
	if len(results) == 0 {
		return "No results found."
	}
	return strings.Join(results, "\n\n")
}

// handleSwarmEvent handles swarm completion events and delivers results to Telegram.
func (b *Bot) handleSwarmEvent(msg *nats.Msg) {
	var event struct {
		Type    string          `json:"type"`
		SwarmID string          `json:"swarm_id"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		return
	}

	if event.Type != "swarm_completed" && event.Type != "swarm_failed" {
		return
	}

	b.swarmChatMu.RLock()
	chatID, ok := b.swarmChat[event.SwarmID]
	b.swarmChatMu.RUnlock()

	if ok {
		// Clean up tracking
		b.swarmChatMu.Lock()
		delete(b.swarmChat, event.SwarmID)
		b.swarmChatMu.Unlock()
	} else if b.cfg.MainChatID != 0 {
		// Swarm launched from Mission Control — deliver to main chat
		chatID = b.cfg.MainChatID
	} else {
		return
	}

	ctx := context.Background()

	if event.Type == "swarm_failed" {
		_ = b.SendMessage(ctx, chatID, "Swarm failed.")
		return
	}

	// Get the swarm run to extract results
	run, err := b.swarmCoord.GetStatus(event.SwarmID)
	if err != nil || run == nil {
		_ = b.SendMessage(ctx, chatID, "Swarm completed but could not retrieve results.")
		return
	}

	var results []swarm.AgentResult
	if run.Results != nil {
		_ = json.Unmarshal(run.Results, &results)
	}

	// Find lead agent's result
	var leadResult string
	for _, r := range results {
		if r.Role == run.LeadAgent && r.Output != "" {
			leadResult = r.Output
			break
		}
	}

	if leadResult != "" {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("*Swarm Result* (%s):\n\n%s", run.Name, leadResult))
	} else {
		// Send all results if no lead result
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("*Swarm Complete* (%s):\n\n", run.Name))
		for _, r := range results {
			sb.WriteString(fmt.Sprintf("*%s* [%s]", r.Role, r.Status))
			if r.Output != "" {
				output := r.Output
				if len(output) > 500 {
					output = output[:500] + "..."
				}
				sb.WriteString(fmt.Sprintf(":\n%s", output))
			}
			sb.WriteString("\n\n")
		}
		_ = b.SendMessage(ctx, chatID, sb.String())
	}
}

// handleTelegramAction dispatches a Telegram action from an agent to the appropriate bot API call.
func (b *Bot) handleTelegramAction(ctx context.Context, action agent.TelegramAction) agent.TelegramActionResult {
	chatID := tu.ID(action.ChatID)

	switch action.Type {
	case "send_message":
		var p struct {
			Text      string `json:"text"`
			ParseMode string `json:"parse_mode"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.Text == "" {
			return agent.TelegramActionResult{Error: "text is required"}
		}
		ids, err := b.sendMessage(ctx, action.ChatID, p.Text)
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if len(ids) > 0 {
			return agent.TelegramActionResult{MessageID: ids[0]}
		}
		return agent.TelegramActionResult{}

	case "reply":
		var p struct {
			Text      string `json:"text"`
			MessageID int    `json:"message_id"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.Text == "" || p.MessageID == 0 {
			return agent.TelegramActionResult{Error: "text and message_id are required"}
		}
		text := toTelegramMarkdown(p.Text)
		msg := tu.Message(chatID, text)
		msg.ParseMode = telego.ModeMarkdown
		msg.ReplyParameters = &telego.ReplyParameters{MessageID: p.MessageID}
		sent, err := b.bot.SendMessage(ctx, msg)
		if err != nil {
			// Retry without markdown
			msg.ParseMode = ""
			sent, err = b.bot.SendMessage(ctx, msg)
		}
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if sent != nil {
			return agent.TelegramActionResult{MessageID: sent.MessageID}
		}
		return agent.TelegramActionResult{}

	case "edit_message":
		var p struct {
			MessageID int    `json:"message_id"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.MessageID == 0 || p.Text == "" {
			return agent.TelegramActionResult{Error: "message_id and text are required"}
		}
		text := toTelegramMarkdown(p.Text)
		sent, err := b.bot.EditMessageText(ctx, &telego.EditMessageTextParams{
			ChatID:    chatID,
			MessageID: p.MessageID,
			Text:      text,
			ParseMode: telego.ModeMarkdown,
		})
		if err != nil {
			// Retry without markdown
			sent, err = b.bot.EditMessageText(ctx, &telego.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: p.MessageID,
				Text:      text,
			})
		}
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if sent != nil {
			return agent.TelegramActionResult{MessageID: sent.MessageID}
		}
		return agent.TelegramActionResult{}

	case "delete_message":
		var p struct {
			MessageID int `json:"message_id"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.MessageID == 0 {
			return agent.TelegramActionResult{Error: "message_id is required"}
		}
		if err := b.bot.DeleteMessage(ctx, &telego.DeleteMessageParams{
			ChatID:    chatID,
			MessageID: p.MessageID,
		}); err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		return agent.TelegramActionResult{}

	case "forward_message":
		var p struct {
			FromChatID int64 `json:"from_chat_id"`
			MessageID  int   `json:"message_id"`
			ToChatID   int64 `json:"to_chat_id"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.MessageID == 0 {
			return agent.TelegramActionResult{Error: "message_id is required"}
		}
		fromChat := chatID
		if p.FromChatID != 0 {
			fromChat = tu.ID(p.FromChatID)
		}
		toChat := chatID
		if p.ToChatID != 0 {
			toChat = tu.ID(p.ToChatID)
		}
		sent, err := b.bot.ForwardMessage(ctx, &telego.ForwardMessageParams{
			ChatID:     toChat,
			FromChatID: fromChat,
			MessageID:  p.MessageID,
		})
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if sent != nil {
			return agent.TelegramActionResult{MessageID: sent.MessageID}
		}
		return agent.TelegramActionResult{}

	case "send_photo_url":
		var p struct {
			URL     string `json:"url"`
			Caption string `json:"caption"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.URL == "" {
			return agent.TelegramActionResult{Error: "url is required"}
		}
		params := &telego.SendPhotoParams{
			ChatID: chatID,
			Photo:  telego.InputFile{URL: p.URL},
		}
		if p.Caption != "" {
			params.Caption = p.Caption
		}
		sent, err := b.bot.SendPhoto(ctx, params)
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if sent != nil {
			return agent.TelegramActionResult{MessageID: sent.MessageID}
		}
		return agent.TelegramActionResult{}

	case "send_sticker":
		var p struct {
			Sticker string `json:"sticker"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.Sticker == "" {
			return agent.TelegramActionResult{Error: "sticker is required"}
		}
		// Sticker can be a file_id or URL
		input := telego.InputFile{FileID: p.Sticker}
		if strings.HasPrefix(p.Sticker, "http") {
			input = telego.InputFile{URL: p.Sticker}
		}
		sent, err := b.bot.SendSticker(ctx, &telego.SendStickerParams{
			ChatID:  chatID,
			Sticker: input,
		})
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if sent != nil {
			return agent.TelegramActionResult{MessageID: sent.MessageID}
		}
		return agent.TelegramActionResult{}

	case "send_voice":
		var p struct {
			Data    string `json:"data"`
			Caption string `json:"caption"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.Data == "" {
			return agent.TelegramActionResult{Error: "data (base64) is required"}
		}
		decoded, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return agent.TelegramActionResult{Error: "base64 decode failed: " + err.Error()}
		}
		params := &telego.SendVoiceParams{
			ChatID: chatID,
			Voice:  telego.InputFile{File: tu.NameReader(bytes.NewReader(decoded), "voice.ogg")},
		}
		if p.Caption != "" {
			params.Caption = p.Caption
		}
		sent, err := b.bot.SendVoice(ctx, params)
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if sent != nil {
			return agent.TelegramActionResult{MessageID: sent.MessageID}
		}
		return agent.TelegramActionResult{}

	case "send_video_note":
		var p struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.Data == "" {
			return agent.TelegramActionResult{Error: "data (base64) is required"}
		}
		decoded, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return agent.TelegramActionResult{Error: "base64 decode failed: " + err.Error()}
		}
		sent, err := b.bot.SendVideoNote(ctx, &telego.SendVideoNoteParams{
			ChatID:    chatID,
			VideoNote: telego.InputFile{File: tu.NameReader(bytes.NewReader(decoded), "videonote.mp4")},
		})
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if sent != nil {
			return agent.TelegramActionResult{MessageID: sent.MessageID}
		}
		return agent.TelegramActionResult{}

	case "send_animation":
		var p struct {
			URL     string `json:"url"`
			Data    string `json:"data"`
			Caption string `json:"caption"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		var input telego.InputFile
		if p.URL != "" {
			input = telego.InputFile{URL: p.URL}
		} else if p.Data != "" {
			decoded, err := base64.StdEncoding.DecodeString(p.Data)
			if err != nil {
				return agent.TelegramActionResult{Error: "base64 decode failed: " + err.Error()}
			}
			input = telego.InputFile{File: tu.NameReader(bytes.NewReader(decoded), "animation.gif")}
		} else {
			return agent.TelegramActionResult{Error: "url or data is required"}
		}
		params := &telego.SendAnimationParams{
			ChatID:    chatID,
			Animation: input,
		}
		if p.Caption != "" {
			params.Caption = p.Caption
		}
		sent, err := b.bot.SendAnimation(ctx, params)
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if sent != nil {
			return agent.TelegramActionResult{MessageID: sent.MessageID}
		}
		return agent.TelegramActionResult{}

	case "send_poll":
		var p struct {
			Question              string   `json:"question"`
			Options               []string `json:"options"`
			IsAnonymous           *bool    `json:"is_anonymous"`
			Type                  string   `json:"type"`
			AllowsMultipleAnswers bool     `json:"allows_multiple_answers"`
			CorrectOptionID       *int     `json:"correct_option_id"`
			Explanation           string   `json:"explanation"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.Question == "" || len(p.Options) < 2 {
			return agent.TelegramActionResult{Error: "question and at least 2 options are required"}
		}
		pollOptions := make([]telego.InputPollOption, len(p.Options))
		for i, o := range p.Options {
			pollOptions[i] = telego.InputPollOption{Text: o}
		}
		params := &telego.SendPollParams{
			ChatID:                chatID,
			Question:              p.Question,
			Options:               pollOptions,
			AllowsMultipleAnswers: p.AllowsMultipleAnswers,
		}
		if p.IsAnonymous != nil {
			params.IsAnonymous = p.IsAnonymous
		}
		if p.CorrectOptionID != nil {
			params.CorrectOptionID = p.CorrectOptionID
		}
		if p.Type != "" {
			params.Type = p.Type
		}
		if p.Explanation != "" {
			params.Explanation = p.Explanation
		}
		sent, err := b.bot.SendPoll(ctx, params)
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		if sent != nil {
			return agent.TelegramActionResult{MessageID: sent.MessageID}
		}
		return agent.TelegramActionResult{}

	case "set_reaction":
		var p struct {
			MessageID int    `json:"message_id"`
			Emoji     string `json:"emoji"`
			IsBig     bool   `json:"is_big"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.MessageID == 0 || p.Emoji == "" {
			return agent.TelegramActionResult{Error: "message_id and emoji are required"}
		}
		if err := b.bot.SetMessageReaction(ctx, &telego.SetMessageReactionParams{
			ChatID:    chatID,
			MessageID: p.MessageID,
			Reaction: []telego.ReactionType{
				&telego.ReactionTypeEmoji{Type: "emoji", Emoji: p.Emoji},
			},
			IsBig: p.IsBig,
		}); err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		return agent.TelegramActionResult{}

	case "pin_message":
		var p struct {
			MessageID           int  `json:"message_id"`
			DisableNotification bool `json:"disable_notification"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if p.MessageID == 0 {
			return agent.TelegramActionResult{Error: "message_id is required"}
		}
		if err := b.bot.PinChatMessage(ctx, &telego.PinChatMessageParams{
			ChatID:              chatID,
			MessageID:           p.MessageID,
			DisableNotification: p.DisableNotification,
		}); err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		return agent.TelegramActionResult{}

	case "unpin_message":
		var p struct {
			MessageID int `json:"message_id"`
		}
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return agent.TelegramActionResult{Error: "invalid payload: " + err.Error()}
		}
		if err := b.bot.UnpinChatMessage(ctx, &telego.UnpinChatMessageParams{
			ChatID:    chatID,
			MessageID: p.MessageID,
		}); err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		return agent.TelegramActionResult{}

	case "get_chat_info":
		info, err := b.bot.GetChat(ctx, &telego.GetChatParams{
			ChatID: chatID,
		})
		if err != nil {
			return agent.TelegramActionResult{Error: err.Error()}
		}
		data, _ := json.Marshal(info)
		return agent.TelegramActionResult{Data: data}

	default:
		return agent.TelegramActionResult{Error: "unknown action type: " + action.Type}
	}
}

// sendMessageWithKeyboard sends a message with an inline keyboard.
func (b *Bot) sendMessageWithKeyboard(ctx context.Context, chatID int64, text string, keyboard *telego.InlineKeyboardMarkup) error {
	text = toTelegramMarkdown(text)
	chunks := chunkMessage(text, 4096)
	for i, chunk := range chunks {
		msg := tu.Message(tu.ID(chatID), chunk)
		msg.ParseMode = telego.ModeMarkdown
		// Only attach keyboard to the last chunk
		if i == len(chunks)-1 {
			msg.ReplyMarkup = keyboard
		}
		_, err := b.bot.SendMessage(ctx, msg)
		if err != nil {
			msg.ParseMode = ""
			_, err = b.bot.SendMessage(ctx, msg)
		}
		if err != nil {
			return fmt.Errorf("send message with keyboard: %w", err)
		}
	}
	return nil
}

// cmdTasks shows the user's scheduled tasks with management buttons.
func (b *Bot) cmdTasks(ctx context.Context, chatID int64, telegramUserID int64) {
	userID := strconv.FormatInt(telegramUserID, 10)
	b.sendTaskList(ctx, chatID, userID)
}

// sendTaskList sends a formatted task list with inline buttons.
func (b *Bot) sendTaskList(ctx context.Context, chatID int64, userID string) {
	tasks, err := b.store.ListTasksByUserID(userID)
	if err != nil {
		_ = b.SendMessage(ctx, chatID, "Failed to load tasks.")
		return
	}

	// Filter to active/paused tasks
	var active []store.ScheduledTask
	for _, t := range tasks {
		if t.Status == "active" || t.Status == "paused" {
			active = append(active, t)
		}
	}

	if len(active) == 0 {
		_ = b.SendMessage(ctx, chatID, "📋 У вас нет активных задач.")
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 *Мои задачи* (%d)\n\n", len(active)))
	for i, t := range active {
		status := "⏰"
		if t.Status == "paused" {
			status = "⏸"
		}
		sb.WriteString(fmt.Sprintf("%d. *%s* — %s %s\n", i+1, t.Name, status, schedule.FormatSchedule(t.Schedule)))
	}

	// Build inline keyboard: pause/resume + delete rows
	var rows [][]telego.InlineKeyboardButton
	var pauseRow []telego.InlineKeyboardButton
	var deleteRow []telego.InlineKeyboardButton
	for i, t := range active {
		label := fmt.Sprintf("⏸ %d", i+1)
		data := fmt.Sprintf("tasks:pause:%s", t.ID)
		if t.Status == "paused" {
			label = fmt.Sprintf("▶️ %d", i+1)
		}
		pauseRow = append(pauseRow, tu.InlineKeyboardButton(label).WithCallbackData(data))
		deleteRow = append(deleteRow, tu.InlineKeyboardButton(fmt.Sprintf("🗑 %d", i+1)).WithCallbackData(fmt.Sprintf("tasks:delete:%s", t.ID)))

		// Max 8 buttons per row for Telegram
		if len(pauseRow) >= 4 {
			rows = append(rows, pauseRow, deleteRow)
			pauseRow = nil
			deleteRow = nil
		}
	}
	if len(pauseRow) > 0 {
		rows = append(rows, pauseRow, deleteRow)
	}

	keyboard := &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
	if err := b.sendMessageWithKeyboard(ctx, chatID, sb.String(), keyboard); err != nil {
		slog.Error("failed to send task list", "error", err)
	}
}

// handleCallbackQuery processes inline button presses.
func (b *Bot) handleCallbackQuery(ctx context.Context, query telego.CallbackQuery) {
	data := query.Data
	chatID := query.Message.GetChat().ID

	// Answer the callback to remove the loading indicator
	_ = b.bot.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID,
	})

	userID := strconv.FormatInt(query.From.ID, 10)

	switch {
	case data == "tasks:list":
		b.sendTaskList(ctx, chatID, userID)

	case strings.HasPrefix(data, "tasks:pause:"):
		taskID := strings.TrimPrefix(data, "tasks:pause:")
		task, err := b.store.GetTask(taskID)
		if err != nil || task == nil {
			_ = b.SendMessage(ctx, chatID, "Задача не найдена.")
			return
		}
		// Verify ownership
		if task.UserID != userID {
			_ = b.SendMessage(ctx, chatID, "Это не ваша задача.")
			return
		}
		newStatus := "paused"
		action := "⏸ Задача приостановлена"
		if task.Status == "paused" {
			newStatus = "active"
			action = "▶️ Задача возобновлена"
		}
		if err := b.store.UpdateTaskStatus(taskID, newStatus); err != nil {
			_ = b.SendMessage(ctx, chatID, "Ошибка обновления задачи.")
			return
		}
		// If resuming, recalculate next run
		if newStatus == "active" {
			nextRun := schedule.CalculateNextRun(task.Schedule)
			if nextRun != nil {
				_ = b.store.UpdateTaskRun(taskID, task.LastStatus, task.LastError, nextRun)
			}
		}
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("%s: *%s*", action, task.Name))
		b.sendTaskList(ctx, chatID, userID)

	case strings.HasPrefix(data, "tasks:delete:"):
		taskID := strings.TrimPrefix(data, "tasks:delete:")
		task, err := b.store.GetTask(taskID)
		if err != nil || task == nil {
			_ = b.SendMessage(ctx, chatID, "Задача не найдена.")
			return
		}
		if task.UserID != userID {
			_ = b.SendMessage(ctx, chatID, "Это не ваша задача.")
			return
		}
		if err := b.store.DeleteTask(taskID); err != nil {
			_ = b.SendMessage(ctx, chatID, "Ошибка удаления задачи.")
			return
		}
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("🗑 Задача удалена: *%s*", task.Name))
		b.sendTaskList(ctx, chatID, userID)
	}
}

