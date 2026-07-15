package telegram_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/session"
	"github.com/aesoteric/aethos/internal/telegram"
)

func TestTelegramChannelDrivesSessionFlowThroughFixtureUpdates(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "session_flow.json"))
	if err != nil {
		t.Fatalf("read Telegram update fixture: %v", err)
	}
	chosenWorkspace := t.TempDir()
	fixture = []byte(strings.ReplaceAll(string(fixture), "/chosen/workspace", chosenWorkspace))
	api := newTelegramFixtureAPI(t, fixture)

	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "Investigate flaky tests",
		Events: []agent.Event{
			agent.Thought{Text: "Checking the test history."},
			agent.ToolCallBegan{ID: "call-1", Title: "Read test logs", Kind: "read", Status: "in_progress"},
			agent.Message{Text: "The shared clock is leaking between tests."},
		},
		Stop: agent.StopEndTurn,
	}, {
		WantPrompt: "Do not rename this Session",
		Events:     []agent.Event{agent.Message{Text: "The original name remains."}},
		Stop:       agent.StopEndTurn,
	}}}
	database := filepath.Join(t.TempDir(), "aethos.db")
	var logs lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	bridge, err := telegram.Open(
		t.Context(),
		database,
		telegram.NewClient(api.server.URL, api.server.Client()),
		logger,
		telegram.Settings{
			Token:          "test-token",
			ChatID:         -1001234567890,
			AllowedUserIDs: []int64{123456789},
			DefaultAgent:   "default-agent",
			Workspace:      "/default/workspace",
		},
		telegram.WithWriteInterval(time.Millisecond),
		telegram.WithPollTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("open Telegram Channel: %v", err)
	}
	t.Cleanup(func() {
		if err := bridge.Close(); err != nil {
			t.Errorf("close Telegram Channel: %v", err)
		}
	})
	manager, err := session.Open(t.Context(), database, scriptedConnector(&script), bridge)
	if err != nil {
		t.Fatalf("open Session manager: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close Session manager: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	waitFor(t, func() bool {
		return api.hasCall("editForumTopic", func(body map[string]any) bool {
			return body["message_thread_id"] == float64(202) && body["name"] == "Investigate flaky tests"
		}) && api.hasVisibleMessage(202, "shared clock") &&
			api.hasVisibleMessage(202, "original name remains")
	})

	records, err := manager.List(t.Context())
	if err != nil {
		t.Fatalf("list Sessions: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("Sessions = %#v, want one allowed Session creation", records)
	}
	record := records[0]
	if record.Owner != (session.Owner{Channel: "telegram", ID: "123456789"}) {
		t.Errorf("owner = %#v, want Telegram allowlisted user", record.Owner)
	}
	if record.Agent != "chosen-agent" || record.Workspace != chosenWorkspace {
		t.Errorf("Session selection = agent %q Workspace %q, want command choices", record.Agent, record.Workspace)
	}
	if record.Name != "Investigate flaky tests" || record.TopicID != 202 {
		t.Errorf("Session name/topic = %q/%d, want first Prompt title bound to Topic 202", record.Name, record.TopicID)
	}
	for _, rejectedID := range []string{"999", "998", "997"} {
		if !strings.Contains(logs.String(), `"telegram_user_id":`+rejectedID) {
			t.Errorf("rejected sender %s was not logged with ID: %s", rejectedID, logs.String())
		}
	}
	if !api.hasCall("sendMessage", func(body map[string]any) bool {
		return body["message_thread_id"] == float64(101) && strings.Contains(body["text"].(string), "show me where to start")
	}) {
		t.Error("plain General message was not redirected to Assistant")
	}
	if got := api.countMatching("editForumTopic", func(body map[string]any) bool {
		return body["message_thread_id"] == float64(202)
	}); got != 1 {
		t.Errorf("Session Topic renames = %d, want exactly one from the first Prompt", got)
	}

	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run Telegram Channel: %v", err)
	}
}

func TestTelegramChannelBatchesStreamingEventsIntoMessageEdits(t *testing.T) {
	api := newTelegramFixtureAPI(t, []byte(`{"ok":true,"result":[]}`))
	database := filepath.Join(t.TempDir(), "aethos.db")
	bridge, err := telegram.Open(
		t.Context(),
		database,
		telegram.NewClient(api.server.URL, api.server.Client()),
		slog.New(slog.DiscardHandler),
		telegram.Settings{
			Token:          "test-token",
			ChatID:         -1001234567890,
			AllowedUserIDs: []int64{123456789},
			DefaultAgent:   "agent",
			Workspace:      "/workspace",
		},
		telegram.WithWriteInterval(time.Millisecond),
		telegram.WithPollTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("open Telegram Channel: %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	manager, err := session.Open(t.Context(), database, scriptedConnector(&agent.Script{}), bridge)
	if err != nil {
		t.Fatalf("open Session manager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	record, err := manager.Create(t.Context(), session.Create{
		Agent:     "agent",
		Workspace: "/workspace",
		Owner:     session.Owner{Channel: "telegram", ID: "123456789"},
		TopicID:   303,
	})
	if err != nil {
		t.Fatalf("create Session: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()
	waitFor(t, func() bool { return api.count("getUpdates") > 0 })

	if err := bridge.Send(t.Context(), channel.Event{SessionID: record.ID, AgentEvent: agent.Message{Text: "Hel"}}); err != nil {
		t.Fatalf("send first Agent event: %v", err)
	}
	waitFor(t, func() bool {
		return api.hasCall("sendMessage", func(body map[string]any) bool {
			return body["message_thread_id"] == float64(303) && body["text"] == "Hel"
		})
	})
	if err := bridge.Send(t.Context(), channel.Event{SessionID: record.ID, AgentEvent: agent.Message{Text: "lo"}}); err != nil {
		t.Fatalf("send second Agent event: %v", err)
	}
	waitFor(t, func() bool {
		return api.hasCall("editMessageText", func(body map[string]any) bool {
			return body["text"] == "Hello"
		})
	})
	if !api.hasVisibleMessage(303, "Hello") {
		t.Error("edited Telegram message is not visible in the Session Topic")
	}

	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run Telegram Channel: %v", err)
	}
}

func TestTelegramChannelFindsAssistantAndResumesDormantSessionAfterRestart(t *testing.T) {
	createFixture := readUpdateFixture(t, "create_session.json")
	resumeFixture := readUpdateFixture(t, "resume_session.json")
	api := newTelegramFixtureAPI(t, createFixture)
	database := filepath.Join(t.TempDir(), "aethos.db")
	settings := telegram.Settings{
		Token:          "test-token",
		ChatID:         -1001234567890,
		AllowedUserIDs: []int64{123456789},
		DefaultAgent:   "agent",
		Workspace:      t.TempDir(),
	}
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt:  "continue after restart",
		WantHistory: []string{"continue after restart"},
		Events:      []agent.Event{agent.Message{Text: "resumed without ceremony"}},
		Stop:        agent.StopEndTurn,
	}}}
	connect := scriptedConnector(&script)

	firstBridge, err := telegram.Open(t.Context(), database, telegram.NewClient(api.server.URL, api.server.Client()), slog.New(slog.DiscardHandler), settings,
		telegram.WithWriteInterval(time.Millisecond), telegram.WithPollTimeout(time.Second))
	if err != nil {
		t.Fatalf("open first Telegram Channel: %v", err)
	}
	firstManager, err := session.Open(t.Context(), database, connect, firstBridge)
	if err != nil {
		t.Fatalf("open first Session manager: %v", err)
	}
	firstCtx, firstCancel := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() { firstDone <- firstBridge.Run(firstCtx, firstManager) }()
	waitFor(t, func() bool {
		records, listErr := firstManager.List(t.Context())
		return listErr == nil && len(records) == 1 && records[0].TopicID == 202
	})
	firstCancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("run first Telegram Channel: %v", err)
	}
	if err := firstManager.Close(); err != nil {
		t.Fatalf("close first Session manager: %v", err)
	}
	if err := firstBridge.Close(); err != nil {
		t.Fatalf("close first Telegram Channel: %v", err)
	}

	api.addFixture(resumeFixture)
	secondBridge, err := telegram.Open(t.Context(), database, telegram.NewClient(api.server.URL, api.server.Client()), slog.New(slog.DiscardHandler), settings,
		telegram.WithWriteInterval(time.Millisecond), telegram.WithPollTimeout(time.Second))
	if err != nil {
		t.Fatalf("open second Telegram Channel: %v", err)
	}
	t.Cleanup(func() { _ = secondBridge.Close() })
	secondManager, err := session.Open(t.Context(), database, connect, secondBridge)
	if err != nil {
		t.Fatalf("open second Session manager: %v", err)
	}
	t.Cleanup(func() { _ = secondManager.Close() })
	recovered, err := secondManager.List(t.Context())
	if err != nil || len(recovered) != 1 || recovered[0].State != session.Dormant {
		t.Fatalf("recovered Sessions = %#v, error %v; want one dormant Session", recovered, err)
	}

	secondCtx, secondCancel := context.WithCancel(t.Context())
	secondDone := make(chan error, 1)
	go func() { secondDone <- secondBridge.Run(secondCtx, secondManager) }()
	waitFor(t, func() bool {
		return api.hasCall("sendMessage", func(body map[string]any) bool {
			text, _ := body["text"].(string)
			return body["message_thread_id"] == float64(202) && strings.Contains(text, "resumed without ceremony")
		})
	})
	resumed, err := secondManager.Get(t.Context(), recovered[0].ID)
	if err != nil {
		t.Fatalf("get resumed Session: %v", err)
	}
	if resumed.State != session.Live || resumed.Name != "continue after restart" || resumed.TopicID != 202 {
		t.Errorf("resumed Session = %#v, want live named Session still bound to Topic 202", resumed)
	}
	if got := api.count("createForumTopic"); got != 2 {
		t.Errorf("createForumTopic calls = %d, want Assistant and Session only; Assistant should be found after restart", got)
	}

	secondCancel()
	if err := <-secondDone; err != nil {
		t.Fatalf("run second Telegram Channel: %v", err)
	}
}

func readUpdateFixture(t *testing.T, name string) []byte {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read Telegram update fixture %q: %v", name, err)
	}
	return fixture
}

type telegramCall struct {
	method string
	body   map[string]any
}

type telegramMessage struct {
	topicID int64
	text    string
}

type lockedBuffer struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (b *lockedBuffer) Write(text []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.Write(text)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.String()
}

type telegramFixtureAPI struct {
	server *httptest.Server

	mu             sync.Mutex
	calls          []telegramCall
	fixtures       [][]byte
	nextMessageID  int64
	nextSessionTop int64
	messages       map[int64]telegramMessage
}

func newTelegramFixtureAPI(t *testing.T, fixture []byte) *telegramFixtureAPI {
	t.Helper()
	api := &telegramFixtureAPI{
		fixtures:       [][]byte{fixture},
		nextMessageID:  400,
		nextSessionTop: 202,
		messages:       make(map[int64]telegramMessage),
	}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	t.Cleanup(api.server.Close)
	return api
}

func (a *telegramFixtureAPI) handle(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/bottest-token/")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.calls = append(a.calls, telegramCall{method: method, body: body})
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch method {
	case "getChat":
		fmt.Fprint(w, `{"ok":true,"result":{"id":-1001234567890,"type":"supergroup","title":"Aethos","is_forum":true}}`)
	case "createForumTopic":
		a.mu.Lock()
		topicID := int64(101)
		if body["name"] != "Assistant" {
			topicID = a.nextSessionTop
			a.nextSessionTop++
		}
		a.mu.Unlock()
		fmt.Fprintf(w, `{"ok":true,"result":{"message_thread_id":%d,"name":%q}}`, topicID, body["name"])
	case "sendMessage":
		a.mu.Lock()
		a.nextMessageID++
		messageID := a.nextMessageID
		a.messages[messageID] = telegramMessage{
			topicID: int64(body["message_thread_id"].(float64)),
			text:    body["text"].(string),
		}
		a.mu.Unlock()
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":-1001234567890,"type":"supergroup"},"text":%q}}`, messageID, body["text"])
	case "editMessageText":
		a.mu.Lock()
		messageID := int64(body["message_id"].(float64))
		message := a.messages[messageID]
		message.text = body["text"].(string)
		a.messages[messageID] = message
		a.mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case "editForumTopic", "deleteForumTopic":
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case "getUpdates":
		a.mu.Lock()
		var fixture []byte
		if len(a.fixtures) > 0 {
			fixture = a.fixtures[0]
			a.fixtures = a.fixtures[1:]
		}
		a.mu.Unlock()
		if fixture != nil {
			_, _ = w.Write(fixture)
			return
		}
		<-r.Context().Done()
	default:
		http.NotFound(w, r)
	}
}

func (a *telegramFixtureAPI) addFixture(fixture []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fixtures = append(a.fixtures, fixture)
}

func (a *telegramFixtureAPI) count(method string) int {
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

func (a *telegramFixtureAPI) countMatching(method string, matches func(map[string]any) bool) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	count := 0
	for _, call := range a.calls {
		if call.method == method && matches(call.body) {
			count++
		}
	}
	return count
}

func (a *telegramFixtureAPI) hasCall(method string, matches func(map[string]any) bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		if call.method == method && matches(call.body) {
			return true
		}
	}
	return false
}

func (a *telegramFixtureAPI) hasVisibleMessage(topicID int64, text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, message := range a.messages {
		if message.topicID == topicID && strings.Contains(message.text, text) {
			return true
		}
	}
	return false
}

func scriptedConnector(script *agent.Script) session.Connect {
	return func(ctx context.Context, _ string, handlers agent.Handlers) (*agent.Conn, error) {
		return agent.ConnectScript(ctx, slog.New(slog.DiscardHandler), handlers, script)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition was not met before timeout")
		case <-ticker.C:
		}
	}
}
