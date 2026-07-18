// Package slack translates between Slack's Web API and Socket Mode protocol
// and aethos-owned types. It is the protocol boundary for the Slack Channel.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIBaseURL is Slack's production Web API endpoint.
const APIBaseURL = "https://slack.com/api"

// Client calls the subset of the Slack Web API used by aethos.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

type authIdentity struct {
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
}

// Message identifies one Slack message returned by the Web API.
type Message struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// NewClient returns a Slack client. Supplying the API base separately keeps
// protocol tests on a local HTTP server while production uses APIBaseURL.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}

func (c *Client) authenticate(ctx context.Context, botToken string) (authIdentity, error) {
	var identity authIdentity
	err := c.call(ctx, botToken, "auth.test", struct{}{}, &identity)
	return identity, err
}

func (c *Client) openSocketURL(ctx context.Context, appToken string) (string, error) {
	var result struct {
		URL string `json:"url"`
	}
	if err := c.call(ctx, appToken, "apps.connections.open", struct{}{}, &result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.URL) == "" {
		return "", fmt.Errorf("slack apps.connections.open returned an empty websocket URL")
	}
	return result.URL, nil
}

// PostMessage posts a top-level message or thread reply.
func (c *Client) PostMessage(ctx context.Context, botToken, channelID, threadTS, text string) (Message, error) {
	var posted Message
	err := c.call(ctx, botToken, "chat.postMessage", struct {
		Channel  string `json:"channel"`
		Text     string `json:"text"`
		ThreadTS string `json:"thread_ts,omitempty"`
	}{Channel: channelID, Text: text, ThreadTS: threadTS}, &posted)
	return posted, err
}

// UpdateMessage replaces the text of a message posted by the authenticated bot.
func (c *Client) UpdateMessage(ctx context.Context, botToken, channelID, timestamp, text string) error {
	var updated Message
	return c.call(ctx, botToken, "chat.update", struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Text    string `json:"text"`
	}{Channel: channelID, TS: timestamp, Text: text}, &updated)
}

func (c *Client) call(ctx context.Context, token, method string, parameters, result any) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("slack %s token is required", method)
	}
	body, err := json.Marshal(parameters)
	if err != nil {
		return fmt.Errorf("encode Slack %s request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Slack %s request: %s", method, redact(err.Error(), token))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call Slack %s: %s", method, redact(err.Error(), token))
	}
	defer resp.Body.Close()

	contents, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read Slack %s response: %w", method, err)
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(contents, &status); err != nil {
		return fmt.Errorf("slack %s returned invalid JSON (HTTP %d)", method, resp.StatusCode)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || !status.OK {
		reason := strings.TrimSpace(status.Error)
		if reason == "" {
			reason = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("slack %s failed: %s", method, reason)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(contents, result); err != nil {
		return fmt.Errorf("slack %s returned an invalid result: %w", method, err)
	}
	return nil
}

func redact(message, token string) string {
	return strings.ReplaceAll(message, token, "[REDACTED]")
}
