package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/agentcatalog"
	"github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/session"
	"github.com/aesoteric/aethos/internal/telegram"
)

func TestAgentsCommandListsAndInstallsRegistryAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
  "version": "1.0.0",
  "agents": [{
    "id": "codex-acp",
    "name": "Codex",
    "version": "1.1.4",
    "description": "ACP integration for Codex",
    "distribution": {"npx": {"package": "@agentclientprotocol/codex-acp@1.1.4"}}
  }]
}`)
	}))
	t.Cleanup(server.Close)
	registry := agentcatalog.NewRegistry(server.URL, server.Client())
	dataDir := t.TempDir()
	logger := slog.New(slog.DiscardHandler)

	var listOutput strings.Builder
	if err := runWithRegistry(
		t.Context(), logger, []string{"agents", "-data-dir", dataDir},
		strings.NewReader(""), &listOutput, io.Discard, nil, registry,
	); err != nil {
		t.Fatalf("list registry Agents: %v", err)
	}
	for _, want := range []string{"codex-acp", "Codex", "npx", "ACP integration for Codex"} {
		if !strings.Contains(listOutput.String(), want) {
			t.Errorf("agents output = %q, want %q", listOutput.String(), want)
		}
	}

	var installOutput strings.Builder
	if err := runWithRegistry(
		t.Context(), logger, []string{"agents", "install", "-data-dir", dataDir, "codex-acp"},
		strings.NewReader(""), &installOutput, io.Discard, nil, registry,
	); err != nil {
		t.Fatalf("install registry Agent: %v", err)
	}
	if !strings.Contains(installOutput.String(), "Installed Codex (codex-acp)") {
		t.Errorf("install output = %q, want installed Agent confirmation", installOutput.String())
	}
	reopened, err := agentcatalog.Open(filepath.Join(dataDir, "agents.json"), filepath.Join(dataDir, "agents"))
	if err != nil {
		t.Fatalf("open installed catalog: %v", err)
	}
	if _, err := reopened.Resolve("codex-acp"); err != nil {
		t.Fatalf("installed Agent is not resolvable: %v", err)
	}
}

func TestSessionSpawnsInstalledAgentFromCatalogEntry(t *testing.T) {
	dataDir := t.TempDir()
	catalog, err := agentcatalog.Open(filepath.Join(dataDir, "agents.json"), filepath.Join(dataDir, "agents"))
	if err != nil {
		t.Fatalf("Open Agent catalog: %v", err)
	}
	_, err = catalog.Install(t.Context(), agentcatalog.RegistryAgent{
		ID:      "codex-acp",
		Name:    "Codex",
		Version: "1.1.4",
		Distribution: agentcatalog.Distribution{NPX: &agentcatalog.PackageDistribution{
			Package: "@agentclientprotocol/codex-acp@1.1.4",
			Args:    []string{"--stdio"},
			Env:     map[string]string{"CODEX_MODE": "acp"},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("install catalog Agent: %v", err)
	}

	spawned := false
	connect := agentConnectorWithSpawner(
		slog.New(slog.DiscardHandler),
		catalog,
		func(ctx context.Context, _ *slog.Logger, handlers agent.Handlers, command string, args []string, env map[string]string) (*agent.Conn, error) {
			spawned = true
			if command != "npx" {
				t.Errorf("spawn command = %q, want npx", command)
			}
			if want := []string{"--yes", "@agentclientprotocol/codex-acp@1.1.4", "--stdio"}; !slices.Equal(args, want) {
				t.Errorf("spawn args = %q, want %q", args, want)
			}
			if env["CODEX_MODE"] != "acp" {
				t.Errorf("spawn env = %q, want CODEX_MODE", env)
			}
			return agent.ConnectScript(ctx, slog.New(slog.DiscardHandler), handlers, &agent.Script{})
		},
	)
	manager, err := session.Open(
		t.Context(), filepath.Join(dataDir, "sessions.db"), connect, &channel.Memory{},
	)
	if err != nil {
		t.Fatalf("open Session manager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	record, err := manager.Create(t.Context(), session.Create{
		Agent:     "codex-acp",
		Workspace: t.TempDir(),
		Owner:     session.Owner{Channel: "test", ID: "developer"},
	})
	if err != nil {
		t.Fatalf("create Session: %v", err)
	}
	if !spawned || record.Agent != "codex-acp" {
		t.Errorf("created Session = %#v, spawned = %t", record, spawned)
	}
}

func TestRunRejectsBadInvocationsWithUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown command", args: []string{"frobnicate"}},
		{name: "dev prompt without agent", args: []string{"dev", "prompt", "hello"}},
		{name: "dev prompt with whitespace-only agent", args: []string{"dev", "prompt", "-agent", "   ", "hello"}},
		{name: "dev prompt without prompt text", args: []string{"dev", "prompt", "-agent", "some-agent"}},
	}

	logger := slog.New(slog.DiscardHandler)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr strings.Builder
			err := run(t.Context(), logger, tt.args, strings.NewReader(""), io.Discard, &stderr, nil)
			if !errors.Is(err, errUsage) {
				t.Errorf("run(%q) = %v, want errUsage", tt.args, err)
			}
			if stderr.String() == "" {
				t.Error("no usage text written to stderr")
			}
		})
	}
}

func TestDevPromptPersistsAndResumesSession(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{
		{
			WantPrompt:  "remember blue",
			WantHistory: []string{"remember blue"},
			Events:      []agent.Event{agent.Message{Text: "remembered"}},
			Stop:        agent.StopEndTurn,
		},
		{
			WantPrompt:  "what was it?",
			WantHistory: []string{"remember blue", "what was it?"},
			Events:      []agent.Event{agent.Message{Text: "blue"}},
			Stop:        agent.StopEndTurn,
		},
	}}
	connect := func(ctx context.Context, _ string, handlers agent.Handlers) (*agent.Conn, error) {
		return agent.ConnectScript(ctx, slog.New(slog.DiscardHandler), handlers, &script)
	}
	dataDir := t.TempDir()
	workspace := t.TempDir()
	logger := slog.New(slog.DiscardHandler)

	var firstOutput strings.Builder
	sessionID, err := devPromptWithConnector(
		t.Context(),
		logger,
		[]string{"-data-dir", dataDir, "-agent", "codex-acp", "-workspace", workspace, "remember blue"},
		&firstOutput,
		io.Discard,
		connect,
	)
	if err != nil {
		t.Fatalf("first dev Prompt: %v", err)
	}
	if sessionID == "" {
		t.Fatal("first dev Prompt returned an empty Session ID")
	}
	if firstOutput.String() != "remembered\n" {
		t.Errorf("first output = %q, want %q", firstOutput.String(), "remembered\\n")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "aethos.db")); err != nil {
		t.Fatalf("durable Session database: %v", err)
	}

	var secondOutput strings.Builder
	resumedID, err := devPromptWithConnector(
		t.Context(),
		logger,
		[]string{"-data-dir", dataDir, "-session", sessionID, "what was it?"},
		&secondOutput,
		io.Discard,
		connect,
	)
	if err != nil {
		t.Fatalf("resumed dev Prompt: %v", err)
	}
	if resumedID != sessionID {
		t.Errorf("resumed Session ID = %q, want %q", resumedID, sessionID)
	}
	if secondOutput.String() != "blue\n" {
		t.Errorf("resumed output = %q, want %q", secondOutput.String(), "blue\\n")
	}
}

func TestDevPromptUsesConfiguredIdleTimeout(t *testing.T) {
	dataDir := t.TempDir()
	configFile := filepath.Join(dataDir, "config.toml")
	contents := `workspace = "/workspace"
default_agent = "codex-acp"
idle_timeout = "never"

[telegram]
bot_token = "token"
chat_id = -1001
allowed_user_ids = [123]
`
	if err := os.WriteFile(configFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	connect := func(context.Context, string, agent.Handlers) (*agent.Conn, error) {
		t.Fatal("Agent connector ran before invalid idle_timeout was rejected")
		return nil, nil
	}

	_, err := devPromptWithConnector(
		t.Context(),
		slog.New(slog.DiscardHandler),
		[]string{"-data-dir", dataDir, "-agent", "codex-acp", "hello"},
		io.Discard,
		io.Discard,
		connect,
	)
	if err == nil || !strings.Contains(err.Error(), "idle_timeout") {
		t.Errorf("dev Prompt error = %v, want configured idle_timeout error", err)
	}
}

func TestDevPromptAutoApprovesConfiguredToolKinds(t *testing.T) {
	dataDir := t.TempDir()
	configFile := filepath.Join(dataDir, "config.toml")
	contents := `workspace = "/workspace"
default_agent = "codex-acp"

[permissions]
timeout = "25ms"
auto_approve = ["read", "search"]

[rest]
bearer_token = "rest-token"

[telegram]
bot_token = "token"
chat_id = -1001
allowed_user_ids = [123]
`
	if err := os.WriteFile(configFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	script := agent.Script{Turns: []agent.Turn{{
		Permissions: []agent.ScriptedPermission{{
			Request: agent.PermissionRequest{
				ToolCallID: "call-1",
				Title:      "Read config.toml",
				Kind:       "read",
				Options: []agent.PermissionOption{
					{ID: "allow-once", Name: "Allow once", Kind: agent.PermissionAllowOnce},
					{ID: "reject-once", Name: "Reject", Kind: agent.PermissionRejectOnce},
				},
			},
			WantOptionID: "allow-once",
		}},
		Events: []agent.Event{agent.Message{Text: "read complete"}},
		Stop:   agent.StopEndTurn,
	}}}
	connect := func(ctx context.Context, _ string, handlers agent.Handlers) (*agent.Conn, error) {
		return agent.ConnectScript(ctx, slog.New(slog.DiscardHandler), handlers, &script)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	var output strings.Builder
	_, err := devPromptWithConnector(
		ctx,
		slog.New(slog.DiscardHandler),
		[]string{"-data-dir", dataDir, "-agent", "codex-acp", "read the config"},
		&output,
		io.Discard,
		connect,
	)
	if err != nil {
		t.Fatalf("dev Prompt with auto-approve: %v", err)
	}
	if got, want := output.String(), "read complete\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestRunFirstRunWritesConfigAndRunsTelegramChannelAcrossRestart(t *testing.T) {
	unsetEnv(t, "AETHOS_DATA_DIR")
	unsetEnv(t, "AETHOS_TELEGRAM_BOT_TOKEN")
	unsetEnv(t, "AETHOS_REST_BEARER_TOKEN")
	unsetEnv(t, "AETHOS_REST_LISTEN_ADDRESS")
	unsetEnv(t, "AETHOS_WORKSPACE")
	unsetEnv(t, "AETHOS_DEFAULT_AGENT")
	t.Setenv("AETHOS_REST_BEARER_TOKEN", "rest-token")
	t.Setenv("AETHOS_REST_LISTEN_ADDRESS", "127.0.0.1:0")

	var callsMu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/botvalid-token/")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		callsMu.Lock()
		calls = append(calls, method)
		callsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"aethos"}}`)
		case "getChat":
			fmt.Fprint(w, `{"ok":true,"result":{"id":-1001234567890,"type":"supergroup","title":"Aethos","is_forum":true}}`)
		case "createForumTopic":
			fmt.Fprint(w, `{"ok":true,"result":{"message_thread_id":101,"name":"Assistant"}}`)
		case "sendMessage":
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":201,"chat":{"id":-1001234567890,"type":"supergroup"}}}`)
		case "getUpdates":
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	dataDir := filepath.Join(t.TempDir(), "data")
	installedCatalog, err := agentcatalog.Open(filepath.Join(dataDir, "agents.json"), filepath.Join(dataDir, "agents"))
	if err != nil {
		t.Fatalf("open Agent catalog: %v", err)
	}
	_, err = installedCatalog.Install(t.Context(), agentcatalog.RegistryAgent{
		ID:      "codex-acp",
		Name:    "Codex",
		Version: "1.1.4",
		Distribution: agentcatalog.Distribution{NPX: &agentcatalog.PackageDistribution{
			Package: "@agentclientprotocol/codex-acp@1.1.4",
		}},
	}, nil)
	if err != nil {
		t.Fatalf("install Agent catalog fixture: %v", err)
	}
	workspace := t.TempDir()
	input := strings.NewReader("valid-token\n-1001234567890\n123456789\n" + workspace + "\ncodex-acp\n")
	var firstOutput strings.Builder
	client := telegram.NewClient(server.URL, server.Client())

	firstCtx, firstCancel := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- run(firstCtx, slog.New(slog.DiscardHandler), []string{"-data-dir", dataDir}, input, &firstOutput, io.Discard, client)
	}()
	waitForRun(t, func() bool {
		callsMu.Lock()
		defer callsMu.Unlock()
		return slices.Contains(calls, "getUpdates")
	})
	if !strings.Contains(firstOutput.String(), "Let's set up aethos") {
		t.Errorf("first-run output = %q, want wizard", firstOutput.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "config.toml")); err != nil {
		t.Fatalf("first run did not create config.toml: %v", err)
	}
	firstCancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first run: %v", err)
	}

	laterCtx, laterCancel := context.WithCancel(t.Context())
	laterDone := make(chan error, 1)
	go func() {
		laterDone <- run(laterCtx, slog.New(slog.DiscardHandler), []string{"-data-dir", dataDir}, strings.NewReader("this must not be read"), io.Discard, io.Discard, client)
	}()
	waitForRun(t, func() bool {
		callsMu.Lock()
		defer callsMu.Unlock()
		return countString(calls, "getUpdates") >= 2
	})
	laterCancel()
	if err := <-laterDone; err != nil {
		t.Fatalf("later run: %v", err)
	}
	callsMu.Lock()
	createdAssistant := countString(calls, "createForumTopic")
	callsMu.Unlock()
	if createdAssistant != 1 {
		t.Errorf("Assistant Topic creations across restart = %d, want 1", createdAssistant)
	}
}

func TestRunReportsInvalidConfigWithoutLaunchingWizard(t *testing.T) {
	unsetEnv(t, "AETHOS_TELEGRAM_BOT_TOKEN")
	unsetEnv(t, "AETHOS_WORKSPACE")
	unsetEnv(t, "AETHOS_DEFAULT_AGENT")

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("[broken"), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	var output strings.Builder
	err := run(
		t.Context(),
		slog.New(slog.DiscardHandler),
		[]string{"-data-dir", dataDir},
		strings.NewReader(""),
		&output,
		io.Discard,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("run with invalid config = %v, want actionable parse error", err)
	}
	if output.Len() != 0 {
		t.Errorf("invalid existing config launched wizard: %q", output.String())
	}
}

func TestRunRequiresConfiguredDefaultAgentInCatalog(t *testing.T) {
	dataDir := t.TempDir()
	configFile := filepath.Join(dataDir, "config.toml")
	contents := `workspace = "/workspace"
default_agent = "codex-acp"

[rest]
bearer_token = "rest-token"

[telegram]
bot_token = "token"
chat_id = -1001
allowed_user_ids = [123]
`
	if err := os.WriteFile(configFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := run(
		t.Context(), slog.New(slog.DiscardHandler), []string{"-data-dir", dataDir},
		strings.NewReader(""), io.Discard, io.Discard, nil,
	)
	if err == nil || !strings.Contains(err.Error(), `default Agent "codex-acp" is not installed`) {
		t.Fatalf("run error = %v, want missing catalog Agent error", err)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	original, present := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, original)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func waitForRun(t *testing.T, condition func() bool) {
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

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}
