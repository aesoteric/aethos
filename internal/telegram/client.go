// Package telegram translates between Telegram's HTTP API and aethos-owned
// types. It is the protocol boundary for the Telegram Channel.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// APIBaseURL is Telegram's production Bot API endpoint.
const APIBaseURL = "https://api.telegram.org"

// Client calls the subset of the Telegram Bot API used by aethos.
type Client struct {
	baseURL    string
	httpClient *http.Client
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

	endpoint := c.baseURL + "/bot" + url.PathEscape(token) + "/getMe"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create Telegram token validation request: %s", redact(err.Error(), token))
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("contact Telegram to validate bot token: %s", redact(err.Error(), token))
	}
	defer resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("telegram returned an invalid token-validation response (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || !result.OK {
		reason := strings.TrimSpace(result.Description)
		if reason == "" {
			reason = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("telegram rejected the bot token: %s", reason)
	}
	return nil
}

func redact(message, token string) string {
	message = strings.ReplaceAll(message, token, "[REDACTED]")
	return strings.ReplaceAll(message, url.PathEscape(token), "[REDACTED]")
}
