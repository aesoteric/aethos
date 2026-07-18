// Package config owns aethos's persisted configuration and data-directory
// layout. Configuration comes from one TOML file, with environment variables
// taking precedence over values stored on disk.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aesoteric/aethos/internal/permission"
)

// DefaultIdleTimeout bounds how long an Agent subprocess remains attached
// without Prompt work when config.toml does not override it.
const DefaultIdleTimeout = 30 * time.Minute

// DefaultRESTListenAddress keeps the REST Channel on the loopback interface
// unless the operator explicitly exposes it.
const DefaultRESTListenAddress = "127.0.0.1:8080"

// Duration is a TOML duration encoded with Go's human-readable duration
// syntax, for example "30m" or "2h15m".
type Duration time.Duration

// UnmarshalText parses one TOML duration string.
func (duration *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("must be a duration such as 30m: %w", err)
	}
	*duration = Duration(parsed)
	return nil
}

// MarshalText formats a duration for config.toml.
func (duration Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(duration).String()), nil
}

const (
	dataDirEnv       = "AETHOS_DATA_DIR"
	botTokenEnv      = "AETHOS_TELEGRAM_BOT_TOKEN"
	restListenEnv    = "AETHOS_REST_LISTEN_ADDRESS"
	restTokenEnv     = "AETHOS_REST_BEARER_TOKEN"
	slackAppTokenEnv = "AETHOS_SLACK_APP_TOKEN"
	slackBotTokenEnv = "AETHOS_SLACK_BOT_TOKEN"
	workspaceEnv     = "AETHOS_WORKSPACE"
	defaultAgentEnv  = "AETHOS_DEFAULT_AGENT"
)

// Paths names every persistent location rooted in aethos's data directory.
type Paths struct {
	DataDir          string
	ConfigFile       string
	DatabaseFile     string
	AgentCatalogFile string
	AgentsDir        string
	LogsDir          string
}

// Telegram holds configuration for the Telegram Channel.
type Telegram struct {
	BotToken       string  `toml:"bot_token"`
	ChatID         int64   `toml:"chat_id"`
	AllowedUserIDs []int64 `toml:"allowed_user_ids"`
}

// REST holds configuration for the automation-facing HTTP Channel.
type REST struct {
	ListenAddress string `toml:"listen_address"`
	BearerToken   string `toml:"bearer_token"`
}

// Slack holds configuration for the Slack Channel.
type Slack struct {
	AppToken       string   `toml:"app_token"`
	BotToken       string   `toml:"bot_token"`
	ChannelID      string   `toml:"channel_id"`
	AllowedUserIDs []string `toml:"allowed_user_ids"`
}

// Permissions configures the fail-safe deadline and exact Agent tool kinds
// that the permission gate may approve without Channel interaction.
type Permissions struct {
	Timeout     Duration `toml:"timeout"`
	AutoApprove []string `toml:"auto_approve"`
}

// Config is the complete startup configuration.
type Config struct {
	Workspace    string      `toml:"workspace"`
	DefaultAgent string      `toml:"default_agent"`
	IdleTimeout  Duration    `toml:"idle_timeout"`
	Permissions  Permissions `toml:"permissions"`
	REST         *REST       `toml:"rest,omitempty"`
	Slack        *Slack      `toml:"slack,omitempty"`
	Telegram     *Telegram   `toml:"telegram,omitempty"`
}

// ValidateTelegramAllowedUserIDs enforces the fail-closed Telegram access
// policy shared by configuration, setup, and the running Channel.
func ValidateTelegramAllowedUserIDs(userIDs []int64) error {
	if len(userIDs) == 0 {
		return fmt.Errorf("telegram.allowed_user_ids must contain at least one Telegram user ID")
	}
	seen := make(map[int64]struct{}, len(userIDs))
	for _, userID := range userIDs {
		if userID <= 0 {
			return fmt.Errorf("telegram.allowed_user_ids contains invalid Telegram user ID %d", userID)
		}
		if _, duplicate := seen[userID]; duplicate {
			return fmt.Errorf("telegram.allowed_user_ids contains duplicate Telegram user ID %d", userID)
		}
		seen[userID] = struct{}{}
	}
	return nil
}

// ResolveDataDir selects the data directory. An explicit flag wins over
// AETHOS_DATA_DIR, which wins over ~/.aethos.
func ResolveDataDir(flagValue string) (string, error) {
	dataDir := strings.TrimSpace(flagValue)
	if dataDir == "" {
		dataDir = strings.TrimSpace(os.Getenv(dataDirEnv))
	}
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory for default data directory: %w", err)
		}
		dataDir = filepath.Join(home, ".aethos")
	}

	return absolutePath("data directory", dataDir)
}

// NewPaths roots all persistent aethos paths under dataDir.
func NewPaths(dataDir string) (Paths, error) {
	abs, err := absolutePath("data directory", dataDir)
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		DataDir:          abs,
		ConfigFile:       filepath.Join(abs, "config.toml"),
		DatabaseFile:     filepath.Join(abs, "aethos.db"),
		AgentCatalogFile: filepath.Join(abs, "agents.json"),
		AgentsDir:        filepath.Join(abs, "agents"),
		LogsDir:          filepath.Join(abs, "logs"),
	}, nil
}

func absolutePath(name, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s %q: %w", name, path, err)
	}
	return filepath.Clean(abs), nil
}

// Load reads, overlays, and validates a TOML configuration file.
func Load(path string) (Config, error) {
	cfg := defaultConfig()
	metadata, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		fields := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			fields = append(fields, key.String())
		}
		return Config{}, fmt.Errorf("unknown config field: %s", strings.Join(fields, ", "))
	}
	if cfg.REST != nil && strings.TrimSpace(cfg.REST.ListenAddress) == "" {
		cfg.REST.ListenAddress = DefaultRESTListenAddress
	}

	applyEnvironment(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return cfg, nil
}

func defaultConfig() Config {
	return Config{
		IdleTimeout: Duration(DefaultIdleTimeout),
		Permissions: Permissions{Timeout: Duration(permission.DefaultTimeout)},
	}
}

func applyEnvironment(cfg *Config) {
	if value, ok := os.LookupEnv(botTokenEnv); ok && cfg.Telegram != nil {
		cfg.Telegram.BotToken = value
	}
	if value, ok := os.LookupEnv(restTokenEnv); ok && cfg.REST != nil {
		cfg.REST.BearerToken = value
	}
	if value, ok := os.LookupEnv(restListenEnv); ok && strings.TrimSpace(value) != "" && cfg.REST != nil {
		cfg.REST.ListenAddress = value
	}
	if value, ok := os.LookupEnv(slackAppTokenEnv); ok && cfg.Slack != nil {
		cfg.Slack.AppToken = value
	}
	if value, ok := os.LookupEnv(slackBotTokenEnv); ok && cfg.Slack != nil {
		cfg.Slack.BotToken = value
	}
	if value, ok := os.LookupEnv(workspaceEnv); ok {
		cfg.Workspace = value
	}
	if value, ok := os.LookupEnv(defaultAgentEnv); ok {
		cfg.DefaultAgent = value
	}
}

// Validate reports the first missing or invalid required value.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Workspace) == "" {
		return fmt.Errorf("workspace is required (set it in config.toml or %s)", workspaceEnv)
	}
	if !filepath.IsAbs(c.Workspace) {
		return fmt.Errorf("workspace must be an absolute path, got %q", c.Workspace)
	}
	if strings.TrimSpace(c.DefaultAgent) == "" {
		return fmt.Errorf("default_agent is required (set it in config.toml or %s)", defaultAgentEnv)
	}
	if c.Telegram == nil && c.Slack == nil && c.REST == nil {
		return fmt.Errorf("at least one Channel section ([telegram], [slack], or [rest]) is required")
	}
	if c.Telegram != nil {
		if strings.TrimSpace(c.Telegram.BotToken) == "" {
			return fmt.Errorf("telegram.bot_token is required (set it in config.toml or %s)", botTokenEnv)
		}
		if c.Telegram.ChatID == 0 {
			return fmt.Errorf("telegram.chat_id is required")
		}
		if err := ValidateTelegramAllowedUserIDs(c.Telegram.AllowedUserIDs); err != nil {
			return err
		}
	}
	if c.Slack != nil {
		if !strings.HasPrefix(strings.TrimSpace(c.Slack.AppToken), "xapp-") {
			return fmt.Errorf("slack.app_token is required and must start with xapp- (set it in config.toml or %s)", slackAppTokenEnv)
		}
		if !strings.HasPrefix(strings.TrimSpace(c.Slack.BotToken), "xoxb-") {
			return fmt.Errorf("slack.bot_token is required and must start with xoxb- (set it in config.toml or %s)", slackBotTokenEnv)
		}
		if strings.TrimSpace(c.Slack.ChannelID) == "" {
			return fmt.Errorf("slack.channel_id is required")
		}
		if err := validateSlackAllowedUserIDs(c.Slack.AllowedUserIDs); err != nil {
			return err
		}
	}
	if c.REST != nil {
		if strings.TrimSpace(c.REST.ListenAddress) == "" {
			return fmt.Errorf("rest.listen_address is required")
		}
		if strings.TrimSpace(c.REST.BearerToken) == "" {
			return fmt.Errorf("rest.bearer_token is required (set it in config.toml or %s)", restTokenEnv)
		}
	}
	if c.IdleTimeout <= 0 {
		return fmt.Errorf("idle_timeout must be greater than zero")
	}
	if c.Permissions.Timeout <= 0 {
		return fmt.Errorf("permissions.timeout must be greater than zero")
	}
	seenKinds := make(map[string]struct{}, len(c.Permissions.AutoApprove))
	for _, kind := range c.Permissions.AutoApprove {
		if strings.TrimSpace(kind) == "" {
			return fmt.Errorf("permissions.auto_approve cannot contain an empty tool kind")
		}
		if _, duplicate := seenKinds[kind]; duplicate {
			return fmt.Errorf("permissions.auto_approve contains duplicate tool kind %q", kind)
		}
		seenKinds[kind] = struct{}{}
	}
	return nil
}

func validateSlackAllowedUserIDs(userIDs []string) error {
	if len(userIDs) == 0 {
		return fmt.Errorf("slack.allowed_user_ids must contain at least one Slack user ID")
	}
	seen := make(map[string]struct{}, len(userIDs))
	for _, userID := range userIDs {
		if strings.TrimSpace(userID) == "" {
			return fmt.Errorf("slack.allowed_user_ids cannot contain an empty Slack user ID")
		}
		if _, duplicate := seen[userID]; duplicate {
			return fmt.Errorf("slack.allowed_user_ids contains duplicate Slack user ID %q", userID)
		}
		seen[userID] = struct{}{}
	}
	return nil
}
