package slack

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/agentcatalog"
	channeltypes "github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/session"
	"github.com/coder/websocket"
)

const (
	channelName    = "slack"
	slackTextLimit = 4000
)

type sessionTarget interface {
	Create(context.Context, session.Create) (session.Record, error)
	Prompt(context.Context, string, string) (agent.StopReason, error)
	Get(context.Context, string) (session.Record, error)
	FindByTopic(context.Context, string, string) (session.Record, error)
	Rename(context.Context, string, string) (session.Record, error)
}

// Settings contains the credentials and access policy needed by the Slack
// Channel at runtime.
type Settings struct {
	AppToken       string
	BotToken       string
	ChannelID      string
	AllowedUserIDs []string
	DefaultAgent   string
	Workspace      string
	Agents         agentcatalog.InstalledLister
}

// Option configures Slack Channel timing at external boundaries.
type Option func(*channelOptions) error

type channelOptions struct {
	initialBackoff time.Duration
	maximumBackoff time.Duration
	writeInterval  time.Duration
}

// WithReconnectBackoff controls the exponential delay between Socket Mode
// connection attempts.
func WithReconnectBackoff(initial, maximum time.Duration) Option {
	return func(options *channelOptions) error {
		if initial <= 0 || maximum < initial {
			return fmt.Errorf("slack reconnect backoff must be positive and maximum must not be less than initial")
		}
		options.initialBackoff = initial
		options.maximumBackoff = maximum
		return nil
	}
}

// WithWriteInterval sets the minimum interval between streamed Slack writes.
func WithWriteInterval(interval time.Duration) Option {
	return func(options *channelOptions) error {
		if interval <= 0 {
			return fmt.Errorf("slack write interval must be greater than zero")
		}
		options.writeInterval = interval
		return nil
	}
}

// Channel receives Slack Socket Mode envelopes and translates them into
// aethos Assistant operations.
type Channel struct {
	client   *Client
	logger   *slog.Logger
	settings Settings
	options  channelOptions
	allowed  map[string]struct{}
	identity authIdentity

	mu       sync.Mutex
	sessions sessionTarget
	lanes    map[string]chan inboundPrompt
	drafts   map[string]*messageDraft
	running  bool
	workers  sync.WaitGroup
}

type inboundPrompt struct {
	sessionID string
	threadTS  string
	text      string
}

type messageDraft struct {
	threadTS string
	chunks   []draftChunk
	lastKind fragmentKind
	dirty    bool
	updated  chan struct{}
}

type draftChunk struct {
	text string
	ts   string
	sent string
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

// New constructs a Slack Channel.
func New(client *Client, logger *slog.Logger, settings Settings, option ...Option) (*Channel, error) {
	if client == nil {
		return nil, fmt.Errorf("slack client is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	settings.AppToken = strings.TrimSpace(settings.AppToken)
	settings.BotToken = strings.TrimSpace(settings.BotToken)
	settings.ChannelID = strings.TrimSpace(settings.ChannelID)
	settings.DefaultAgent = strings.TrimSpace(settings.DefaultAgent)
	if settings.AppToken == "" {
		return nil, fmt.Errorf("slack app token is required")
	}
	if settings.BotToken == "" {
		return nil, fmt.Errorf("slack bot token is required")
	}
	if settings.ChannelID == "" {
		return nil, fmt.Errorf("slack channel ID is required")
	}
	if len(settings.AllowedUserIDs) == 0 {
		return nil, fmt.Errorf("slack allowed user IDs are required")
	}
	if settings.DefaultAgent == "" {
		return nil, fmt.Errorf("default Agent is required")
	}
	if !filepath.IsAbs(settings.Workspace) {
		return nil, fmt.Errorf("default Workspace must be absolute, got %q", settings.Workspace)
	}
	allowed := make(map[string]struct{}, len(settings.AllowedUserIDs))
	for _, userID := range settings.AllowedUserIDs {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			return nil, fmt.Errorf("slack allowed user ID cannot be empty")
		}
		allowed[userID] = struct{}{}
	}
	options := channelOptions{
		initialBackoff: time.Second,
		maximumBackoff: 30 * time.Second,
		writeInterval:  time.Second,
	}
	for _, configure := range option {
		if configure == nil {
			return nil, fmt.Errorf("slack Channel option is required")
		}
		if err := configure(&options); err != nil {
			return nil, err
		}
	}
	return &Channel{
		client: client, logger: logger, settings: settings, options: options,
		allowed: allowed, lanes: make(map[string]chan inboundPrompt), drafts: make(map[string]*messageDraft),
	}, nil
}

// Run authenticates the bot and receives Socket Mode envelopes until ctx is
// cancelled. Every reconnect obtains a fresh temporary websocket URL.
func (c *Channel) Run(ctx context.Context, sessions sessionTarget) error {
	if sessions == nil {
		return fmt.Errorf("session target is required")
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		cancel()
		return fmt.Errorf("slack Channel is already running")
	}
	c.running = true
	c.sessions = sessions
	c.mu.Unlock()
	defer func() {
		cancel()
		c.workers.Wait()
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	identity, err := c.client.authenticate(runCtx, c.settings.BotToken)
	if err != nil {
		return fmt.Errorf("authenticate Slack bot: %w", err)
	}
	c.identity = identity
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		c.stream(runCtx)
	}()
	backoff := c.options.initialBackoff
	for {
		err := c.runConnection(runCtx)
		if runCtx.Err() != nil {
			return nil
		}
		if err != nil {
			c.logger.Warn("Slack Socket Mode connection ended", "error", err, "retry_in", backoff)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-runCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		}
		backoff = min(backoff*2, c.options.maximumBackoff)
	}
}

func (c *Channel) runConnection(ctx context.Context) error {
	socketURL, err := c.client.openSocketURL(ctx, c.settings.AppToken)
	if err != nil {
		return fmt.Errorf("open Slack Socket Mode connection: %w", err)
	}
	connection, _, err := websocket.Dial(ctx, socketURL, &websocket.DialOptions{HTTPClient: c.client.httpClient})
	if err != nil {
		return fmt.Errorf("connect Slack Socket Mode websocket: %w", err)
	}
	defer func() { _ = connection.CloseNow() }()
	for {
		messageType, contents, err := connection.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageText {
			continue
		}
		var envelope struct {
			Type       string          `json:"type"`
			EnvelopeID string          `json:"envelope_id"`
			Reason     string          `json:"reason"`
			Payload    json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(contents, &envelope); err != nil {
			c.logger.Warn("ignored invalid Slack Socket Mode envelope", "error", err)
			continue
		}
		if envelope.EnvelopeID != "" {
			acknowledgement, err := json.Marshal(struct {
				EnvelopeID string `json:"envelope_id"`
			}{EnvelopeID: envelope.EnvelopeID})
			if err != nil {
				return err
			}
			if err := connection.Write(ctx, websocket.MessageText, acknowledgement); err != nil {
				return fmt.Errorf("acknowledge Slack envelope %q: %w", envelope.EnvelopeID, err)
			}
		}
		if envelope.Type == "disconnect" {
			return fmt.Errorf("slack requested disconnect: %s", envelope.Reason)
		}
		if envelope.Type == "events_api" {
			if err := c.handleEvent(ctx, envelope.Payload); err != nil {
				c.logger.Error("handle Slack event", "envelope_id", envelope.EnvelopeID, "error", err)
			}
		}
	}
}

func (c *Channel) handleEvent(ctx context.Context, payload json.RawMessage) error {
	var callback struct {
		Type  string `json:"type"`
		Event struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			User     string `json:"user"`
			BotID    string `json:"bot_id"`
			Channel  string `json:"channel"`
			Text     string `json:"text"`
			ThreadTS string `json:"thread_ts"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &callback); err != nil {
		return fmt.Errorf("decode Slack event payload: %w", err)
	}
	event := callback.Event
	if callback.Type != "event_callback" || event.Type != "message" || event.Subtype != "" || event.BotID != "" {
		return nil
	}
	if event.Channel != c.settings.ChannelID || event.User == "" || event.User == c.identity.UserID {
		return nil
	}
	if _, allowed := c.allowed[event.User]; !allowed {
		return nil
	}
	if event.ThreadTS != "" {
		text := strings.TrimSpace(event.Text)
		if text == "" {
			return nil
		}
		record, err := c.sessions.FindByTopic(ctx, channelName, event.ThreadTS)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return c.enqueuePrompt(ctx, inboundPrompt{
			sessionID: record.ID,
			threadTS:  event.ThreadTS,
			text:      text,
		})
	}
	command, argument, _ := strings.Cut(strings.TrimSpace(event.Text), " ")
	if command == "new" {
		return c.createSession(ctx, event.User, strings.TrimSpace(argument))
	}
	if command == "agents" {
		return c.sendAgentList(ctx)
	}
	_, err := c.client.PostMessage(
		ctx, c.settings.BotToken, c.settings.ChannelID, "", "Send agents to list installed Agents.",
	)
	return err
}

func (c *Channel) enqueuePrompt(ctx context.Context, prompt inboundPrompt) error {
	c.mu.Lock()
	lane := c.lanes[prompt.threadTS]
	if lane == nil {
		lane = make(chan inboundPrompt, 64)
		c.lanes[prompt.threadTS] = lane
		c.workers.Add(1)
		go func() {
			defer c.workers.Done()
			c.runPromptLane(ctx, prompt.threadTS, lane)
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

func (c *Channel) runPromptLane(ctx context.Context, threadTS string, lane <-chan inboundPrompt) {
	defer func() {
		c.mu.Lock()
		delete(c.lanes, threadTS)
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
		c.reportPromptError(ctx, prompt.threadTS, err)
		return
	}
	if record.Name == "" {
		record, err = c.sessions.Rename(ctx, record.ID, sessionName(prompt.text))
		if err != nil {
			c.reportPromptError(ctx, prompt.threadTS, err)
			return
		}
		if err := c.client.UpdateMessage(
			ctx, c.settings.BotToken, c.settings.ChannelID, prompt.threadTS, rootText(record),
		); err != nil {
			c.logger.Warn("rename Slack Session thread", "session", record.ID, "thread_ts", prompt.threadTS, "error", err)
		}
	}
	if _, err := c.sessions.Prompt(ctx, record.ID, prompt.text); err != nil && !errors.Is(err, context.Canceled) {
		c.logger.Error("Slack Prompt failed", "session", record.ID, "thread_ts", prompt.threadTS, "error", err)
	}
}

func (c *Channel) reportPromptError(ctx context.Context, threadTS string, promptErr error) {
	c.logger.Error("Slack Prompt failed", "thread_ts", threadTS, "error", promptErr)
	if _, err := c.client.PostMessage(
		ctx, c.settings.BotToken, c.settings.ChannelID, threadTS, "Prompt failed: "+promptErr.Error(),
	); err != nil {
		c.logger.Error("report Slack Prompt failure", "thread_ts", threadTS, "error", err)
	}
}

func rootText(record session.Record) string {
	name := strings.TrimSpace(record.Name)
	if name == "" {
		name = "New Session"
	}
	return fmt.Sprintf("%s\nState: %s", name, record.State)
}

func sessionName(prompt string) string {
	name := strings.Join(strings.Fields(prompt), " ")
	runes := []rune(name)
	if len(runes) > 80 {
		name = strings.TrimSpace(string(runes[:79])) + "…"
	}
	if name == "" {
		return "New Session"
	}
	return name
}

func (c *Channel) createSession(ctx context.Context, userID, argument string) error {
	workspace, agentID, err := c.sessionSelection(argument)
	if err != nil {
		_, sendErr := c.client.PostMessage(ctx, c.settings.BotToken, c.settings.ChannelID, "", err.Error())
		return sendErr
	}
	root, err := c.client.PostMessage(
		ctx, c.settings.BotToken, c.settings.ChannelID, "", "New Session\nState: live",
	)
	if err != nil {
		return fmt.Errorf("post Slack Session root: %w", err)
	}
	if _, err := c.sessions.Create(ctx, session.Create{
		Agent:     session.AgentRef(agentID),
		Workspace: workspace,
		Owner:     session.Owner{Channel: channelName, ID: userID},
		TopicKey:  root.TS,
	}); err != nil {
		return fmt.Errorf("create Slack Session: %w", err)
	}
	if _, err := c.client.PostMessage(
		ctx, c.settings.BotToken, c.settings.ChannelID, root.TS,
		"Session ready. Send a Prompt in this thread.",
	); err != nil {
		return fmt.Errorf("post Slack Session status: %w", err)
	}
	return nil
}

func (c *Channel) sessionSelection(argument string) (string, string, error) {
	argument = strings.TrimSpace(argument)
	if argument == "" {
		return c.settings.Workspace, c.settings.DefaultAgent, nil
	}
	workspace, agentID, found := strings.Cut(argument, "|")
	if !found {
		return "", "", fmt.Errorf("usage: new <Workspace> | <installed Agent ID>")
	}
	workspace = strings.TrimSpace(workspace)
	agentID = strings.TrimSpace(agentID)
	if workspace == "" {
		workspace = c.settings.Workspace
	}
	if agentID == "" {
		agentID = c.settings.DefaultAgent
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
	if c.settings.Agents == nil {
		return "", "", fmt.Errorf("installed Agent catalog is unavailable")
	}
	installed, err := c.settings.Agents.Installed()
	if err != nil {
		return "", "", fmt.Errorf("list installed Agents: %w", err)
	}
	for _, installedAgent := range installed {
		if installedAgent.ID == agentID {
			return workspace, agentID, nil
		}
	}
	return "", "", fmt.Errorf("Agent %q is not installed", agentID)
}

// Send implements channel.Channel. Agent events are accumulated in a draft so
// the Agent stream does not block on Slack's per-channel write rate.
func (c *Channel) Send(ctx context.Context, event channeltypes.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := c.sessionRecord(ctx, event.SessionID)
	if err != nil {
		return err
	}
	if record.Owner.Channel != channelName || record.TopicKey == "" {
		return nil
	}
	fragment := renderEvent(event.AgentEvent)
	if fragment.text == "" {
		return nil
	}
	c.appendDraft(record.ID, record.TopicKey, fragment)
	return nil
}

// SendLifecycle implements channel.LifecycleChannel.
func (c *Channel) SendLifecycle(ctx context.Context, event channeltypes.LifecycleEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := c.sessionRecord(ctx, event.SessionID)
	if err != nil {
		return err
	}
	if record.Owner.Channel != channelName || record.TopicKey == "" {
		return nil
	}
	switch lifecycle := event.SessionEvent.(type) {
	case channeltypes.SessionStateChanged:
		return c.client.UpdateMessage(
			ctx, c.settings.BotToken, c.settings.ChannelID, record.TopicKey, rootText(record),
		)
	case channeltypes.PromptStarted:
		c.beginDraft(record.ID, record.TopicKey)
		_, err := c.client.PostMessage(
			ctx, c.settings.BotToken, c.settings.ChannelID, record.TopicKey, "Prompt started.",
		)
		return err
	case channeltypes.PromptFinished:
		flushErr := c.waitForDraft(ctx, record.ID)
		text := "Prompt finished: " + string(lifecycle.StopReason) + "."
		if lifecycle.Error != "" {
			text = "Prompt finished with error: " + lifecycle.Error
		}
		_, postErr := c.client.PostMessage(
			ctx, c.settings.BotToken, c.settings.ChannelID, record.TopicKey, text,
		)
		return errors.Join(flushErr, postErr)
	default:
		return nil
	}
}

func (c *Channel) sessionRecord(ctx context.Context, sessionID string) (session.Record, error) {
	c.mu.Lock()
	sessions := c.sessions
	c.mu.Unlock()
	if sessions == nil {
		return session.Record{}, fmt.Errorf("slack Channel is not running")
	}
	return sessions.Get(ctx, sessionID)
}

func (c *Channel) beginDraft(sessionID, threadTS string) {
	c.mu.Lock()
	c.drafts[sessionID] = newMessageDraft(threadTS)
	c.mu.Unlock()
}

func newMessageDraft(threadTS string) *messageDraft {
	return &messageDraft{threadTS: threadTS, updated: make(chan struct{}, 1)}
}

func (c *Channel) appendDraft(sessionID, threadTS string, fragment eventFragment) {
	c.mu.Lock()
	draft := c.drafts[sessionID]
	if draft == nil {
		draft = newMessageDraft(threadTS)
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
		space := slackTextLimit - utf8.RuneCountInString(draft.chunks[last].text)
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
	threadTS  string
	messageTS string
	text      string
}

func (c *Channel) flushOne(ctx context.Context) {
	write, ok := c.nextDraftWrite()
	if !ok {
		return
	}
	var err error
	var posted Message
	if write.messageTS == "" {
		posted, err = c.client.PostMessage(
			ctx, c.settings.BotToken, c.settings.ChannelID, write.threadTS, write.text,
		)
	} else {
		err = c.client.UpdateMessage(
			ctx, c.settings.BotToken, c.settings.ChannelID, write.messageTS, write.text,
		)
	}
	if err != nil {
		c.logger.Warn("stream Agent output to Slack", "session", write.sessionID, "error", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.drafts[write.sessionID]
	if current != write.draft || write.index >= len(current.chunks) {
		return
	}
	chunk := &current.chunks[write.index]
	if write.messageTS == "" && chunk.ts == "" {
		chunk.ts = posted.TS
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
			if chunk.ts == "" || chunk.sent != chunk.text {
				return draftWrite{
					sessionID: sessionID,
					draft:     draft,
					index:     index,
					threadTS:  draft.threadTS,
					messageTS: chunk.ts,
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
		if chunk.ts == "" || chunk.sent != chunk.text {
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
	case agent.Crashed:
		return eventFragment{kind: discreteFragment, text: "Agent stopped: " + one.Error}
	default:
		return eventFragment{}
	}
}

func (c *Channel) sendAgentList(ctx context.Context) error {
	var installed []agentcatalog.InstalledAgent
	if c.settings.Agents != nil {
		var err error
		installed, err = c.settings.Agents.Installed()
		if err != nil {
			return err
		}
	}
	if len(installed) == 0 {
		_, err := c.client.PostMessage(
			ctx, c.settings.BotToken, c.settings.ChannelID, "", "No Agents are installed.",
		)
		return err
	}
	var text strings.Builder
	text.WriteString("Installed Agents:")
	for _, installedAgent := range installed {
		fmt.Fprintf(&text, "\n%s — %s (%s)", installedAgent.ID, installedAgent.Name, installedAgent.Type)
	}
	_, err := c.client.PostMessage(ctx, c.settings.BotToken, c.settings.ChannelID, "", text.String())
	return err
}

var _ channeltypes.Channel = (*Channel)(nil)
var _ channeltypes.LifecycleChannel = (*Channel)(nil)
