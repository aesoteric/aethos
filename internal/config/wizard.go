package config

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// TokenValidator checks a Telegram bot token at Telegram's protocol edge.
type TokenValidator interface {
	ValidateToken(context.Context, string) error
}

// RunWizard collects a first-run configuration, validates it, and writes it to
// paths.ConfigFile. Secrets supplied through the environment are never copied
// into the file.
func RunWizard(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	paths Paths,
	validator TokenValidator,
) (Config, error) {
	if validator == nil {
		return Config{}, fmt.Errorf("Telegram token validator is required")
	}

	interaction := wizardInteraction{reader: bufio.NewReader(input), output: output}
	fmt.Fprintln(output, "No config.toml found. Let's set up aethos.")

	token, tokenFromEnvironment, err := interaction.collectToken(ctx, validator)
	if err != nil {
		return Config{}, err
	}
	workspace, err := interaction.collectWorkspace()
	if err != nil {
		return Config{}, err
	}
	defaultAgent, err := interaction.collectValue(defaultAgentEnv, "Default Agent command: ")
	if err != nil {
		return Config{}, err
	}

	effective := Config{
		Workspace:    workspace,
		DefaultAgent: defaultAgent,
		IdleTimeout:  Duration(DefaultIdleTimeout),
		Telegram:     Telegram{BotToken: token},
	}
	if err := effective.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate setup: %w", err)
	}

	persisted := effective
	if tokenFromEnvironment {
		persisted.Telegram.BotToken = ""
	}
	if err := writeCommentedConfig(paths, persisted); err != nil {
		return Config{}, err
	}
	fmt.Fprintf(output, "Configuration written to %s\n", paths.ConfigFile)

	loaded, err := Load(paths.ConfigFile)
	if err != nil {
		return Config{}, fmt.Errorf("verify generated config: %w", err)
	}
	return loaded, nil
}

type wizardInteraction struct {
	reader *bufio.Reader
	output io.Writer
}

func (i wizardInteraction) collectToken(
	ctx context.Context,
	validator TokenValidator,
) (token string, fromEnvironment bool, err error) {
	if value, ok := nonEmptyEnvironment(botTokenEnv); ok {
		fmt.Fprintf(i.output, "Validating Telegram bot token from %s...\n", botTokenEnv)
		if err := validator.ValidateToken(ctx, value); err != nil {
			return "", false, fmt.Errorf("%s is not a valid Telegram bot token: %w", botTokenEnv, err)
		}
		return value, true, nil
	}

	for {
		value, err := i.prompt("Telegram bot token: ")
		if err != nil {
			return "", false, err
		}
		if value == "" {
			fmt.Fprintln(i.output, "A Telegram bot token is required.")
			continue
		}
		if err := validator.ValidateToken(ctx, value); err != nil {
			fmt.Fprintf(i.output, "Telegram bot token rejected: %v. Try again.\n", err)
			continue
		}
		return value, false, nil
	}
}

func (i wizardInteraction) collectWorkspace() (string, error) {
	if value, ok := nonEmptyEnvironment(workspaceEnv); ok {
		workspace, err := validateWorkspace(value)
		if err != nil {
			return "", fmt.Errorf("invalid %s: %w", workspaceEnv, err)
		}
		return workspace, nil
	}

	for {
		value, err := i.prompt("Workspace directory: ")
		if err != nil {
			return "", err
		}
		workspace, err := validateWorkspace(value)
		if err != nil {
			fmt.Fprintf(i.output, "Workspace rejected: %v. Try again.\n", err)
			continue
		}
		return workspace, nil
	}
}

func validateWorkspace(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("a directory is required")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", value, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", abs)
	}
	return filepath.Clean(abs), nil
}

func (i wizardInteraction) collectValue(envName, question string) (string, error) {
	if value, ok := nonEmptyEnvironment(envName); ok {
		return value, nil
	}
	for {
		value, err := i.prompt(question)
		if err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
		fmt.Fprintln(i.output, "A value is required.")
	}
}

func (i wizardInteraction) prompt(question string) (string, error) {
	fmt.Fprint(i.output, question)
	line, err := i.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read setup answer: %w", err)
	}
	value := strings.TrimSpace(line)
	if errors.Is(err, io.EOF) && value == "" {
		return "", fmt.Errorf("read setup answer: input ended")
	}
	return value, nil
}

func nonEmptyEnvironment(name string) (string, bool) {
	value, ok := os.LookupEnv(name)
	value = strings.TrimSpace(value)
	return value, ok && value != ""
}

func writeCommentedConfig(paths Paths, cfg Config) error {
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory %q: %w", paths.DataDir, err)
	}
	if err := os.MkdirAll(paths.LogsDir, 0o700); err != nil {
		return fmt.Errorf("create logs directory %q: %w", paths.LogsDir, err)
	}

	var contents bytes.Buffer
	contents.WriteString(`# aethos configuration
#
# Workspace is the directory offered when a Session is created.
# Agent is the command used to start the default ACP-compatible Agent.
# idle_timeout releases an Agent subprocess while keeping its Session record;
# the next Prompt resumes it automatically.
# Telegram bot_token authenticates the Telegram Channel. For containers and
# other secret-managed deployments, leave it empty and set
# AETHOS_TELEGRAM_BOT_TOKEN instead. AETHOS_WORKSPACE and
# AETHOS_DEFAULT_AGENT also override their corresponding values in this file.

`)
	if err := toml.NewEncoder(&contents).Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	file, err := os.OpenFile(paths.ConfigFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create config %q: %w", paths.ConfigFile, err)
	}
	_, writeErr := file.Write(contents.Bytes())
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write config %q: %w", paths.ConfigFile, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close config %q: %w", paths.ConfigFile, closeErr)
	}
	return nil
}
