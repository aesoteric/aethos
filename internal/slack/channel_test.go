package slack_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/agentcatalog"
	"github.com/aesoteric/aethos/internal/slack"
	"github.com/coder/websocket"
)

func TestSocketModeAcknowledgesEnvelopesAndReconnectsAfterDrop(t *testing.T) {
	api := newSlackFixtureAPI(t,
		socketScript{
			envelopes: []string{
				`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
				messageEnvelope("envelope-1", "U9999999999", "C0123456789", "ignored", ""),
			},
			drop: true,
		},
		socketScript{
			envelopes: []string{`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`},
		},
	)
	bridge := newSlackChannel(t, api, slack.WithReconnectBackoff(25*time.Millisecond, 25*time.Millisecond))
	manager := openSlackSessionManager(t, bridge, &agent.Script{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	waitFor(t, func() bool {
		return api.hasAcknowledgement("envelope-1") && api.connectionCount() >= 2
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run Slack Channel: %v", err)
	}
	if got := api.callCount("auth.test"); got != 1 {
		t.Errorf("auth.test calls = %d, want one startup identity check", got)
	}
	if !api.hasCall("auth.test", "Bearer xoxb-test-token", map[string]any{}) {
		t.Error("auth.test did not receive the configured bot token in the Authorization header")
	}
	if got := api.callCount("apps.connections.open"); got < 2 {
		t.Errorf("apps.connections.open calls = %d, want a fresh URL after the drop", got)
	}
	if !api.hasCall("apps.connections.open", "Bearer xapp-test-token", map[string]any{}) {
		t.Error("apps.connections.open did not receive the configured app token in the Authorization header")
	}
	connectionOpens := api.callTimes("apps.connections.open")
	if len(connectionOpens) >= 2 && connectionOpens[1].Sub(connectionOpens[0]) < 20*time.Millisecond {
		t.Errorf("Socket Mode reconnected after %s, want the configured backoff", connectionOpens[1].Sub(connectionOpens[0]))
	}
}

func TestAssistantListsInstalledAgentsAndRepliesWithUsage(t *testing.T) {
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("agents-envelope", "U0123456789", "C0123456789", "agents", ""),
		messageEnvelope("usage-envelope", "U0123456789", "C0123456789", "what now", ""),
	}})
	bridge := newSlackChannel(t, api)
	manager := openSlackSessionManager(t, bridge, &agent.Script{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	waitFor(t, func() bool {
		return api.hasPost("C0123456789", "Installed Agents:\ncodex-acp — Codex (npx)\ngoose — goose (binary)") &&
			api.hasPost("C0123456789", "Send agents to list installed Agents.")
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run Slack Channel: %v", err)
	}
	if got := api.callCount("chat.postMessage"); got != 2 {
		t.Errorf("chat.postMessage calls = %d, want one Agent list and one usage reply", got)
	}
}

func TestSlackChannelSilentlyIgnoresRejectedMessages(t *testing.T) {
	botEnvelope := map[string]any{
		"type":        "events_api",
		"envelope_id": "other-bot-envelope",
		"payload": map[string]any{
			"type": "event_callback",
			"event": map[string]any{
				"type": "message", "subtype": "bot_message", "bot_id": "B9999999999",
				"channel": "C0123456789", "text": "agents", "ts": "1750000000.000002",
			},
		},
	}
	encodedBotEnvelope, err := json.Marshal(botEnvelope)
	if err != nil {
		t.Fatalf("encode bot envelope: %v", err)
	}
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("non-allowlisted-envelope", "U9999999999", "C0123456789", "agents", ""),
		string(encodedBotEnvelope),
		messageEnvelope("aethos-envelope", "U0AETHOS000", "C0123456789", "agents", ""),
		messageEnvelope("foreign-channel-envelope", "U0123456789", "C9999999999", "agents", ""),
		messageEnvelope("thread-envelope", "U0123456789", "C0123456789", "agents", "1750000000.000003"),
	}})
	bridge := newSlackChannel(t, api)
	manager := openSlackSessionManager(t, bridge, &agent.Script{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	waitFor(t, func() bool {
		for _, envelopeID := range []string{
			"non-allowlisted-envelope", "other-bot-envelope", "aethos-envelope",
			"foreign-channel-envelope", "thread-envelope",
		} {
			if !api.hasAcknowledgement(envelopeID) {
				return false
			}
		}
		return true
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run Slack Channel: %v", err)
	}
	if got := api.callCount("chat.postMessage"); got != 0 {
		t.Errorf("chat.postMessage calls = %d, want rejected messages ignored silently", got)
	}
}

func TestSocketModeReconnectsAfterSlackRefreshRequest(t *testing.T) {
	api := newSlackFixtureAPI(t,
		socketScript{envelopes: []string{
			`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
			`{"type":"disconnect","reason":"refresh_requested"}`,
		}},
		socketScript{envelopes: []string{
			`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
			messageEnvelope("after-refresh-envelope", "U0123456789", "C0123456789", "agents", ""),
		}},
	)
	bridge := newSlackChannel(t, api, slack.WithReconnectBackoff(25*time.Millisecond, 25*time.Millisecond))
	manager := openSlackSessionManager(t, bridge, &agent.Script{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	waitFor(t, func() bool {
		return api.connectionCount() >= 2 && api.hasAcknowledgement("after-refresh-envelope") &&
			api.hasPost("C0123456789", "Installed Agents:\ncodex-acp — Codex (npx)\ngoose — goose (binary)")
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run Slack Channel: %v", err)
	}
	connectionOpens := api.callTimes("apps.connections.open")
	if len(connectionOpens) < 2 {
		t.Fatalf("apps.connections.open calls = %d, want a fresh URL after Slack's refresh request", len(connectionOpens))
	}
	if elapsed := connectionOpens[1].Sub(connectionOpens[0]); elapsed < 20*time.Millisecond {
		t.Errorf("Socket Mode refreshed after %s, want the configured backoff", elapsed)
	}
}

func newSlackChannel(t *testing.T, api *slackFixtureAPI, options ...slack.Option) *slack.Channel {
	t.Helper()
	return newSlackChannelWithDefaults(t, api, "/workspace", options...)
}

func newSlackChannelWithDefaults(
	t *testing.T,
	api *slackFixtureAPI,
	workspace string,
	options ...slack.Option,
) *slack.Channel {
	t.Helper()
	if len(options) == 0 {
		options = []slack.Option{slack.WithReconnectBackoff(time.Millisecond, 5*time.Millisecond)}
	}
	bridge, err := slack.New(
		slack.NewClient(api.server.URL+"/api", api.server.Client()),
		slog.New(slog.DiscardHandler),
		slack.Settings{
			AppToken:       "xapp-test-token",
			BotToken:       "xoxb-test-token",
			ChannelID:      "C0123456789",
			AllowedUserIDs: []string{"U0123456789"},
			DefaultAgent:   "codex-acp",
			Workspace:      workspace,
			Agents: staticAgentCatalog{
				{ID: "codex-acp", Name: "Codex", Type: agentcatalog.NPX},
				{ID: "goose", Name: "goose", Type: agentcatalog.Binary},
			},
		},
		options...,
	)
	if err != nil {
		t.Fatalf("new Slack Channel: %v", err)
	}
	return bridge
}

type staticAgentCatalog []agentcatalog.InstalledAgent

func (c staticAgentCatalog) Installed() ([]agentcatalog.InstalledAgent, error) {
	return append([]agentcatalog.InstalledAgent(nil), c...), nil
}

func messageEnvelope(envelopeID, userID, channelID, text, threadTS string) string {
	event := map[string]any{
		"type":         "message",
		"user":         userID,
		"channel":      channelID,
		"text":         text,
		"ts":           "1750000000.000001",
		"channel_type": "channel",
	}
	if threadTS != "" {
		event["thread_ts"] = threadTS
	}
	envelope := map[string]any{
		"type":        "events_api",
		"envelope_id": envelopeID,
		"payload": map[string]any{
			"type":  "event_callback",
			"event": event,
		},
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

type socketScript struct {
	envelopes []string
	drop      bool
}

type slackCall struct {
	method        string
	authorization string
	body          map[string]any
	responseTS    string
	at            time.Time
}

type slackFixtureAPI struct {
	server *httptest.Server

	mu               sync.Mutex
	scripts          []socketScript
	calls            []slackCall
	acknowledgements []string
	connections      int
	nextMessage      int
}

func newSlackFixtureAPI(t *testing.T, scripts ...socketScript) *slackFixtureAPI {
	t.Helper()
	api := &slackFixtureAPI{scripts: append([]socketScript(nil), scripts...)}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	t.Cleanup(api.server.Close)
	return api
}

func (a *slackFixtureAPI) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/socket" {
		a.handleSocket(w, r)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	responseTS := ""
	if method == "chat.postMessage" {
		a.nextMessage++
		responseTS = fmt.Sprintf("1750000001.%06d", a.nextMessage)
	}
	a.calls = append(a.calls, slackCall{
		method: method, authorization: r.Header.Get("Authorization"), body: body, responseTS: responseTS, at: time.Now(),
	})
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch method {
	case "auth.test":
		fmt.Fprint(w, `{"ok":true,"team_id":"T0123456789","user_id":"U0AETHOS000","bot_id":"B0AETHOS000"}`)
	case "apps.connections.open":
		fmt.Fprintf(w, `{"ok":true,"url":%q}`, strings.Replace(a.server.URL, "http://", "ws://", 1)+"/socket")
	case "chat.postMessage":
		fmt.Fprintf(w, `{"ok":true,"channel":"C0123456789","ts":%q}`, responseTS)
	case "chat.update":
		fmt.Fprint(w, `{"ok":true,"channel":"C0123456789","ts":"1750000001.000001"}`)
	default:
		http.NotFound(w, r)
	}
}

func (a *slackFixtureAPI) handleSocket(w http.ResponseWriter, r *http.Request) {
	connection, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = connection.CloseNow() }()
	a.mu.Lock()
	a.connections++
	var script socketScript
	if len(a.scripts) > 0 {
		script = a.scripts[0]
		a.scripts = a.scripts[1:]
	}
	a.mu.Unlock()

	for _, envelope := range script.envelopes {
		if err := connection.Write(r.Context(), websocket.MessageText, []byte(envelope)); err != nil {
			return
		}
		var decoded struct {
			EnvelopeID string `json:"envelope_id"`
		}
		if json.Unmarshal([]byte(envelope), &decoded) != nil || decoded.EnvelopeID == "" {
			continue
		}
		_, acknowledgement, err := connection.Read(r.Context())
		if err != nil {
			return
		}
		var ack struct {
			EnvelopeID string `json:"envelope_id"`
		}
		if json.Unmarshal(acknowledgement, &ack) != nil {
			return
		}
		a.mu.Lock()
		a.acknowledgements = append(a.acknowledgements, ack.EnvelopeID)
		a.mu.Unlock()
	}
	if script.drop {
		_ = connection.CloseNow()
		return
	}
	_, _, _ = connection.Read(r.Context())
}

func (a *slackFixtureAPI) hasAcknowledgement(envelopeID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, acknowledgement := range a.acknowledgements {
		if acknowledgement == envelopeID {
			return true
		}
	}
	return false
}

func (a *slackFixtureAPI) connectionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connections
}

func (a *slackFixtureAPI) callCount(method string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	count := 0
	for _, call := range a.calls {
		if call.method == method {
			count++
		}
	}
	return count
}

func (a *slackFixtureAPI) callTimes(method string) []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	var times []time.Time
	for _, call := range a.calls {
		if call.method == method {
			times = append(times, call.at)
		}
	}
	return times
}

func (a *slackFixtureAPI) hasPost(channelID, text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		if call.method == "chat.postMessage" && call.body["channel"] == channelID && call.body["text"] == text {
			if threadTS, present := call.body["thread_ts"]; !present || threadTS == "" {
				return true
			}
		}
	}
	return false
}

func (a *slackFixtureAPI) hasPostContaining(channelID, text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		bodyText, _ := call.body["text"].(string)
		if call.method == "chat.postMessage" && call.body["channel"] == channelID && strings.Contains(bodyText, text) {
			if threadTS, present := call.body["thread_ts"]; !present || threadTS == "" {
				return true
			}
		}
	}
	return false
}

func (a *slackFixtureAPI) hasThreadPost(threadTS, text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		if call.method == "chat.postMessage" && call.body["thread_ts"] == threadTS && call.body["text"] == text {
			return true
		}
	}
	return false
}

func (a *slackFixtureAPI) hasThreadPostContaining(threadTS, text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		bodyText, _ := call.body["text"].(string)
		if call.method == "chat.postMessage" && call.body["thread_ts"] == threadTS && strings.Contains(bodyText, text) {
			return true
		}
	}
	return false
}

func (a *slackFixtureAPI) hasUpdate(timestamp, text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		if call.method == "chat.update" && call.body["ts"] == timestamp && call.body["text"] == text {
			return true
		}
	}
	return false
}

func (a *slackFixtureAPI) threadPostTimestamp(threadTS, text string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		if call.method == "chat.postMessage" && call.body["thread_ts"] == threadTS && call.body["text"] == text {
			return call.responseTS
		}
	}
	return ""
}

func (a *slackFixtureAPI) updatePrecedesThreadPost(
	timestamp, updateText, threadTS, postText string,
) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	updateIndex := -1
	postIndex := -1
	for index, call := range a.calls {
		if call.method == "chat.update" && call.body["ts"] == timestamp && call.body["text"] == updateText {
			updateIndex = index
		}
		if call.method == "chat.postMessage" && call.body["thread_ts"] == threadTS && call.body["text"] == postText {
			postIndex = index
		}
	}
	return updateIndex >= 0 && postIndex > updateIndex
}

func (a *slackFixtureAPI) hasCall(method, authorization string, body map[string]any) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		if call.method != method || call.authorization != authorization || len(call.body) != len(body) {
			continue
		}
		matches := true
		for key, value := range body {
			if call.body[key] != value {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for Slack flow")
}
