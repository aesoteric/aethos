package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aesoteric/aethos/internal/telegram"
)

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

func TestRunFirstRunWritesConfigThenLaterRunBootsSilently(t *testing.T) {
	unsetEnv(t, "AETHOS_DATA_DIR")
	unsetEnv(t, "AETHOS_TELEGRAM_BOT_TOKEN")
	unsetEnv(t, "AETHOS_WORKSPACE")
	unsetEnv(t, "AETHOS_DEFAULT_AGENT")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botvalid-token/getMe" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"aethos"}}`)
	}))
	t.Cleanup(server.Close)

	dataDir := filepath.Join(t.TempDir(), "data")
	workspace := t.TempDir()
	input := strings.NewReader("valid-token\n" + workspace + "\ncodex-acp\n")
	var firstOutput strings.Builder
	validator := telegram.NewClient(server.URL, server.Client())

	err := run(
		t.Context(),
		slog.New(slog.DiscardHandler),
		[]string{"-data-dir", dataDir},
		input,
		&firstOutput,
		io.Discard,
		validator,
	)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !strings.Contains(firstOutput.String(), "Let's set up aethos") {
		t.Errorf("first-run output = %q, want wizard", firstOutput.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "config.toml")); err != nil {
		t.Fatalf("first run did not create config.toml: %v", err)
	}

	var laterOutput, laterErrors strings.Builder
	err = run(
		t.Context(),
		slog.New(slog.DiscardHandler),
		[]string{"-data-dir", dataDir},
		strings.NewReader("this must not be read"),
		&laterOutput,
		&laterErrors,
		nil,
	)
	if err != nil {
		t.Fatalf("later run: %v", err)
	}
	if laterOutput.Len() != 0 || laterErrors.Len() != 0 {
		t.Errorf("later run was not silent: stdout=%q stderr=%q", laterOutput.String(), laterErrors.String())
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
