package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
)

const (
	permissionCallbackPrefix     = "permission:"
	completedPermissionRetention = 10 * time.Minute
)

type permissionMessage struct {
	messageID   int64
	topicID     int64
	title       string
	options     []agent.PermissionOption
	completedAt time.Time
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
	if err := sessions.ResolvePermission(ctx, channel.PermissionResponse{
		RequestID: requestID,
		OptionID:  optionID,
	}); err != nil {
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
	for id, permission := range c.permissions {
		if !permission.completedAt.IsZero() && time.Since(permission.completedAt) >= completedPermissionRetention {
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

type permissionKindPresentation struct {
	buttonLabel string
	outcome     string
}

func permissionPresentation(kind agent.PermissionOptionKind) permissionKindPresentation {
	switch kind {
	case agent.PermissionAllowOnce:
		return permissionKindPresentation{buttonLabel: "Approve", outcome: "Approved"}
	case agent.PermissionAllowAlways:
		return permissionKindPresentation{buttonLabel: "Always approve", outcome: "Approved"}
	case agent.PermissionRejectOnce:
		return permissionKindPresentation{buttonLabel: "Deny", outcome: "Denied"}
	case agent.PermissionRejectAlways:
		return permissionKindPresentation{buttonLabel: "Always deny", outcome: "Denied"}
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
