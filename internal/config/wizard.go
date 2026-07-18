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
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// TokenValidator checks a Telegram bot token at Telegram's protocol edge.
type TokenValidator interface {
	ValidateToken(context.Context, string) error
}

// AgentChoice is one installed Agent offered by the setup wizard.
type AgentChoice struct {
	ID   string
	Name string
	Type string
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
	agents []AgentChoice,
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
	chatID, err := interaction.collectChatID()
	if err != nil {
		return Config{}, err
	}
	allowedUserIDs, err := interaction.collectAllowedUserIDs()
	if err != nil {
		return Config{}, err
	}
	workspace, err := interaction.collectWorkspace()
	if err != nil {
		return Config{}, err
	}
	defaultAgent, err := interaction.collectAgent(agents)
	if err != nil {
		return Config{}, err
	}
	restToken, restTokenFromEnvironment, err := interaction.collectSecret(restTokenEnv, "REST bearer token: ")
	if err != nil {
		return Config{}, err
	}

	effective := defaultConfig()
	effective.Workspace = workspace
	effective.DefaultAgent = defaultAgent
	effective.REST = &REST{ListenAddress: DefaultRESTListenAddress}
	effective.Telegram = &Telegram{}
	effective.REST.BearerToken = restToken
	effective.Telegram.BotToken = token
	effective.Telegram.ChatID = chatID
	effective.Telegram.AllowedUserIDs = allowedUserIDs
	if err := effective.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate setup: %w", err)
	}

	persisted := effective
	if tokenFromEnvironment {
		persisted.Telegram.BotToken = ""
	}
	if restTokenFromEnvironment {
		persisted.REST.BearerToken = ""
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

func (i wizardInteraction) collectAgent(agents []AgentChoice) (string, error) {
	if len(agents) == 0 {
		return i.collectValue(defaultAgentEnv, "Default Agent command: ")
	}
	fmt.Fprintln(i.output, "Installed Agents:")
	for _, agent := range agents {
		fmt.Fprintf(i.output, "  %s — %s (%s)\n", agent.ID, agent.Name, agent.Type)
	}
	if value, ok := nonEmptyEnvironment(defaultAgentEnv); ok {
		if isAgentChoice(value, agents) {
			return value, nil
		}
		return "", fmt.Errorf("invalid %s: Agent %q is not installed", defaultAgentEnv, value)
	}
	for {
		value, err := i.prompt("Default Agent ID: ")
		if err != nil {
			return "", err
		}
		if isAgentChoice(value, agents) {
			return value, nil
		}
		fmt.Fprintln(i.output, "Choose one of the installed Agent IDs shown above.")
	}
}

func isAgentChoice(id string, agents []AgentChoice) bool {
	for _, agent := range agents {
		if id == agent.ID {
			return true
		}
	}
	return false
}

type wizardInteraction struct {
	reader *bufio.Reader
	output io.Writer
}

func (i wizardInteraction) collectChatID() (int64, error) {
	for {
		value, err := i.prompt("Telegram forum group ID: ")
		if err != nil {
			return 0, err
		}
		chatID, err := strconv.ParseInt(value, 10, 64)
		if err != nil || chatID >= 0 {
			fmt.Fprintln(i.output, "A Telegram forum group ID must be a negative integer, such as -1001234567890.")
			continue
		}
		return chatID, nil
	}
}

func (i wizardInteraction) collectAllowedUserIDs() ([]int64, error) {
	for {
		value, err := i.prompt("Allowed Telegram user IDs (comma-separated): ")
		if err != nil {
			return nil, err
		}
		fields := strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t'
		})
		userIDs := make([]int64, 0, len(fields))
		valid := len(fields) > 0
		for _, field := range fields {
			userID, parseErr := strconv.ParseInt(field, 10, 64)
			if parseErr != nil {
				valid = false
				break
			}
			userIDs = append(userIDs, userID)
		}
		if valid && ValidateTelegramAllowedUserIDs(userIDs) != nil {
			valid = false
		}
		if !valid {
			fmt.Fprintln(i.output, "Enter one or more unique positive Telegram user IDs.")
			continue
		}
		return userIDs, nil
	}
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

func (i wizardInteraction) collectSecret(envName, question string) (value string, fromEnvironment bool, err error) {
	if value, ok := nonEmptyEnvironment(envName); ok {
		return value, true, nil
	}
	value, err = i.collectValue(envName, question)
	return value, false, err
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
# Agent is the installed catalog ID used for new Sessions by default.
# idle_timeout releases an Agent subprocess while keeping its Session record;
# the next Prompt resumes it automatically.
# permissions.timeout denies unanswered risky actions after the deadline.
# permissions.auto_approve lists exact Agent tool kinds, such as "read", that
# are allowed without asking; leave it empty to ask every time.
# REST listen_address selects the REST Channel's HTTP socket and defaults to
# loopback. bearer_token authenticates every endpoint except health. For
# secret-managed deployments, leave it empty and set AETHOS_REST_BEARER_TOKEN.
# AETHOS_REST_LISTEN_ADDRESS overrides the REST socket.
# Telegram bot_token authenticates the Telegram Channel. For containers and
# other secret-managed deployments, leave it empty and set
# AETHOS_TELEGRAM_BOT_TOKEN instead. AETHOS_WORKSPACE and
# AETHOS_DEFAULT_AGENT also override their corresponding values in this file.
# chat_id selects the Telegram forum supergroup. Only the allowlisted Telegram
# user IDs may interact with aethos; keep this list as small as possible.

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
