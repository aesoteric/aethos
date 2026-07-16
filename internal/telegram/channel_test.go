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
	"strconv"
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

func TestTelegramPermissionButtonsRouteFixtureCallbacksAndRemoveKeyboard(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "update the config",
		Permissions: []agent.ScriptedPermission{{
			Request: agent.PermissionRequest{
				ToolCallID: "call-1",
				Title:      "Update config.toml",
				Kind:       "edit",
				Options: []agent.PermissionOption{
					{ID: "allow-once", Name: "Allow once", Kind: agent.PermissionAllowOnce},
					{ID: "reject-once", Name: "Reject once", Kind: agent.PermissionRejectOnce},
				},
			},
			WantOptionID: "allow-once",
		}},
		Events: []agent.Event{agent.Message{Text: "config updated"}},
		Stop:   agent.StopEndTurn,
	}}}
	flow := newTelegramFlow(t, readUpdateFixture(t, "permission_prompt.json"), &script)
	flow.createSession(t, "agent", 303)
	flow.run(t)

	var permissionMessageID int64
	var approveData string
	waitFor(t, func() bool {
		permissionMessageID, approveData = flow.api.permissionButton(303, "Update config.toml", "Approve")
		return permissionMessageID != 0 && approveData != ""
	})
	if _, denyData := flow.api.permissionButton(303, "Update config.toml", "Deny"); denyData == "" {
		t.Fatal("permission request did not include a Deny button")
	}
	if len([]byte(approveData)) > 64 {
		t.Fatalf("Approve callback data is %d bytes, want at most 64", len([]byte(approveData)))
	}

	callbackFixture := string(readUpdateFixture(t, "permission_callback.json"))
	callbackFixture = strings.ReplaceAll(callbackFixture, "{{MESSAGE_ID}}", strconv.FormatInt(permissionMessageID, 10))
	callbackFixture = strings.ReplaceAll(callbackFixture, "{{CALLBACK_DATA}}", approveData)
	flow.api.addFixture([]byte(callbackFixture))

	waitFor(t, func() bool {
		return flow.api.hasCall("editMessageText", func(body map[string]any) bool {
			markup, _ := body["reply_markup"].(map[string]any)
			keyboard, _ := markup["inline_keyboard"].([]any)
			text, _ := body["text"].(string)
			return body["message_id"] == float64(permissionMessageID) &&
				strings.Contains(text, "Approved") && len(keyboard) == 0
		}) && flow.api.count("answerCallbackQuery") == 2 && flow.api.hasVisibleMessage(303, "config updated")
	})
	for _, callbackID := range []string{"approve-1", "approve-2"} {
		if !flow.api.hasCall("answerCallbackQuery", func(body map[string]any) bool {
			return body["callback_query_id"] == callbackID
		}) {
			t.Errorf("callback %q was not acknowledged", callbackID)
		}
	}
}

func TestTelegramPermissionTimeoutRemovesButtonsAndShowsOutcome(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "update the config",
		Permissions: []agent.ScriptedPermission{{
			Request: agent.PermissionRequest{
				ToolCallID: "call-1",
				Title:      "Update config.toml",
				Kind:       "edit",
				Options: []agent.PermissionOption{
					{ID: "allow-once", Name: "Allow once", Kind: agent.PermissionAllowOnce},
					{ID: "reject-once", Name: "Reject once", Kind: agent.PermissionRejectOnce},
				},
			},
			WantOptionID: "reject-once",
		}},
		Events: []agent.Event{agent.Message{Text: "continued after denial"}},
		Stop:   agent.StopEndTurn,
	}}}
	flow := newTelegramFlow(
		t,
		readUpdateFixture(t, "permission_prompt.json"),
		&script,
		session.WithPermissionTimeout(25*time.Millisecond),
	)
	flow.createSession(t, "agent", 303)
	flow.run(t)

	waitFor(t, func() bool {
		return flow.api.hasCall("editMessageText", func(body map[string]any) bool {
			markup, _ := body["reply_markup"].(map[string]any)
			keyboard, _ := markup["inline_keyboard"].([]any)
			text, _ := body["text"].(string)
			return strings.Contains(text, "Timed out") && len(keyboard) == 0
		}) && flow.api.hasVisibleMessage(303, "continued after denial")
	})
	if flow.api.count("answerCallbackQuery") != 0 {
		t.Errorf("timeout answered %d callback queries, want none", flow.api.count("answerCallbackQuery"))
	}
}

func TestTelegramCancelCommandStopsInflightPrompt(t *testing.T) {
	started := make(chan struct{}, 1)
	neverContinue := make(chan struct{})
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "keep working",
		Started:    started,
		Continue:   neverContinue,
		Stop:       agent.StopEndTurn,
	}}}
	flow := newTelegramFlow(t, readUpdateFixture(t, "cancel_prompt.json"), &script)
	record := flow.createSession(t, "agent", 303)
	flow.run(t)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Prompt did not start")
	}
	flow.api.addFixture(readUpdateFixture(t, "cancel_command.json"))

	waitFor(t, func() bool { return flow.api.hasVisibleMessage(303, "Prompt cancelled.") })
	if flow.api.hasVisibleMessage(303, "Prompt failed") {
		t.Error("cancelled Prompt was also reported as failed")
	}
	got, err := flow.manager.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("get Session after cancel: %v", err)
	}
	if got.State != session.Live {
		t.Errorf("Session state after cancel = %q, want %q", got.State, session.Live)
	}
}

func TestTelegramAssistantListsAndClosesSessions(t *testing.T) {
	flow := newTelegramFlow(t, []byte(`{"ok":true,"result":[]}`), &agent.Script{})
	record := flow.createSession(t, "codex-acp", 303)
	record, err := flow.manager.Rename(t.Context(), record.ID, "Fix flaky tests")
	if err != nil {
		t.Fatalf("rename Session: %v", err)
	}
	controls := strings.ReplaceAll(string(readUpdateFixture(t, "session_controls.json")), "{{SESSION_ID}}", record.ID)
	flow.api.addFixture([]byte(controls))
	flow.run(t)

	waitFor(t, func() bool {
		return flow.api.hasCall("sendMessage", func(body map[string]any) bool {
			text, _ := body["text"].(string)
			return body["message_thread_id"] == float64(101) && strings.Contains(text, record.ID) &&
				strings.Contains(text, "Fix flaky tests") && strings.Contains(text, "codex-acp") &&
				strings.Contains(text, "live")
		}) && flow.api.hasCall("sendMessage", func(body map[string]any) bool {
			text, _ := body["text"].(string)
			return body["message_thread_id"] == float64(101) &&
				strings.Contains(text, "Session closed") && strings.Contains(text, "Fix flaky tests")
		}) && flow.api.hasCall("sendMessage", func(body map[string]any) bool {
			text, _ := body["text"].(string)
			return body["message_thread_id"] == float64(101) && strings.Contains(text, record.ID) &&
				strings.Contains(text, "closed")
		})
	})
	closed, err := flow.manager.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("get closed Session: %v", err)
	}
	if closed.State != session.Closed {
		t.Errorf("Session state = %q, want %q", closed.State, session.Closed)
	}
}

type telegramFlow struct {
	api     *telegramFixtureAPI
	bridge  *telegram.Channel
	manager *session.Manager
}

func newTelegramFlow(t *testing.T, fixture []byte, script *agent.Script, options ...session.Option) *telegramFlow {
	t.Helper()
	api := newTelegramFixtureAPI(t, fixture)
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
	t.Cleanup(func() {
		if err := bridge.Close(); err != nil {
			t.Errorf("close Telegram Channel: %v", err)
		}
	})
	manager, err := session.Open(t.Context(), database, scriptedConnector(script), bridge, options...)
	if err != nil {
		t.Fatalf("open Session manager: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close Session manager: %v", err)
		}
	})
	return &telegramFlow{api: api, bridge: bridge, manager: manager}
}

func (f *telegramFlow) createSession(t *testing.T, agentCommand string, topicID int64) session.Record {
	t.Helper()
	record, err := f.manager.Create(t.Context(), session.Create{
		Agent:     agentCommand,
		Workspace: "/workspace",
		Owner:     session.Owner{Channel: "telegram", ID: "123456789"},
		TopicID:   topicID,
	})
	if err != nil {
		t.Fatalf("create Session: %v", err)
	}
	return record
}

func (f *telegramFlow) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.bridge.Run(ctx, f.manager) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("run Telegram Channel: %v", err)
		}
	})
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
	fixtureAdded   chan struct{}
	nextMessageID  int64
	nextSessionTop int64
	messages       map[int64]telegramMessage
}

func newTelegramFixtureAPI(t *testing.T, fixture []byte) *telegramFixtureAPI {
	t.Helper()
	api := &telegramFixtureAPI{
		fixtures:       [][]byte{fixture},
		fixtureAdded:   make(chan struct{}, 1),
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
		for {
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
			select {
			case <-a.fixtureAdded:
				continue
			case <-r.Context().Done():
				return
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func (a *telegramFixtureAPI) addFixture(fixture []byte) {
	a.mu.Lock()
	a.fixtures = append(a.fixtures, fixture)
	a.mu.Unlock()
	select {
	case a.fixtureAdded <- struct{}{}:
	default:
	}
}

func (a *telegramFixtureAPI) permissionButton(topicID int64, text, label string) (int64, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		if call.method != "sendMessage" || call.body["message_thread_id"] != float64(topicID) {
			continue
		}
		messageText, _ := call.body["text"].(string)
		if !strings.Contains(messageText, text) {
			continue
		}
		markup, _ := call.body["reply_markup"].(map[string]any)
		rows, _ := markup["inline_keyboard"].([]any)
		for _, rawRow := range rows {
			row, _ := rawRow.([]any)
			for _, rawButton := range row {
				button, _ := rawButton.(map[string]any)
				if button["text"] == label {
					for messageID, message := range a.messages {
						if message.topicID == topicID && message.text == messageText {
							callbackData, _ := button["callback_data"].(string)
							return messageID, callbackData
						}
					}
				}
			}
		}
	}
	return 0, ""
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
