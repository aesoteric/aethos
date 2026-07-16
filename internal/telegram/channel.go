package telegram

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aesoteric/aethos/internal/agent"
	channeltypes "github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/config"
	"github.com/aesoteric/aethos/internal/session"

	_ "modernc.org/sqlite"
)

const (
	assistantTopicName           = "Assistant"
	newSessionTopicName          = "New Session"
	telegramTextLimit            = 4000
	permissionCallbackPrefix     = "permission:"
	completedPermissionRetention = 10 * time.Minute
)

// Settings contains the Telegram and default Session values needed by the
// Telegram Channel at runtime.
type Settings struct {
	Token          string
	ChatID         int64
	AllowedUserIDs []int64
	DefaultAgent   string
	Workspace      string
}

// Option configures Telegram Channel timing at external boundaries.
type Option func(*channelOptions) error

type channelOptions struct {
	writeInterval time.Duration
	pollTimeout   time.Duration
}

// WithWriteInterval sets the minimum interval between streamed Telegram
// writes. The production default is conservative for a forum group's shared
// rate limit.
func WithWriteInterval(interval time.Duration) Option {
	return func(options *channelOptions) error {
		if interval <= 0 {
			return fmt.Errorf("telegram write interval must be greater than zero")
		}
		options.writeInterval = interval
		return nil
	}
}

// WithPollTimeout controls each Telegram long-poll request.
func WithPollTimeout(timeout time.Duration) Option {
	return func(options *channelOptions) error {
		if timeout <= 0 {
			return fmt.Errorf("telegram poll timeout must be greater than zero")
		}
		options.pollTimeout = timeout
		return nil
	}
}

type sessionTarget interface {
	Create(context.Context, session.Create) (session.Record, error)
	Prompt(context.Context, string, string) (agent.StopReason, error)
	Cancel(context.Context, string) error
	ResolvePermission(context.Context, string, string) error
	Get(context.Context, string) (session.Record, error)
	FindByTopic(context.Context, int64) (session.Record, error)
	Rename(context.Context, string, string) (session.Record, error)
	List(context.Context) ([]session.Record, error)
	CloseSession(context.Context, string) (session.Record, error)
}

// Channel translates Telegram updates into Session operations and streams
// aethos events back into their bound Topics.
type Channel struct {
	client   *Client
	settings Settings
	logger   *slog.Logger
	db       *sql.DB
	options  channelOptions
	allowed  map[int64]struct{}

	mu               sync.Mutex
	sessions         sessionTarget
	assistantTopicID int64
	drafts           map[string]*messageDraft
	permissions      map[string]*permissionMessage
	lanes            map[int64]chan inboundPrompt
	running          bool
	closed           bool
	cancel           context.CancelFunc
	workers          sync.WaitGroup
}

type inboundPrompt struct {
	sessionID string
	topicID   int64
	text      string
}

type permissionMessage struct {
	messageID   int64
	topicID     int64
	title       string
	options     []agent.PermissionOption
	completedAt time.Time
}

type messageDraft struct {
	topicID  int64
	chunks   []draftChunk
	lastKind fragmentKind
	dirty    bool
	updated  chan struct{}
}

type draftChunk struct {
	text      string
	messageID int64
	sent      string
}

type fragmentKind uint8

const (
	thoughtFragment fragmentKind = iota + 1
	messageFragment
	discreteFragment
)

type eventFragment struct {
	kind fragmentKind
	text string
}

// Open prepares the Telegram Channel's small durable state table. Session
// records and Topic bindings remain owned by the Session manager.
func Open(
	ctx context.Context,
	databasePath string,
	client *Client,
	logger *slog.Logger,
	settings Settings,
	option ...Option,
) (*Channel, error) {
	if !filepath.IsAbs(databasePath) {
		return nil, fmt.Errorf("database path must be absolute, got %q", databasePath)
	}
	if client == nil {
		return nil, fmt.Errorf("telegram client is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	if strings.TrimSpace(settings.Token) == "" {
		return nil, fmt.Errorf("telegram bot token is required")
	}
	if settings.ChatID == 0 {
		return nil, fmt.Errorf("telegram forum group ID is required")
	}
	if err := config.ValidateTelegramAllowedUserIDs(settings.AllowedUserIDs); err != nil {
		return nil, err
	}
	if strings.TrimSpace(settings.DefaultAgent) == "" {
		return nil, fmt.Errorf("default Agent is required")
	}
	if !filepath.IsAbs(settings.Workspace) {
		return nil, fmt.Errorf("default Workspace must be absolute, got %q", settings.Workspace)
	}
	options := channelOptions{writeInterval: 3 * time.Second, pollTimeout: 10 * time.Second}
	for _, configure := range option {
		if configure == nil {
			return nil, fmt.Errorf("telegram Channel option is required")
		}
		if err := configure(&options); err != nil {
			return nil, err
		}
	}
	allowed := make(map[int64]struct{}, len(settings.AllowedUserIDs))
	for _, userID := range settings.AllowedUserIDs {
		allowed[userID] = struct{}{}
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, fmt.Errorf("open Telegram state database %q: %w", databasePath, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return nil, errors.Join(fmt.Errorf("configure Telegram state database: %w", err), db.Close())
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS telegram_state (
		key TEXT PRIMARY KEY,
		value INTEGER NOT NULL
	) STRICT`); err != nil {
		return nil, errors.Join(fmt.Errorf("create Telegram state table: %w", err), db.Close())
	}

	channel := &Channel{
		client:      client,
		settings:    settings,
		logger:      logger,
		db:          db,
		options:     options,
		allowed:     allowed,
		drafts:      make(map[string]*messageDraft),
		permissions: make(map[string]*permissionMessage),
		lanes:       make(map[int64]chan inboundPrompt),
	}
	err = db.QueryRowContext(ctx, `SELECT value FROM telegram_state WHERE key = 'assistant_topic_id'`).Scan(&channel.assistantTopicID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, errors.Join(fmt.Errorf("load Assistant Topic binding: %w", err), db.Close())
	}
	return channel, nil
}

// Run validates the configured forum, bootstraps Assistant, and receives
// Telegram updates until ctx is cancelled.
func (c *Channel) Run(ctx context.Context, sessions sessionTarget) error {
	if sessions == nil {
		return fmt.Errorf("session target is required")
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return fmt.Errorf("telegram Channel is closed")
	}
	if c.running {
		c.mu.Unlock()
		cancel()
		return fmt.Errorf("telegram Channel is already running")
	}
	c.running = true
	c.sessions = sessions
	c.cancel = cancel
	c.mu.Unlock()
	defer func() {
		cancel()
		c.workers.Wait()
		c.mu.Lock()
		c.running = false
		c.cancel = nil
		c.mu.Unlock()
	}()

	configuredChat, err := c.client.getChat(runCtx, c.settings.Token, c.settings.ChatID)
	if err != nil {
		return fmt.Errorf("inspect configured Telegram forum: %w", err)
	}
	if configuredChat.ID != c.settings.ChatID || configuredChat.Type != "supergroup" || !configuredChat.IsForum {
		return fmt.Errorf("configured Telegram chat %d must be a forum supergroup", c.settings.ChatID)
	}
	if err := c.bootstrapAssistant(runCtx); err != nil {
		return err
	}

	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		c.stream(runCtx)
	}()
	var offset int64
	for {
		updates, err := c.client.getUpdates(runCtx, c.settings.Token, offset, c.options.pollTimeout)
		if err != nil {
			if runCtx.Err() != nil {
				return nil
			}
			c.logger.Warn("Telegram polling failed", "error", err)
			select {
			case <-time.After(time.Second):
				continue
			case <-runCtx.Done():
				return nil
			}
		}
		for _, one := range updates {
			if one.UpdateID >= offset {
				offset = one.UpdateID + 1
			}
			if err := c.handleUpdate(runCtx, one); err != nil {
				c.logger.Error("handle Telegram update", "update_id", one.UpdateID, "error", err)
			}
		}
		if runCtx.Err() != nil {
			return nil
		}
	}
}

func (c *Channel) bootstrapAssistant(ctx context.Context) error {
	c.mu.Lock()
	topicID := c.assistantTopicID
	c.mu.Unlock()
	if topicID == 0 {
		topic, err := c.client.createForumTopic(ctx, c.settings.Token, c.settings.ChatID, assistantTopicName)
		if err != nil {
			return fmt.Errorf("create Assistant Topic: %w", err)
		}
		topicID = topic.MessageThreadID
		if _, err := c.db.ExecContext(ctx, `INSERT INTO telegram_state(key, value)
			VALUES ('assistant_topic_id', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, topicID); err != nil {
			return fmt.Errorf("persist Assistant Topic binding: %w", err)
		}
		c.mu.Lock()
		c.assistantTopicID = topicID
		c.mu.Unlock()
	}
	status := "aethos is online. Send /new to create a Session, or /sessions to list and close Sessions."
	if _, err := c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, topicID, status); err != nil {
		return fmt.Errorf("post Assistant status: %w", err)
	}
	return nil
}

func (c *Channel) handleUpdate(ctx context.Context, one update) error {
	if one.CallbackQuery != nil {
		return c.handleCallback(ctx, one.CallbackQuery)
	}
	message := one.Message
	if message == nil {
		return nil
	}
	if message.Chat.ID != c.settings.ChatID {
		if message.From != nil {
			c.logger.Warn("rejected Telegram message from unconfigured chat",
				"telegram_chat_id", message.Chat.ID,
				"telegram_user_id", message.From.ID)
		} else {
			c.logger.Warn("rejected Telegram message from unconfigured chat", "telegram_chat_id", message.Chat.ID)
		}
		return nil
	}
	if message.From == nil {
		c.logger.Warn("rejected Telegram message without a user identity", "telegram_chat_id", message.Chat.ID)
		return nil
	}
	if _, ok := c.allowed[message.From.ID]; !ok {
		c.logger.Warn("rejected non-allowlisted Telegram user", "telegram_user_id", message.From.ID)
		return nil
	}
	if strings.TrimSpace(message.Text) == "" {
		return nil
	}

	c.mu.Lock()
	assistantTopicID := c.assistantTopicID
	c.mu.Unlock()
	switch message.MessageThreadID {
	case 0, 1:
		redirect := fmt.Sprintf("From General — %s:\n%s", displayName(message.From), message.Text)
		_, err := c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, assistantTopicID, redirect)
		return err
	case assistantTopicID:
		return c.handleAssistant(ctx, message)
	default:
		record, err := c.sessions.FindByTopic(ctx, message.MessageThreadID)
		if errors.Is(err, sql.ErrNoRows) {
			c.logger.Debug("ignored Telegram message in an unbound Topic", "topic_id", message.MessageThreadID)
			return nil
		}
		if err != nil {
			return err
		}
		command, _ := telegramCommand(message.Text)
		if command == "/cancel" {
			return c.cancelPrompt(ctx, record)
		}
		return c.enqueuePrompt(ctx, inboundPrompt{
			sessionID: record.ID,
			topicID:   record.TopicID,
			text:      strings.TrimSpace(message.Text),
		})
	}
}

func (c *Channel) handleCallback(ctx context.Context, query *callbackQuery) (returnErr error) {
	answer := ""
	defer func() {
		if query.ID == "" {
			return
		}
		returnErr = errors.Join(returnErr, c.client.answerCallbackQuery(
			ctx,
			c.settings.Token,
			query.ID,
			answer,
		))
	}()

	if query.From == nil || query.Message == nil || query.Message.Chat.ID != c.settings.ChatID {
		answer = "This action is not available."
		return nil
	}
	if _, ok := c.allowed[query.From.ID]; !ok {
		c.logger.Warn("rejected non-allowlisted Telegram callback", "telegram_user_id", query.From.ID)
		answer = "You are not allowed to use this control."
		return nil
	}
	requestID, optionIndex, err := parsePermissionCallback(query.Data)
	if err != nil {
		answer = "This action is no longer available."
		return nil
	}

	c.mu.Lock()
	pending := c.permissions[requestID]
	sessions := c.sessions
	if pending == nil || pending.messageID != query.Message.MessageID ||
		pending.topicID != query.Message.MessageThreadID || optionIndex >= len(pending.options) {
		c.mu.Unlock()
		answer = "This action is no longer available."
		return nil
	}
	optionID := pending.options[optionIndex].ID
	c.mu.Unlock()
	if sessions == nil {
		answer = "aethos is not ready."
		return nil
	}
	if err := sessions.ResolvePermission(ctx, requestID, optionID); err != nil {
		answer = "The permission response could not be recorded."
		return err
	}
	answer = "Permission response recorded."
	return nil
}

func parsePermissionCallback(data string) (string, int, error) {
	payload, ok := strings.CutPrefix(data, permissionCallbackPrefix)
	if !ok {
		return "", 0, fmt.Errorf("unknown Telegram callback")
	}
	requestID, rawIndex, ok := strings.Cut(payload, ":")
	if !ok || requestID == "" {
		return "", 0, fmt.Errorf("invalid Telegram permission callback")
	}
	index, err := strconv.Atoi(rawIndex)
	if err != nil || index < 0 {
		return "", 0, fmt.Errorf("invalid Telegram permission option index")
	}
	return requestID, index, nil
}

func (c *Channel) cancelPrompt(ctx context.Context, record session.Record) error {
	if err := c.sessions.Cancel(ctx, record.ID); err != nil {
		if errors.Is(err, session.ErrNoPrompt) {
			_, sendErr := c.client.sendMessage(
				ctx,
				c.settings.Token,
				c.settings.ChatID,
				record.TopicID,
				"No Prompt is currently running.",
			)
			return sendErr
		}
		return err
	}
	_, err := c.client.sendMessage(
		ctx,
		c.settings.Token,
		c.settings.ChatID,
		record.TopicID,
		"Prompt cancelled.",
	)
	return err
}

func telegramCommand(text string) (string, string) {
	command, argument, _ := strings.Cut(strings.TrimSpace(text), " ")
	if name, _, found := strings.Cut(command, "@"); found {
		command = name
	}
	return command, strings.TrimSpace(argument)
}

func (c *Channel) handleAssistant(ctx context.Context, message *message) error {
	command, argument := telegramCommand(message.Text)
	switch command {
	case "/sessions":
		return c.sendSessionList(ctx)
	case "/close":
		return c.closeSession(ctx, argument)
	case "/new":
	default:
		_, err := c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, c.assistantTopicID,
			"Send /new to create a Session, /sessions to list Sessions, or /close <Session ID> to close one.")
		return err
	}

	workspace, agentCommand, err := c.sessionSelection(argument)
	if err != nil {
		_, sendErr := c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, c.assistantTopicID, err.Error())
		return errors.Join(err, sendErr)
	}
	topic, err := c.client.createForumTopic(ctx, c.settings.Token, c.settings.ChatID, newSessionTopicName)
	if err != nil {
		return fmt.Errorf("create Session Topic: %w", err)
	}
	record, err := c.sessions.Create(ctx, session.Create{
		Agent:     agentCommand,
		Workspace: workspace,
		Owner: session.Owner{
			Channel: "telegram",
			ID:      strconv.FormatInt(message.From.ID, 10),
		},
		TopicID: topic.MessageThreadID,
	})
	if err != nil {
		cleanupErr := c.client.deleteForumTopic(ctx, c.settings.Token, c.settings.ChatID, topic.MessageThreadID)
		return errors.Join(fmt.Errorf("create Session: %w", err), cleanupErr)
	}
	if _, err := c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, record.TopicID,
		"Session ready. Send a Prompt in this Topic."); err != nil {
		return fmt.Errorf("post Session status: %w", err)
	}
	return nil
}

func (c *Channel) sendSessionList(ctx context.Context) error {
	records, err := c.sessions.List(ctx)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		_, err := c.client.sendMessage(
			ctx,
			c.settings.Token,
			c.settings.ChatID,
			c.assistantTopicID,
			"No Sessions.",
		)
		return err
	}
	if _, err := c.client.sendMessage(
		ctx,
		c.settings.Token,
		c.settings.ChatID,
		c.assistantTopicID,
		"Sessions:",
	); err != nil {
		return err
	}
	for _, record := range records {
		name := strings.TrimSpace(record.Name)
		if name == "" {
			name = "(unnamed)"
		}
		text := fmt.Sprintf(
			"%s\nState: %s\nAgent: %s\nName: %s",
			record.ID,
			record.State,
			record.Agent,
			name,
		)
		if _, err := c.client.sendMessage(
			ctx,
			c.settings.Token,
			c.settings.ChatID,
			c.assistantTopicID,
			text,
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *Channel) closeSession(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		_, err := c.client.sendMessage(
			ctx,
			c.settings.Token,
			c.settings.ChatID,
			c.assistantTopicID,
			"Usage: /close <Session ID>",
		)
		return err
	}
	record, err := c.sessions.CloseSession(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, sendErr := c.client.sendMessage(
				ctx,
				c.settings.Token,
				c.settings.ChatID,
				c.assistantTopicID,
				"Session not found: "+id,
			)
			return sendErr
		}
		if errors.Is(err, session.ErrInvalidTransition) {
			_, sendErr := c.client.sendMessage(
				ctx,
				c.settings.Token,
				c.settings.ChatID,
				c.assistantTopicID,
				"Session is already closed: "+id,
			)
			return sendErr
		}
		return err
	}
	name := strings.TrimSpace(record.Name)
	if name == "" {
		name = record.ID
	}
	_, err = c.client.sendMessage(
		ctx,
		c.settings.Token,
		c.settings.ChatID,
		c.assistantTopicID,
		fmt.Sprintf("Session closed: %s (%s).", name, record.ID),
	)
	return err
}

func (c *Channel) sessionSelection(argument string) (string, string, error) {
	argument = strings.TrimSpace(argument)
	if argument == "" {
		return c.settings.Workspace, c.settings.DefaultAgent, nil
	}
	workspace, agentCommand, found := strings.Cut(argument, "|")
	if !found {
		return "", "", fmt.Errorf("usage: /new <Workspace> | <Agent command>")
	}
	workspace = strings.TrimSpace(workspace)
	agentCommand = strings.TrimSpace(agentCommand)
	if workspace == "" {
		workspace = c.settings.Workspace
	}
	if agentCommand == "" {
		agentCommand = c.settings.DefaultAgent
	}
	if !filepath.IsAbs(workspace) {
		return "", "", fmt.Errorf("workspace must be an absolute path")
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return "", "", fmt.Errorf("open Workspace %q: %w", workspace, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("workspace %q is not a directory", workspace)
	}
	return workspace, agentCommand, nil
}

func (c *Channel) enqueuePrompt(ctx context.Context, prompt inboundPrompt) error {
	c.mu.Lock()
	lane := c.lanes[prompt.topicID]
	if lane == nil {
		lane = make(chan inboundPrompt, 64)
		c.lanes[prompt.topicID] = lane
		c.workers.Add(1)
		go func() {
			defer c.workers.Done()
			c.runPromptLane(ctx, prompt.topicID, lane)
		}()
	}
	c.mu.Unlock()
	select {
	case lane <- prompt:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Channel) runPromptLane(ctx context.Context, topicID int64, lane <-chan inboundPrompt) {
	defer func() {
		c.mu.Lock()
		delete(c.lanes, topicID)
		c.mu.Unlock()
	}()
	for {
		select {
		case prompt := <-lane:
			c.handlePrompt(ctx, prompt)
		case <-ctx.Done():
			return
		}
	}
}

func (c *Channel) handlePrompt(ctx context.Context, prompt inboundPrompt) {
	record, err := c.sessions.Get(ctx, prompt.sessionID)
	if err != nil {
		c.reportPromptError(ctx, prompt.topicID, err)
		return
	}
	if record.Name == "" {
		name := sessionName(prompt.text)
		renamed, err := c.sessions.Rename(ctx, record.ID, name)
		if err != nil {
			c.reportPromptError(ctx, record.TopicID, err)
			return
		}
		record = renamed
		if err := c.client.editForumTopic(ctx, c.settings.Token, c.settings.ChatID, record.TopicID, name); err != nil {
			c.logger.Warn("rename Telegram Session Topic", "session", record.ID, "topic_id", record.TopicID, "error", err)
		}
	}
	c.beginDraft(record.ID, record.TopicID)
	_, err = c.sessions.Prompt(ctx, record.ID, prompt.text)
	flushErr := c.waitForDraft(ctx, record.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		c.reportPromptError(ctx, record.TopicID, err)
		return
	}
	if flushErr != nil && ctx.Err() == nil {
		c.logger.Warn("finish Telegram Agent output", "session", record.ID, "error", flushErr)
	}
}

func (c *Channel) reportPromptError(ctx context.Context, topicID int64, promptErr error) {
	c.logger.Error("Telegram Prompt failed", "topic_id", topicID, "error", promptErr)
	if _, err := c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, topicID,
		"Prompt failed: "+promptErr.Error()); err != nil {
		c.logger.Error("report Telegram Prompt failure", "topic_id", topicID, "error", err)
	}
}

// Send implements channel.Channel. It queues rendered Agent events so the ACP
// stream never blocks on Telegram's shared forum-group rate limit.
func (c *Channel) Send(ctx context.Context, event channeltypes.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	sessions := c.sessions
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return fmt.Errorf("telegram Channel is closed")
	}
	if sessions == nil {
		return fmt.Errorf("telegram Channel is not running")
	}
	record, err := sessions.Get(ctx, event.SessionID)
	if err != nil {
		return err
	}
	if record.TopicID == 0 || record.Owner.Channel != "telegram" {
		return nil
	}
	switch permissionEvent := event.AgentEvent.(type) {
	case agent.PermissionRequested:
		return c.presentPermission(ctx, record.TopicID, permissionEvent)
	case agent.PermissionResolved:
		return c.finishPermission(ctx, permissionEvent)
	}
	fragment := renderEvent(event.AgentEvent)
	if fragment.text == "" {
		return nil
	}
	c.appendDraft(record.ID, record.TopicID, fragment)
	return nil
}

func (c *Channel) presentPermission(ctx context.Context, topicID int64, request agent.PermissionRequested) error {
	buttons := make([]inlineKeyboardButton, 0, len(request.Options))
	for index, option := range request.Options {
		callbackData := fmt.Sprintf("%s%s:%d", permissionCallbackPrefix, request.ID, index)
		if len([]byte(callbackData)) > 64 {
			return fmt.Errorf("permission callback data is %d bytes, exceeds Telegram's 64-byte limit", len([]byte(callbackData)))
		}
		buttons = append(buttons, inlineKeyboardButton{
			Text:         permissionButtonLabel(option),
			CallbackData: callbackData,
		})
	}
	if len(buttons) == 0 {
		return fmt.Errorf("permission request %q offered no response options", request.ID)
	}
	rows := make([][]inlineKeyboardButton, 0, len(buttons))
	for _, button := range buttons {
		rows = append(rows, []inlineKeyboardButton{button})
	}
	sent, err := c.client.sendMessageWithReplyMarkup(
		ctx,
		c.settings.Token,
		c.settings.ChatID,
		topicID,
		permissionRequestText(request.Title, request.Kind),
		&inlineKeyboardMarkup{InlineKeyboard: rows},
	)
	if err != nil {
		return err
	}

	c.mu.Lock()
	for id, one := range c.permissions {
		if !one.completedAt.IsZero() && time.Since(one.completedAt) >= completedPermissionRetention {
			delete(c.permissions, id)
		}
	}
	c.permissions[request.ID] = &permissionMessage{
		messageID: sent.MessageID,
		topicID:   topicID,
		title:     request.Title,
		options:   append([]agent.PermissionOption(nil), request.Options...),
	}
	c.mu.Unlock()
	return nil
}

func (c *Channel) finishPermission(ctx context.Context, result agent.PermissionResolved) error {
	c.mu.Lock()
	pending := c.permissions[result.ID]
	if pending == nil {
		c.mu.Unlock()
		return nil
	}
	pending.completedAt = time.Now()
	messageID := pending.messageID
	title := pending.title
	options := append([]agent.PermissionOption(nil), pending.options...)
	c.mu.Unlock()

	outcome := permissionOutcome(result, options)
	return c.client.editMessageTextWithReplyMarkup(
		ctx,
		c.settings.Token,
		c.settings.ChatID,
		messageID,
		permissionRequestText(title, "")+"\nOutcome: "+outcome,
		&inlineKeyboardMarkup{InlineKeyboard: [][]inlineKeyboardButton{}},
	)
}

func permissionRequestText(title, kind string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Agent action"
	}
	text := "Permission requested: " + title
	if kind != "" {
		text += "\nKind: " + kind
	}
	return text
}

func permissionButtonLabel(option agent.PermissionOption) string {
	switch option.Kind {
	case agent.PermissionAllowOnce:
		return "Approve"
	case agent.PermissionAllowAlways:
		return "Always approve"
	case agent.PermissionRejectOnce:
		return "Deny"
	case agent.PermissionRejectAlways:
		return "Always deny"
	default:
		if name := strings.TrimSpace(option.Name); name != "" {
			return name
		}
		return "Respond"
	}
}

func permissionOutcome(result agent.PermissionResolved, options []agent.PermissionOption) string {
	if result.TimedOut {
		return "Timed out — denied"
	}
	if result.Cancelled {
		return "Cancelled"
	}
	for _, option := range options {
		if option.ID != result.OptionID {
			continue
		}
		switch option.Kind {
		case agent.PermissionAllowOnce, agent.PermissionAllowAlways:
			return "Approved"
		case agent.PermissionRejectOnce, agent.PermissionRejectAlways:
			return "Denied"
		}
		if name := strings.TrimSpace(option.Name); name != "" {
			return name
		}
	}
	return "Resolved"
}

func (c *Channel) beginDraft(sessionID string, topicID int64) {
	c.mu.Lock()
	c.drafts[sessionID] = newMessageDraft(topicID)
	c.mu.Unlock()
}

func newMessageDraft(topicID int64) *messageDraft {
	return &messageDraft{topicID: topicID, updated: make(chan struct{}, 1)}
}

func (c *Channel) appendDraft(sessionID string, topicID int64, fragment eventFragment) {
	c.mu.Lock()
	draft := c.drafts[sessionID]
	if draft == nil {
		draft = newMessageDraft(topicID)
		c.drafts[sessionID] = draft
	}
	continuous := fragment.kind == draft.lastKind &&
		(fragment.kind == thoughtFragment || fragment.kind == messageFragment)
	if len(draft.chunks) > 0 && draft.chunks[len(draft.chunks)-1].text != "" && !continuous {
		appendDraftText(draft, "\n\n")
	}
	if fragment.kind == thoughtFragment && draft.lastKind != thoughtFragment {
		appendDraftText(draft, "Thinking\n")
	}
	appendDraftText(draft, fragment.text)
	draft.lastKind = fragment.kind
	draft.dirty = true
	notifyDraft(draft)
	c.mu.Unlock()
}

func appendDraftText(draft *messageDraft, text string) {
	remaining := []rune(text)
	if len(draft.chunks) == 0 {
		draft.chunks = append(draft.chunks, draftChunk{})
	}
	for len(remaining) > 0 {
		last := len(draft.chunks) - 1
		space := telegramTextLimit - utf8.RuneCountInString(draft.chunks[last].text)
		if space == 0 {
			draft.chunks = append(draft.chunks, draftChunk{})
			continue
		}
		count := min(space, len(remaining))
		draft.chunks[last].text += string(remaining[:count])
		remaining = remaining[count:]
	}
}

func (c *Channel) stream(ctx context.Context) {
	ticker := time.NewTicker(c.options.writeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.flushOne(ctx)
		case <-ctx.Done():
			return
		}
	}
}

type draftWrite struct {
	sessionID string
	draft     *messageDraft
	index     int
	topicID   int64
	messageID int64
	text      string
}

func (c *Channel) flushOne(ctx context.Context) {
	write, ok := c.nextDraftWrite()
	if !ok {
		return
	}
	var err error
	var sentMessage message
	if write.messageID == 0 {
		sentMessage, err = c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, write.topicID, write.text)
	} else {
		err = c.client.editMessageText(ctx, c.settings.Token, c.settings.ChatID, write.messageID, write.text)
	}
	if err != nil {
		c.logger.Warn("stream Agent output to Telegram", "session", write.sessionID, "error", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.drafts[write.sessionID]
	if current != write.draft || write.index >= len(current.chunks) {
		return
	}
	chunk := &current.chunks[write.index]
	if write.messageID == 0 && chunk.messageID == 0 {
		chunk.messageID = sentMessage.MessageID
	}
	chunk.sent = write.text
	current.dirty = draftNeedsWrite(current)
	notifyDraft(current)
}

func (c *Channel) nextDraftWrite() (draftWrite, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sessionID, draft := range c.drafts {
		if !draft.dirty {
			continue
		}
		for index, chunk := range draft.chunks {
			if chunk.messageID == 0 || chunk.sent != chunk.text {
				return draftWrite{
					sessionID: sessionID,
					draft:     draft,
					index:     index,
					topicID:   draft.topicID,
					messageID: chunk.messageID,
					text:      chunk.text,
				}, true
			}
		}
		draft.dirty = false
		notifyDraft(draft)
	}
	return draftWrite{}, false
}

func draftNeedsWrite(draft *messageDraft) bool {
	for _, chunk := range draft.chunks {
		if chunk.messageID == 0 || chunk.sent != chunk.text {
			return true
		}
	}
	return false
}

func notifyDraft(draft *messageDraft) {
	select {
	case draft.updated <- struct{}{}:
	default:
	}
}

func (c *Channel) waitForDraft(ctx context.Context, sessionID string) error {
	for {
		c.mu.Lock()
		draft := c.drafts[sessionID]
		if draft == nil || !draft.dirty {
			c.mu.Unlock()
			return nil
		}
		updated := draft.updated
		c.mu.Unlock()
		select {
		case <-updated:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func renderEvent(event agent.Event) eventFragment {
	switch one := event.(type) {
	case agent.Thought:
		return eventFragment{kind: thoughtFragment, text: one.Text}
	case agent.Message:
		return eventFragment{kind: messageFragment, text: one.Text}
	case agent.ToolCallBegan:
		line := "Tool: " + one.Title
		if one.Kind != "" {
			line += " [" + one.Kind + "]"
		}
		if one.Status != "" {
			line += " — " + one.Status
		}
		return eventFragment{kind: discreteFragment, text: line}
	case agent.ToolCallProgressed:
		line := "Tool update"
		if one.Title != "" {
			line += ": " + one.Title
		}
		if one.Status != "" {
			line += " — " + one.Status
		}
		return eventFragment{kind: discreteFragment, text: line}
	case agent.PermissionRequested:
		return eventFragment{kind: discreteFragment, text: "Permission requested: " + one.Title}
	case agent.Crashed:
		return eventFragment{kind: discreteFragment, text: "Agent stopped: " + one.Error}
	default:
		return eventFragment{}
	}
}

func sessionName(prompt string) string {
	name := strings.Join(strings.Fields(prompt), " ")
	runes := []rune(name)
	if len(runes) > 80 {
		name = strings.TrimSpace(string(runes[:79])) + "…"
	}
	if name == "" {
		return newSessionTopicName
	}
	return name
}

func displayName(from *user) string {
	if name := strings.TrimSpace(from.FirstName); name != "" {
		return name
	}
	return strconv.FormatInt(from.ID, 10)
}

// Close stops a running Channel and releases its durable state connection.
func (c *Channel) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cancelRun := c.cancel
	c.mu.Unlock()
	if cancelRun != nil {
		cancelRun()
	}
	if err := c.db.Close(); err != nil {
		return fmt.Errorf("close Telegram state database: %w", err)
	}
	return nil
}

var _ channeltypes.Channel = (*Channel)(nil)
