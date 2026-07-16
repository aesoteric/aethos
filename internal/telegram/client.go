// Package telegram translates between Telegram's HTTP API and aethos-owned
// types. It is the protocol boundary for the Telegram Channel.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIBaseURL is Telegram's production Bot API endpoint.
const APIBaseURL = "https://api.telegram.org"

// Client calls the subset of the Telegram Bot API used by aethos.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

type user struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
}

type chat struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	IsForum bool   `json:"is_forum"`
}

type message struct {
	MessageID       int64  `json:"message_id"`
	MessageThreadID int64  `json:"message_thread_id"`
	IsTopicMessage  bool   `json:"is_topic_message"`
	From            *user  `json:"from"`
	Chat            chat   `json:"chat"`
	Text            string `json:"text"`
}

type update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *message       `json:"message"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

type callbackQuery struct {
	ID      string   `json:"id"`
	From    *user    `json:"from"`
	Message *message `json:"message"`
	Data    string   `json:"data"`
}

type inlineKeyboardMarkup struct {
	InlineKeyboard [][]inlineKeyboardButton `json:"inline_keyboard"`
}

type inlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type forumTopic struct {
	MessageThreadID int64  `json:"message_thread_id"`
	Name            string `json:"name"`
}

// NewClient returns a Telegram client. Supplying the API base separately keeps
// protocol tests on a local HTTP server while production uses APIBaseURL.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}

// ValidateToken asks Telegram for the bot identity associated with token.
func (c *Client) ValidateToken(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("telegram bot token is required")
	}

	var identity user
	if err := c.call(ctx, token, "getMe", struct{}{}, &identity); err != nil {
		return fmt.Errorf("telegram rejected the bot token: %w", err)
	}
	return nil
}

func (c *Client) getChat(ctx context.Context, token string, chatID int64) (chat, error) {
	var result chat
	err := c.call(ctx, token, "getChat", struct {
		ChatID int64 `json:"chat_id"`
	}{ChatID: chatID}, &result)
	return result, err
}

func (c *Client) getUpdates(ctx context.Context, token string, offset int64, timeout time.Duration) ([]update, error) {
	seconds := int(timeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	var result []update
	err := c.call(ctx, token, "getUpdates", struct {
		Offset         int64    `json:"offset,omitempty"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{Offset: offset, Timeout: seconds, AllowedUpdates: []string{"message", "callback_query"}}, &result)
	return result, err
}

func (c *Client) createForumTopic(ctx context.Context, token string, chatID int64, name string) (forumTopic, error) {
	var result forumTopic
	err := c.call(ctx, token, "createForumTopic", struct {
		ChatID int64  `json:"chat_id"`
		Name   string `json:"name"`
	}{ChatID: chatID, Name: name}, &result)
	return result, err
}

func (c *Client) editForumTopic(ctx context.Context, token string, chatID, topicID int64, name string) error {
	var result bool
	return c.call(ctx, token, "editForumTopic", struct {
		ChatID  int64  `json:"chat_id"`
		TopicID int64  `json:"message_thread_id"`
		Name    string `json:"name"`
	}{ChatID: chatID, TopicID: topicID, Name: name}, &result)
}

func (c *Client) deleteForumTopic(ctx context.Context, token string, chatID, topicID int64) error {
	var result bool
	return c.call(ctx, token, "deleteForumTopic", struct {
		ChatID  int64 `json:"chat_id"`
		TopicID int64 `json:"message_thread_id"`
	}{ChatID: chatID, TopicID: topicID}, &result)
}

func (c *Client) sendMessage(ctx context.Context, token string, chatID, topicID int64, text string) (message, error) {
	return c.sendMessageWithReplyMarkup(ctx, token, chatID, topicID, text, nil)
}

func (c *Client) sendMessageWithReplyMarkup(
	ctx context.Context,
	token string,
	chatID, topicID int64,
	text string,
	replyMarkup *inlineKeyboardMarkup,
) (message, error) {
	var result message
	err := c.call(ctx, token, "sendMessage", struct {
		ChatID              int64                 `json:"chat_id"`
		MessageThreadID     int64                 `json:"message_thread_id,omitempty"`
		Text                string                `json:"text"`
		DisableNotification bool                  `json:"disable_notification,omitempty"`
		ReplyMarkup         *inlineKeyboardMarkup `json:"reply_markup,omitempty"`
	}{
		ChatID:              chatID,
		MessageThreadID:     topicID,
		Text:                text,
		DisableNotification: true,
		ReplyMarkup:         replyMarkup,
	}, &result)
	return result, err
}

func (c *Client) editMessageText(ctx context.Context, token string, chatID, messageID int64, text string) error {
	return c.editMessageTextWithReplyMarkup(ctx, token, chatID, messageID, text, nil)
}

func (c *Client) editMessageTextWithReplyMarkup(
	ctx context.Context,
	token string,
	chatID, messageID int64,
	text string,
	replyMarkup *inlineKeyboardMarkup,
) error {
	var result any
	return c.call(ctx, token, "editMessageText", struct {
		ChatID      int64                 `json:"chat_id"`
		MessageID   int64                 `json:"message_id"`
		Text        string                `json:"text"`
		ReplyMarkup *inlineKeyboardMarkup `json:"reply_markup,omitempty"`
	}{ChatID: chatID, MessageID: messageID, Text: text, ReplyMarkup: replyMarkup}, &result)
}

func (c *Client) answerCallbackQuery(ctx context.Context, token, callbackQueryID, text string) error {
	var result bool
	return c.call(ctx, token, "answerCallbackQuery", struct {
		CallbackQueryID string `json:"callback_query_id"`
		Text            string `json:"text,omitempty"`
	}{CallbackQueryID: callbackQueryID, Text: text}, &result)
}

func (c *Client) call(ctx context.Context, token, method string, parameters, result any) error {
	body, err := json.Marshal(parameters)
	if err != nil {
		return fmt.Errorf("encode Telegram %s request: %w", method, err)
	}
	endpoint := c.baseURL + "/bot" + url.PathEscape(token) + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Telegram %s request: %s", method, redact(err.Error(), token))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call Telegram %s: %s", method, redact(err.Error(), token))
	}
	defer resp.Body.Close()

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("telegram %s returned invalid JSON (HTTP %d)", method, resp.StatusCode)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || !envelope.OK {
		reason := strings.TrimSpace(envelope.Description)
		if reason == "" {
			reason = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("telegram %s failed: %s", method, reason)
	}
	if result == nil || len(envelope.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("telegram %s returned an invalid result: %w", method, err)
	}
	return nil
}

func redact(message, token string) string {
	message = strings.ReplaceAll(message, token, "[REDACTED]")
	return strings.ReplaceAll(message, url.PathEscape(token), "[REDACTED]")
}
