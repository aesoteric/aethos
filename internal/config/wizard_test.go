package config_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aesoteric/aethos/internal/config"
	"github.com/aesoteric/aethos/internal/slack"
	"github.com/aesoteric/aethos/internal/telegram"
)

func TestWizardChannelSelectionProducesOnlyChosenSections(t *testing.T) {
	for _, envName := range []string{
		"AETHOS_TELEGRAM_BOT_TOKEN",
		"AETHOS_SLACK_APP_TOKEN",
		"AETHOS_SLACK_BOT_TOKEN",
		"AETHOS_WORKSPACE",
		"AETHOS_DEFAULT_AGENT",
	} {
		unsetEnvironment(t, envName)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bottelegram-token/getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"aethos"}}`)
		case "/auth.test":
			fmt.Fprint(w, `{"ok":true,"team_id":"T0123456789","user_id":"U0AETHOS000","bot_id":"B0AETHOS000"}`)
		case "/apps.connections.open":
			fmt.Fprint(w, `{"ok":true,"url":"wss://socket.slack.test/link"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	tests := []struct {
		name         string
		selection    string
		answers      string
		wantTelegram bool
		wantSlack    bool
	}{
		{
			name:         "Telegram only",
			selection:    "1",
			answers:      "telegram-token\n-1001234567890\n123456789\n",
			wantTelegram: true,
		},
		{
			name:      "Slack only",
			selection: "2",
			answers:   "xapp-app-token\nxoxb-bot-token\nC0123456789\nU0123456789\n",
			wantSlack: true,
		},
		{
			name:         "Telegram and Slack",
			selection:    "3",
			answers:      "telegram-token\n-1001234567890\n123456789\nxapp-app-token\nxoxb-bot-token\nC0123456789\nU0123456789\n",
			wantTelegram: true,
			wantSlack:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			paths, err := config.NewPaths(filepath.Join(t.TempDir(), "aethos-data"))
			if err != nil {
				t.Fatalf("NewPaths: %v", err)
			}
			input := strings.NewReader(tt.selection + "\n" + tt.answers + workspace + "\ncodex-acp\n")
			var output strings.Builder

			got, err := config.RunWizard(
				t.Context(), input, &output, paths,
				telegram.NewClient(server.URL, server.Client()),
				slack.NewClient(server.URL, server.Client()),
				nil,
			)
			if err != nil {
				t.Fatalf("RunWizard: %v\noutput: %s", err, output.String())
			}
			if (got.Telegram != nil) != tt.wantTelegram {
				t.Errorf("Telegram configured = %t, want %t", got.Telegram != nil, tt.wantTelegram)
			}
			if (got.Slack != nil) != tt.wantSlack {
				t.Errorf("Slack configured = %t, want %t", got.Slack != nil, tt.wantSlack)
			}
			if got.REST != nil {
				t.Error("wizard configured the unselected REST Channel")
			}
		})
	}
}

func TestWizardExplainsRejectedSlackTokensAndPromptsAgain(t *testing.T) {
	for _, envName := range []string{
		"AETHOS_TELEGRAM_BOT_TOKEN",
		"AETHOS_SLACK_APP_TOKEN",
		"AETHOS_SLACK_BOT_TOKEN",
		"AETHOS_WORKSPACE",
		"AETHOS_DEFAULT_AGENT",
	} {
		unsetEnvironment(t, envName)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer xapp-good-token" && r.URL.Path == "/apps.connections.open" {
			fmt.Fprint(w, `{"ok":true,"url":"wss://socket.slack.test/link"}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer xoxb-good-token" && r.URL.Path == "/auth.test" {
			fmt.Fprint(w, `{"ok":true,"team_id":"T0123456789","user_id":"U0AETHOS000","bot_id":"B0AETHOS000"}`)
			return
		}
		fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	paths, err := config.NewPaths(filepath.Join(t.TempDir(), "aethos-data"))
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	input := strings.NewReader(strings.Join([]string{
		"2",
		"xapp-rejected-token",
		"xapp-good-token",
		"xoxb-rejected-token",
		"xoxb-good-token",
		"C0123456789",
		"U0123456789",
		workspace,
		"codex-acp",
		"",
	}, "\n"))
	var output strings.Builder

	got, err := config.RunWizard(
		t.Context(), input, &output, paths, nil,
		slack.NewClient(server.URL, server.Client()), nil,
	)
	if err != nil {
		t.Fatalf("RunWizard: %v\noutput: %s", err, output.String())
	}
	if got.Slack.AppToken != "xapp-good-token" || got.Slack.BotToken != "xoxb-good-token" {
		t.Errorf("Slack tokens = (%q, %q), want accepted retry values", got.Slack.AppToken, got.Slack.BotToken)
	}
	for _, want := range []string{
		"Slack app-level token rejected: slack apps.connections.open failed: invalid_auth. Try again.",
		"Slack bot token rejected: slack auth.test failed: invalid_auth. Try again.",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("wizard output = %q, want rejection %q", output.String(), want)
		}
	}
	if strings.Count(output.String(), "Slack app-level token: ") != 2 {
		t.Errorf("wizard output = %q, want app-level token prompt twice", output.String())
	}
	if strings.Count(output.String(), "Slack bot token: ") != 2 {
		t.Errorf("wizard output = %q, want bot token prompt twice", output.String())
	}
}

func TestWizardValidatesEnvironmentSlackTokensWithoutWritingThemToDisk(t *testing.T) {
	unsetEnvironment(t, "AETHOS_TELEGRAM_BOT_TOKEN")
	t.Setenv("AETHOS_SLACK_APP_TOKEN", "xapp-environment-secret")
	t.Setenv("AETHOS_SLACK_BOT_TOKEN", "xoxb-environment-secret")
	unsetEnvironment(t, "AETHOS_WORKSPACE")
	unsetEnvironment(t, "AETHOS_DEFAULT_AGENT")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/apps.connections.open" && r.Header.Get("Authorization") == "Bearer xapp-environment-secret":
			fmt.Fprint(w, `{"ok":true,"url":"wss://socket.slack.test/link"}`)
		case r.URL.Path == "/auth.test" && r.Header.Get("Authorization") == "Bearer xoxb-environment-secret":
			fmt.Fprint(w, `{"ok":true,"team_id":"T0123456789","user_id":"U0AETHOS000","bot_id":"B0AETHOS000"}`)
		default:
			fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
		}
	}))
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	paths, err := config.NewPaths(filepath.Join(t.TempDir(), "aethos-data"))
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	input := strings.NewReader("2\nC0123456789\nU0123456789\n" + workspace + "\ncodex-acp\n")
	var output strings.Builder

	got, err := config.RunWizard(
		t.Context(), input, &output, paths, nil,
		slack.NewClient(server.URL, server.Client()), nil,
	)
	if err != nil {
		t.Fatalf("RunWizard: %v\noutput: %s", err, output.String())
	}
	if got.Slack.AppToken != "xapp-environment-secret" || got.Slack.BotToken != "xoxb-environment-secret" {
		t.Errorf("Slack tokens = (%q, %q), want environment values", got.Slack.AppToken, got.Slack.BotToken)
	}
	for _, prompt := range []string{"Slack app-level token: ", "Slack bot token: "} {
		if strings.Contains(output.String(), prompt) {
			t.Errorf("wizard prompted with %q despite environment secret: %q", prompt, output.String())
		}
	}

	written, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	for _, secret := range []string{"xapp-environment-secret", "xoxb-environment-secret"} {
		if strings.Contains(string(written), secret) {
			t.Errorf("generated config contains environment-only secret %q", secret)
		}
	}
}

func TestWizardRepromptsForRejectedTokenAndWritesCommentedConfig(t *testing.T) {
	unsetEnvironment(t, "AETHOS_TELEGRAM_BOT_TOKEN")
	unsetEnvironment(t, "AETHOS_SLACK_APP_TOKEN")
	unsetEnvironment(t, "AETHOS_SLACK_BOT_TOKEN")
	unsetEnvironment(t, "AETHOS_WORKSPACE")
	unsetEnvironment(t, "AETHOS_DEFAULT_AGENT")

	requestedTokens := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		requestedTokens = append(requestedTokens, r.URL.Path)
		if r.URL.Path == "/botgood-token/getMe" {
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"aethos"}}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "aethos-data")
	paths, err := config.NewPaths(dataDir)
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	input := strings.NewReader("1\nbad-token\ngood-token\n-1001234567890\n123456789, 987654321\n" + workspace + "\ncodex-acp\n")
	var output strings.Builder

	validator := telegram.NewClient(server.URL, server.Client())
	got, err := config.RunWizard(t.Context(), input, &output, paths, validator, nil, []config.AgentChoice{
		{ID: "codex-acp", Name: "Codex", Type: "npx"},
		{ID: "goose", Name: "goose", Type: "binary"},
	})
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}

	if len(requestedTokens) != 2 {
		t.Fatalf("Telegram validation requests = %v, want rejected then accepted token", requestedTokens)
	}
	if strings.Count(output.String(), "Telegram bot token:") != 2 {
		t.Errorf("wizard output = %q, want token prompt twice", output.String())
	}
	if !strings.Contains(output.String(), "Unauthorized") {
		t.Errorf("wizard output = %q, want clear Telegram rejection", output.String())
	}
	for _, want := range []string{"Installed Agents:", "codex-acp", "Codex", "npx", "goose", "binary"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("wizard output = %q, want installed Agent choice %q", output.String(), want)
		}
	}
	if got.Telegram.BotToken != "good-token" || got.Workspace != workspace || got.DefaultAgent != "codex-acp" {
		t.Errorf("RunWizard config = %#v, want collected values", got)
	}
	if got.REST != nil || got.Slack != nil {
		t.Errorf("RunWizard configured unselected Channels: REST=%#v Slack=%#v", got.REST, got.Slack)
	}
	if got.Telegram.ChatID != -1001234567890 {
		t.Errorf("Telegram.ChatID = %d, want -1001234567890", got.Telegram.ChatID)
	}
	if want := []int64{123456789, 987654321}; !slices.Equal(got.Telegram.AllowedUserIDs, want) {
		t.Errorf("Telegram.AllowedUserIDs = %v, want %v", got.Telegram.AllowedUserIDs, want)
	}

	written, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	for _, comment := range []string{"# Workspace", "# Agent", "# permissions", "# Telegram", "allowlisted", "AETHOS_TELEGRAM_BOT_TOKEN"} {
		if !strings.Contains(string(written), comment) {
			t.Errorf("generated config does not contain explanatory comment %q:\n%s", comment, written)
		}
	}
	info, err := os.Stat(paths.ConfigFile)
	if err != nil {
		t.Fatalf("stat generated config: %v", err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Errorf("generated config mode = %o, want 600", gotMode)
	}
}

func TestWizardDoesNotWriteEnvironmentBotTokenToDisk(t *testing.T) {
	t.Setenv("AETHOS_TELEGRAM_BOT_TOKEN", "environment-secret")
	unsetEnvironment(t, "AETHOS_SLACK_APP_TOKEN")
	unsetEnvironment(t, "AETHOS_SLACK_BOT_TOKEN")
	unsetEnvironment(t, "AETHOS_WORKSPACE")
	unsetEnvironment(t, "AETHOS_DEFAULT_AGENT")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"aethos"}}`)
	}))
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	paths, err := config.NewPaths(filepath.Join(t.TempDir(), "aethos-data"))
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	input := strings.NewReader("1\n-1001234567890\n123456789\n" + workspace + "\ncodex-acp\n")
	var output strings.Builder

	validator := telegram.NewClient(server.URL, server.Client())
	got, err := config.RunWizard(t.Context(), input, &output, paths, validator, nil, nil)
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if got.Telegram.BotToken != "environment-secret" {
		t.Errorf("Telegram.BotToken = %q, want environment value", got.Telegram.BotToken)
	}
	if got.REST != nil || got.Slack != nil {
		t.Errorf("RunWizard configured unselected Channels: REST=%#v Slack=%#v", got.REST, got.Slack)
	}
	if strings.Contains(output.String(), "Telegram bot token:") {
		t.Errorf("wizard prompted for token despite environment secret: %q", output.String())
	}

	written, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if strings.Contains(string(written), "environment-secret") {
		t.Fatal("generated config contains the environment-only bot token")
	}
}

func unsetEnvironment(t *testing.T, key string) {
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
