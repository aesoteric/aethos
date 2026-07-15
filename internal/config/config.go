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
)

// DefaultIdleTimeout bounds how long an Agent subprocess remains attached
// without Prompt work when config.toml does not override it.
const DefaultIdleTimeout = 30 * time.Minute

// Duration is a TOML duration encoded with Go's human-readable duration
// syntax, for example "30m" or "2h15m".
type Duration time.Duration

// UnmarshalText parses one TOML duration string.
func (duration *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("idle_timeout must be a duration such as 30m: %w", err)
	}
	*duration = Duration(parsed)
	return nil
}

// MarshalText formats a duration for config.toml.
func (duration Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(duration).String()), nil
}

const (
	dataDirEnv      = "AETHOS_DATA_DIR"
	botTokenEnv     = "AETHOS_TELEGRAM_BOT_TOKEN"
	workspaceEnv    = "AETHOS_WORKSPACE"
	defaultAgentEnv = "AETHOS_DEFAULT_AGENT"
)

// Paths names every persistent location rooted in aethos's data directory.
type Paths struct {
	DataDir      string
	ConfigFile   string
	DatabaseFile string
	LogsDir      string
}

// Telegram holds configuration for the Telegram Channel.
type Telegram struct {
	BotToken string `toml:"bot_token"`
}

// Config is the complete startup configuration.
type Config struct {
	Workspace    string   `toml:"workspace"`
	DefaultAgent string   `toml:"default_agent"`
	IdleTimeout  Duration `toml:"idle_timeout"`
	Telegram     Telegram `toml:"telegram"`
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
		DataDir:      abs,
		ConfigFile:   filepath.Join(abs, "config.toml"),
		DatabaseFile: filepath.Join(abs, "aethos.db"),
		LogsDir:      filepath.Join(abs, "logs"),
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
	cfg := Config{IdleTimeout: Duration(DefaultIdleTimeout)}
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

	applyEnvironment(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return cfg, nil
}

func applyEnvironment(cfg *Config) {
	if value, ok := os.LookupEnv(botTokenEnv); ok {
		cfg.Telegram.BotToken = value
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
	if strings.TrimSpace(c.Telegram.BotToken) == "" {
		return fmt.Errorf("telegram.bot_token is required (set it in config.toml or %s)", botTokenEnv)
	}
	if strings.TrimSpace(c.Workspace) == "" {
		return fmt.Errorf("workspace is required (set it in config.toml or %s)", workspaceEnv)
	}
	if !filepath.IsAbs(c.Workspace) {
		return fmt.Errorf("workspace must be an absolute path, got %q", c.Workspace)
	}
	if strings.TrimSpace(c.DefaultAgent) == "" {
		return fmt.Errorf("default_agent is required (set it in config.toml or %s)", defaultAgentEnv)
	}
	if c.IdleTimeout <= 0 {
		return fmt.Errorf("idle_timeout must be greater than zero")
	}
	return nil
}
