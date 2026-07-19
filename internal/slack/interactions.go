package slack

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	channeltypes "github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/session"
)

const (
	cancelPromptActionID         = "cancel_prompt"
	resolvePermissionActionID    = "resolve_permission"
	completedPermissionRetention = 10 * time.Minute
)

type permissionMessage struct {
	messageTS   string
	threadTS    string
	text        string
	options     []agent.PermissionOption
	values      []string
	answering   bool
	completedAt time.Time
}

type blockActionPayload struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Actions []struct {
		Type     string `json:"type"`
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

func (c *Channel) handleInteraction(ctx context.Context, payload json.RawMessage) error {
	var interaction blockActionPayload
	if err := json.Unmarshal(payload, &interaction); err != nil {
		return err
	}
	if interaction.Type != "block_actions" || interaction.Channel.ID != c.settings.ChannelID {
		return nil
	}
	if _, allowed := c.allowed[interaction.User.ID]; !allowed {
		return nil
	}
	for _, action := range interaction.Actions {
		if action.Type != "button" {
			continue
		}
		switch action.ActionID {
		case cancelPromptActionID:
			return c.cancelPrompt(ctx, interaction.Message.ThreadTS, action.Value)
		case resolvePermissionActionID:
			return c.resolvePermission(
				ctx,
				interaction.Message.TS,
				interaction.Message.ThreadTS,
				action.Value,
			)
		}
	}
	return nil
}

func (c *Channel) resolvePermission(ctx context.Context, messageTS, threadTS, controlValue string) error {
	c.mu.Lock()
	var requestID, optionID string
	var pending *permissionMessage
	for id, permission := range c.permissions {
		if permission.threadTS != threadTS || permission.answering || !permission.completedAt.IsZero() {
			continue
		}
		if permission.messageTS != "" && permission.messageTS != messageTS {
			continue
		}
		for index, value := range permission.values {
			if value == controlValue {
				requestID = id
				optionID = permission.options[index].ID
				pending = permission
				permission.answering = true
				break
			}
		}
		if pending != nil {
			break
		}
	}
	sessions := c.sessions
	c.mu.Unlock()
	if pending == nil || sessions == nil {
		return nil
	}
	if err := sessions.ResolvePermission(ctx, channeltypes.PermissionResponse{
		RequestID: requestID,
		OptionID:  optionID,
	}); err != nil {
		c.mu.Lock()
		if current := c.permissions[requestID]; current == pending {
			current.answering = false
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *Channel) presentPermission(
	ctx context.Context,
	threadTS string,
	request agent.PermissionRequested,
) error {
	if len(request.Options) == 0 {
		return fmt.Errorf("permission request %q offered no response options", request.ID)
	}
	text := permissionRequestText(request.Title, request.Kind)

	c.mu.Lock()
	for id, permission := range c.permissions {
		if !permission.completedAt.IsZero() && time.Since(permission.completedAt) >= completedPermissionRetention {
			delete(c.permissions, id)
		}
	}
	c.nextControlID++
	controlID := c.nextControlID
	values := make([]string, len(request.Options))
	buttons := make([]blockElement, 0, len(request.Options))
	for index, option := range request.Options {
		value := fmt.Sprintf("permission:%d:%d", controlID, index)
		values[index] = value
		presentation := permissionPresentation(option.Kind)
		buttons = append(buttons, button(
			permissionButtonLabel(option),
			resolvePermissionActionID,
			value,
			presentation.style,
		))
	}
	pending := &permissionMessage{
		threadTS: threadTS,
		text:     text,
		options:  append([]agent.PermissionOption(nil), request.Options...),
		values:   values,
	}
	c.permissions[request.ID] = pending
	c.mu.Unlock()

	blocks := interactiveMessageBlocks(text, buttons...)
	posted, err := c.client.postMessage(
		ctx,
		c.settings.BotToken,
		c.settings.ChannelID,
		threadTS,
		text,
		&blocks,
	)
	if err != nil {
		c.mu.Lock()
		if c.permissions[request.ID] == pending {
			delete(c.permissions, request.ID)
		}
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	if c.permissions[request.ID] == pending {
		pending.messageTS = posted.TS
	}
	c.mu.Unlock()
	return nil
}

func (c *Channel) finishPermission(ctx context.Context, result agent.PermissionResolved) error {
	c.mu.Lock()
	pending := c.permissions[result.ID]
	if pending == nil || !pending.completedAt.IsZero() {
		c.mu.Unlock()
		return nil
	}
	pending.completedAt = time.Now()
	pending.answering = false
	messageTS := pending.messageTS
	text := pending.text + "\nOutcome: " + permissionOutcome(result, pending.options)
	c.mu.Unlock()
	if messageTS == "" {
		return fmt.Errorf("permission request %q has no Slack message timestamp", result.ID)
	}
	emptyBlocks := []messageBlock{}
	return c.client.updateMessage(
		ctx,
		c.settings.BotToken,
		c.settings.ChannelID,
		messageTS,
		text,
		&emptyBlocks,
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

type permissionKindPresentation struct {
	buttonLabel string
	outcome     string
	style       string
}

func permissionPresentation(kind agent.PermissionOptionKind) permissionKindPresentation {
	switch kind {
	case agent.PermissionAllowOnce:
		return permissionKindPresentation{buttonLabel: "Approve", outcome: "Approved", style: "primary"}
	case agent.PermissionAllowAlways:
		return permissionKindPresentation{buttonLabel: "Always approve", outcome: "Approved", style: "primary"}
	case agent.PermissionRejectOnce:
		return permissionKindPresentation{buttonLabel: "Deny", outcome: "Denied", style: "danger"}
	case agent.PermissionRejectAlways:
		return permissionKindPresentation{buttonLabel: "Always deny", outcome: "Denied", style: "danger"}
	default:
		return permissionKindPresentation{}
	}
}

func permissionButtonLabel(option agent.PermissionOption) string {
	if label := permissionPresentation(option.Kind).buttonLabel; label != "" {
		return label
	}
	if name := strings.TrimSpace(option.Name); name != "" {
		return name
	}
	return "Respond"
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
		if outcome := permissionPresentation(option.Kind).outcome; outcome != "" {
			return outcome
		}
		if name := strings.TrimSpace(option.Name); name != "" {
			return name
		}
	}
	return "Resolved"
}

func (c *Channel) cancelPrompt(ctx context.Context, threadTS, controlValue string) error {
	c.mu.Lock()
	var destination sessionThread
	for _, draft := range c.drafts {
		if draft.cancelValue == controlValue && draft.destination.threadTS == threadTS {
			destination = draft.destination
			break
		}
	}
	sessions := c.sessions
	c.mu.Unlock()
	if destination.sessionID == "" || sessions == nil {
		return nil
	}
	if err := sessions.Cancel(ctx, destination.sessionID); err != nil {
		if errors.Is(err, session.ErrNoPrompt) {
			_, sendErr := c.client.PostMessage(
				ctx,
				c.settings.BotToken,
				c.settings.ChannelID,
				destination.threadTS,
				"No Prompt is currently running.",
			)
			return sendErr
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	_, err := c.client.PostMessage(
		ctx,
		c.settings.BotToken,
		c.settings.ChannelID,
		destination.threadTS,
		"Prompt cancelled.",
	)
	return err
}
