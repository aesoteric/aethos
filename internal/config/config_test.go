package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	if time.Duration(got.IdleTimeout) != 45*time.Minute {
		t.Errorf("IdleTimeout = %s, want 45m", time.Duration(got.IdleTimeout))
	}
}

func TestLoadDefaultsAndValidatesIdleTimeout(t *testing.T) {
	t.Setenv("AETHOS_TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("AETHOS_WORKSPACE", "/workspace")
	t.Setenv("AETHOS_DEFAULT_AGENT", "codex-acp")

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, path, "[telegram]\nbot_token = \"\"\n")
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load default idle timeout: %v", err)
	}
	if time.Duration(got.IdleTimeout) != config.DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %s, want default %s", time.Duration(got.IdleTimeout), config.DefaultIdleTimeout)
	}
	if time.Duration(got.Permissions.Timeout) != 10*time.Minute {
		t.Errorf("Permissions.Timeout = %s, want default 10m", time.Duration(got.Permissions.Timeout))
	}

	writeFixture(t, path, "idle_timeout = \"never\"\n[telegram]\nbot_token = \"\"\n")
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "idle_timeout") {
		t.Errorf("Load invalid idle timeout error = %v, want actionable idle_timeout error", err)
	}
}

func TestLoadReadsAndValidatesPermissionPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

[permissions]
timeout = "45s"
auto_approve = ["read", "search"]

[telegram]
bot_token = "token"
`)

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load permission policy: %v", err)
	}
	if time.Duration(got.Permissions.Timeout) != 45*time.Second {
		t.Errorf("Permissions.Timeout = %s, want 45s", time.Duration(got.Permissions.Timeout))
	}
	if want := []string{"read", "search"}; !slices.Equal(got.Permissions.AutoApprove, want) {
		t.Errorf("Permissions.AutoApprove = %q, want %q", got.Permissions.AutoApprove, want)
	}

	writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

[permissions]
timeout = "0s"

[telegram]
bot_token = "token"
`)
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "permissions.timeout") {
		t.Errorf("Load invalid permission timeout error = %v, want actionable permissions.timeout error", err)
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
