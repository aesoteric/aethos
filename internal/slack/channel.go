package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aesoteric/aethos/internal/agentcatalog"
	"github.com/coder/websocket"
)

// Settings contains the credentials and access policy needed by the Slack
// Channel at runtime.
type Settings struct {
	AppToken       string
	BotToken       string
	ChannelID      string
	AllowedUserIDs []string
	Agents         agentcatalog.InstalledLister
}

// Option configures Slack Channel timing at external boundaries.
type Option func(*channelOptions) error

type channelOptions struct {
	initialBackoff time.Duration
	maximumBackoff time.Duration
}

// WithReconnectBackoff controls the exponential delay between Socket Mode
// connection attempts.
func WithReconnectBackoff(initial, maximum time.Duration) Option {
	return func(options *channelOptions) error {
		if initial <= 0 || maximum < initial {
			return fmt.Errorf("Slack reconnect backoff must be positive and maximum must not be less than initial")
		}
		options.initialBackoff = initial
		options.maximumBackoff = maximum
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

	mu      sync.Mutex
	running bool
}

// New constructs a Slack Channel.
func New(client *Client, logger *slog.Logger, settings Settings, option ...Option) (*Channel, error) {
	if client == nil {
		return nil, fmt.Errorf("Slack client is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	settings.AppToken = strings.TrimSpace(settings.AppToken)
	settings.BotToken = strings.TrimSpace(settings.BotToken)
	settings.ChannelID = strings.TrimSpace(settings.ChannelID)
	if settings.AppToken == "" {
		return nil, fmt.Errorf("Slack app token is required")
	}
	if settings.BotToken == "" {
		return nil, fmt.Errorf("Slack bot token is required")
	}
	if settings.ChannelID == "" {
		return nil, fmt.Errorf("Slack channel ID is required")
	}
	if len(settings.AllowedUserIDs) == 0 {
		return nil, fmt.Errorf("Slack allowed user IDs are required")
	}
	allowed := make(map[string]struct{}, len(settings.AllowedUserIDs))
	for _, userID := range settings.AllowedUserIDs {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			return nil, fmt.Errorf("Slack allowed user ID cannot be empty")
		}
		allowed[userID] = struct{}{}
	}
	options := channelOptions{initialBackoff: time.Second, maximumBackoff: 30 * time.Second}
	for _, configure := range option {
		if configure == nil {
			return nil, fmt.Errorf("Slack Channel option is required")
		}
		if err := configure(&options); err != nil {
			return nil, err
		}
	}
	return &Channel{client: client, logger: logger, settings: settings, options: options, allowed: allowed}, nil
}

// Run authenticates the bot and receives Socket Mode envelopes until ctx is
// cancelled. Every reconnect obtains a fresh temporary websocket URL.
func (c *Channel) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return fmt.Errorf("Slack Channel is already running")
	}
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	identity, err := c.client.authenticate(ctx, c.settings.BotToken)
	if err != nil {
		return fmt.Errorf("authenticate Slack bot: %w", err)
	}
	c.identity = identity
	backoff := c.options.initialBackoff
	for {
		err := c.runConnection(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			c.logger.Warn("Slack Socket Mode connection ended", "error", err, "retry_in", backoff)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
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
	defer connection.CloseNow()
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
			return fmt.Errorf("Slack requested disconnect: %s", envelope.Reason)
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
	if event.Channel != c.settings.ChannelID || event.ThreadTS != "" || event.User == "" || event.User == c.identity.UserID {
		return nil
	}
	if _, allowed := c.allowed[event.User]; !allowed {
		return nil
	}
	command, _, _ := strings.Cut(strings.TrimSpace(event.Text), " ")
	if command == "agents" {
		return c.sendAgentList(ctx)
	}
	_, err := c.client.PostMessage(
		ctx, c.settings.BotToken, c.settings.ChannelID, "", "Send agents to list installed Agents.",
	)
	return err
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
