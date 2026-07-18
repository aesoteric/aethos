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
		"config":        filepath.Join(dataDir, "config.toml"),
		"database":      filepath.Join(dataDir, "aethos.db"),
		"agent catalog": filepath.Join(dataDir, "agents.json"),
		"agents":        filepath.Join(dataDir, "agents"),
		"logs":          filepath.Join(dataDir, "logs"),
	}
	got := map[string]string{
		"config":        paths.ConfigFile,
		"database":      paths.DatabaseFile,
		"agent catalog": paths.AgentCatalogFile,
		"agents":        paths.AgentsDir,
		"logs":          paths.LogsDir,
	}
	for name, wantPath := range want {
		if got[name] != wantPath {
			t.Errorf("%s path = %q, want %q", name, got[name], wantPath)
		}
	}
}

func TestLoadReadsFixtureAndAppliesEnvironmentOverrides(t *testing.T) {
	t.Setenv("AETHOS_TELEGRAM_BOT_TOKEN", "env-token")
	t.Setenv("AETHOS_REST_BEARER_TOKEN", "env-rest-token")
	t.Setenv("AETHOS_REST_LISTEN_ADDRESS", "127.0.0.1:9191")
	t.Setenv("AETHOS_WORKSPACE", "/env/workspace")
	t.Setenv("AETHOS_DEFAULT_AGENT", "env-agent")

	got, err := config.Load(filepath.Join("testdata", "valid.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Telegram.BotToken != "env-token" {
		t.Errorf("Telegram.BotToken = %q, want environment override", got.Telegram.BotToken)
	}
	if got.REST.BearerToken != "env-rest-token" {
		t.Errorf("REST.BearerToken = %q, want environment override", got.REST.BearerToken)
	}
	if got.REST.ListenAddress != "127.0.0.1:9191" {
		t.Errorf("REST.ListenAddress = %q, want environment override", got.REST.ListenAddress)
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
	if got.Telegram.ChatID != -1001234567890 {
		t.Errorf("Telegram.ChatID = %d, want -1001234567890", got.Telegram.ChatID)
	}
	if want := []int64{123456789, 987654321}; !slices.Equal(got.Telegram.AllowedUserIDs, want) {
		t.Errorf("Telegram.AllowedUserIDs = %v, want %v", got.Telegram.AllowedUserIDs, want)
	}
}

func TestLoadEnablesOnlyPresentChannelSections(t *testing.T) {
	tests := []struct {
		name         string
		channels     string
		wantTelegram bool
		wantSlack    bool
	}{
		{
			name: "Telegram only",
			channels: `[telegram]
bot_token = "telegram-token"
chat_id = -1001
allowed_user_ids = [123]
`,
			wantTelegram: true,
		},
		{
			name: "Slack only",
			channels: `[slack]
app_token = "xapp-file-token"
bot_token = "xoxb-file-token"
channel_id = "C0123456789"
allowed_user_ids = ["U0123456789"]
`,
			wantSlack: true,
		},
		{
			name: "Telegram and Slack",
			channels: `[telegram]
bot_token = "telegram-token"
chat_id = -1001
allowed_user_ids = [123]

[slack]
app_token = "xapp-file-token"
bot_token = "xoxb-file-token"
channel_id = "C0123456789"
allowed_user_ids = ["U0123456789"]
`,
			wantTelegram: true,
			wantSlack:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

`+tt.channels)

			got, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if (got.Telegram != nil) != tt.wantTelegram {
				t.Errorf("Telegram configured = %t, want %t", got.Telegram != nil, tt.wantTelegram)
			}
			if (got.Slack != nil) != tt.wantSlack {
				t.Errorf("Slack configured = %t, want %t", got.Slack != nil, tt.wantSlack)
			}
			if got.REST != nil {
				t.Error("REST configured without a [rest] section")
			}
		})
	}
}

func TestLoadRejectsConfigWithoutAChannelSection(t *testing.T) {
	t.Setenv("AETHOS_TELEGRAM_BOT_TOKEN", "environment-telegram-token")
	t.Setenv("AETHOS_SLACK_APP_TOKEN", "xapp-environment-token")
	t.Setenv("AETHOS_SLACK_BOT_TOKEN", "xoxb-environment-token")
	t.Setenv("AETHOS_REST_BEARER_TOKEN", "environment-rest-token")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"
`)

	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "at least one Channel section") {
		t.Fatalf("Load error = %v, want a clear missing Channel diagnostic", err)
	}
}

func TestLoadValidatesPresentSlackSectionFailClosed(t *testing.T) {
	tests := []struct {
		name      string
		slack     string
		wantError string
	}{
		{
			name: "missing app token",
			slack: `bot_token = "xoxb-bot-token"
channel_id = "C0123456789"
allowed_user_ids = ["U0123456789"]`,
			wantError: "slack.app_token",
		},
		{
			name: "invalid app token",
			slack: `app_token = "not-an-app-token"
bot_token = "xoxb-bot-token"
channel_id = "C0123456789"
allowed_user_ids = ["U0123456789"]`,
			wantError: "must start with xapp-",
		},
		{
			name: "missing bot token",
			slack: `app_token = "xapp-app-token"
channel_id = "C0123456789"
allowed_user_ids = ["U0123456789"]`,
			wantError: "slack.bot_token",
		},
		{
			name: "invalid bot token",
			slack: `app_token = "xapp-app-token"
bot_token = "not-a-bot-token"
channel_id = "C0123456789"
allowed_user_ids = ["U0123456789"]`,
			wantError: "must start with xoxb-",
		},
		{
			name: "missing channel ID",
			slack: `app_token = "xapp-app-token"
bot_token = "xoxb-bot-token"
allowed_user_ids = ["U0123456789"]`,
			wantError: "slack.channel_id",
		},
		{
			name: "empty allowed user IDs",
			slack: `app_token = "xapp-app-token"
bot_token = "xoxb-bot-token"
channel_id = "C0123456789"
allowed_user_ids = []`,
			wantError: "at least one Slack user ID",
		},
		{
			name: "blank allowed user ID",
			slack: `app_token = "xapp-app-token"
bot_token = "xoxb-bot-token"
channel_id = "C0123456789"
allowed_user_ids = [" "]`,
			wantError: "empty Slack user ID",
		},
		{
			name: "duplicate allowed user ID",
			slack: `app_token = "xapp-app-token"
bot_token = "xoxb-bot-token"
channel_id = "C0123456789"
allowed_user_ids = ["U0123456789", "U0123456789"]`,
			wantError: "duplicate Slack user ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

[slack]
`+tt.slack+"\n")

			_, err := config.Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tt.wantError)
			}
		})
	}
}

func TestLoadAppliesSlackTokenEnvironmentOverrides(t *testing.T) {
	t.Setenv("AETHOS_SLACK_APP_TOKEN", "xapp-environment-token")
	t.Setenv("AETHOS_SLACK_BOT_TOKEN", "xoxb-environment-token")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

[slack]
app_token = "invalid-file-app-token"
bot_token = "invalid-file-bot-token"
channel_id = "C0123456789"
allowed_user_ids = ["U0123456789"]
`)

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Slack.AppToken != "xapp-environment-token" {
		t.Errorf("Slack.AppToken = %q, want environment override", got.Slack.AppToken)
	}
	if got.Slack.BotToken != "xoxb-environment-token" {
		t.Errorf("Slack.BotToken = %q, want environment override", got.Slack.BotToken)
	}
}

func TestLoadValidatesPresentRESTSectionFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

[rest]
`)

	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "rest.bearer_token") {
		t.Fatalf("Load error = %v, want present REST section validated in full", err)
	}
}

func TestLoadRequiresTelegramForumAndAllowlist(t *testing.T) {
	t.Setenv("AETHOS_REST_BEARER_TOKEN", "rest-token")
	tests := []struct {
		name      string
		telegram  string
		wantError string
	}{
		{
			name:      "missing forum group",
			telegram:  "allowed_user_ids = [123]",
			wantError: "telegram.chat_id",
		},
		{
			name:      "empty allowlist",
			telegram:  "chat_id = -1001234567890",
			wantError: "telegram.allowed_user_ids",
		},
		{
			name: "duplicate allowlisted user",
			telegram: `chat_id = -1001234567890
allowed_user_ids = [123, 123]`,
			wantError: "duplicate Telegram user ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

[telegram]
bot_token = "token"
`+tt.telegram+"\n")

			_, err := config.Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tt.wantError)
			}
		})
	}
}

func TestLoadDefaultsAndValidatesIdleTimeout(t *testing.T) {
	t.Setenv("AETHOS_TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("AETHOS_REST_BEARER_TOKEN", "rest-token")
	t.Setenv("AETHOS_REST_LISTEN_ADDRESS", "")
	t.Setenv("AETHOS_WORKSPACE", "/workspace")
	t.Setenv("AETHOS_DEFAULT_AGENT", "codex-acp")

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, path, "[telegram]\nbot_token = \"\"\nchat_id = -1001\nallowed_user_ids = [123]\n\n[rest]\n")
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
	if got.REST.ListenAddress != config.DefaultRESTListenAddress {
		t.Errorf("REST.ListenAddress = %q, want default %q", got.REST.ListenAddress, config.DefaultRESTListenAddress)
	}

	writeFixture(t, path, "idle_timeout = \"never\"\n[telegram]\nbot_token = \"\"\nchat_id = -1001\nallowed_user_ids = [123]\n")
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "idle_timeout") {
		t.Errorf("Load invalid idle timeout error = %v, want actionable idle_timeout error", err)
	}
}

func TestLoadReadsAndValidatesPermissionPolicy(t *testing.T) {
	t.Setenv("AETHOS_REST_BEARER_TOKEN", "rest-token")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

[permissions]
timeout = "45s"
auto_approve = ["read", "search"]

[telegram]
bot_token = "token"
chat_id = -1001
allowed_user_ids = [123]
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
chat_id = -1001
allowed_user_ids = [123]
`)
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "permissions.timeout") {
		t.Errorf("Load invalid permission timeout error = %v, want actionable permissions.timeout error", err)
	}
}

func TestLoadAllowsBotTokenToExistOnlyInEnvironment(t *testing.T) {
	t.Setenv("AETHOS_TELEGRAM_BOT_TOKEN", "env-only-token")
	t.Setenv("AETHOS_REST_BEARER_TOKEN", "rest-token")

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, path, `workspace = "/workspace"
default_agent = "codex-acp"

[telegram]
bot_token = ""
chat_id = -1001
allowed_user_ids = [123]
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
	t.Setenv("AETHOS_REST_BEARER_TOKEN", "rest-token")
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
