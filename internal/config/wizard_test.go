package config_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aesoteric/aethos/internal/config"
	"github.com/aesoteric/aethos/internal/telegram"
)

func TestWizardRepromptsForRejectedTokenAndWritesCommentedConfig(t *testing.T) {
	unsetEnvironment(t, "AETHOS_TELEGRAM_BOT_TOKEN")
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
	input := strings.NewReader("bad-token\ngood-token\n" + workspace + "\ncodex-acp\n")
	var output strings.Builder

	validator := telegram.NewClient(server.URL, server.Client())
	got, err := config.RunWizard(t.Context(), input, &output, paths, validator)
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
	if got.Telegram.BotToken != "good-token" || got.Workspace != workspace || got.DefaultAgent != "codex-acp" {
		t.Errorf("RunWizard config = %#v, want collected values", got)
	}

	written, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	for _, comment := range []string{"# Workspace", "# Agent", "# Telegram", "AETHOS_TELEGRAM_BOT_TOKEN"} {
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
	input := strings.NewReader(workspace + "\ncodex-acp\n")
	var output strings.Builder

	validator := telegram.NewClient(server.URL, server.Client())
	got, err := config.RunWizard(t.Context(), input, &output, paths, validator)
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if got.Telegram.BotToken != "environment-secret" {
		t.Errorf("Telegram.BotToken = %q, want environment value", got.Telegram.BotToken)
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
