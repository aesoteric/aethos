package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aesoteric/aethos/internal/config"
)

func TestResolveDataDirUsesFlagThenEnvironmentThenHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		name      string
		flagValue string
		envValue  string
		want      string
	}{
		{
			name:      "flag wins over environment",
			flagValue: filepath.Join(home, "from-flag"),
			envValue:  filepath.Join(home, "from-env"),
			want:      filepath.Join(home, "from-flag"),
		},
		{
			name:     "environment wins over default",
			envValue: filepath.Join(home, "from-env"),
			want:     filepath.Join(home, "from-env"),
		},
		{
			name: "home supplies default",
			want: filepath.Join(home, ".aethos"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AETHOS_DATA_DIR", tt.envValue)
			got, err := config.ResolveDataDir(tt.flagValue)
			if err != nil {
				t.Fatalf("ResolveDataDir: %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveDataDir(%q) = %q, want %q", tt.flagValue, got, tt.want)
			}
		})
	}
}

func TestPathsKeepAllPersistentFilesUnderDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	paths, err := config.NewPaths(dataDir)
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}

	want := map[string]string{
		"config":   filepath.Join(dataDir, "config.toml"),
		"database": filepath.Join(dataDir, "aethos.db"),
		"logs":     filepath.Join(dataDir, "logs"),
	}
	got := map[string]string{
		"config":   paths.ConfigFile,
		"database": paths.DatabaseFile,
		"logs":     paths.LogsDir,
	}
	for name, wantPath := range want {
		if got[name] != wantPath {
			t.Errorf("%s path = %q, want %q", name, got[name], wantPath)
		}
	}
}

func TestLoadReadsFixtureAndAppliesEnvironmentOverrides(t *testing.T) {
	t.Setenv("AETHOS_TELEGRAM_BOT_TOKEN", "env-token")
	t.Setenv("AETHOS_WORKSPACE", "/env/workspace")
	t.Setenv("AETHOS_DEFAULT_AGENT", "env-agent")

	got, err := config.Load(filepath.Join("testdata", "valid.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Telegram.BotToken != "env-token" {
		t.Errorf("Telegram.BotToken = %q, want environment override", got.Telegram.BotToken)
	}
	if got.Workspace != "/env/workspace" {
		t.Errorf("Workspace = %q, want environment override", got.Workspace)
	}
	if got.DefaultAgent != "env-agent" {
		t.Errorf("DefaultAgent = %q, want environment override", got.DefaultAgent)
	}
}

func TestLoadAllowsBotTokenToExistOnlyInEnvironment(t *testing.T) {
	t.Setenv("AETHOS_TELEGRAM_BOT_TOKEN", "env-only-token")

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

[telegram]
bot_token = ""
`)

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Telegram.BotToken != "env-only-token" {
		t.Errorf("Telegram.BotToken = %q, want env-only-token", got.Telegram.BotToken)
	}
}

func TestLoadReportsActionableConfigurationErrors(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    string
	}{
		{name: "invalid TOML", fixture: "invalid.toml", want: "parse config"},
		{name: "missing field", fixture: "missing-workspace.toml", want: "workspace is required"},
		{name: "unknown field", fixture: "unknown-field.toml", want: "unknown config field"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AETHOS_TELEGRAM_BOT_TOKEN", "file-token")
			t.Setenv("AETHOS_WORKSPACE", "")
			t.Setenv("AETHOS_DEFAULT_AGENT", "codex-acp")

			_, err := config.Load(filepath.Join("testdata", tt.fixture))
			if err == nil {
				t.Fatal("Load succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
