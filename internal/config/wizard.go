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

// TelegramTokenValidator checks a Telegram bot token at Telegram's protocol edge.
type TelegramTokenValidator interface {
	ValidateToken(context.Context, string) error
}

// SlackTokenValidator checks Slack credentials at Slack's protocol edge.
type SlackTokenValidator interface {
	ValidateAppToken(context.Context, string) error
	ValidateBotToken(context.Context, string) error
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
	telegramValidator TelegramTokenValidator,
	slackValidator SlackTokenValidator,
	agents []AgentChoice,
) (Config, error) {
	interaction := wizardInteraction{reader: bufio.NewReader(input), output: output}
	fmt.Fprintln(output, "No config.toml found. Let's set up aethos.")

	channels, err := interaction.collectChannels()
	if err != nil {
		return Config{}, err
	}
	var telegramConfig *Telegram
	var telegramTokenFromEnvironment bool
	if channels.telegram {
		if telegramValidator == nil {
			return Config{}, fmt.Errorf("Telegram token validator is required")
		}
		token, fromEnvironment, collectErr := interaction.collectTelegramToken(ctx, telegramValidator)
		if collectErr != nil {
			return Config{}, collectErr
		}
		chatID, collectErr := interaction.collectTelegramChatID()
		if collectErr != nil {
			return Config{}, collectErr
		}
		allowedUserIDs, collectErr := interaction.collectTelegramAllowedUserIDs()
		if collectErr != nil {
			return Config{}, collectErr
		}
		telegramConfig = &Telegram{
			BotToken: token, ChatID: chatID, AllowedUserIDs: allowedUserIDs,
		}
		telegramTokenFromEnvironment = fromEnvironment
	}

	var slackConfig *Slack
	var slackAppTokenFromEnvironment, slackBotTokenFromEnvironment bool
	if channels.slack {
		if slackValidator == nil {
			return Config{}, fmt.Errorf("Slack token validator is required")
		}
		appToken, fromEnvironment, collectErr := interaction.collectSlackToken(
			ctx, slackAppTokenEnv, "Slack app-level token: ", "Slack app-level token", "xapp-",
			slackValidator.ValidateAppToken,
		)
		if collectErr != nil {
			return Config{}, collectErr
		}
		slackAppTokenFromEnvironment = fromEnvironment
		botToken, fromEnvironment, collectErr := interaction.collectSlackToken(
			ctx, slackBotTokenEnv, "Slack bot token: ", "Slack bot token", "xoxb-",
			slackValidator.ValidateBotToken,
		)
		if collectErr != nil {
			return Config{}, collectErr
		}
		slackBotTokenFromEnvironment = fromEnvironment
		channelID, collectErr := interaction.collectSlackChannelID()
		if collectErr != nil {
			return Config{}, collectErr
		}
		allowedUserIDs, collectErr := interaction.collectSlackAllowedUserIDs()
		if collectErr != nil {
			return Config{}, collectErr
		}
		slackConfig = &Slack{
			AppToken: appToken, BotToken: botToken, ChannelID: channelID, AllowedUserIDs: allowedUserIDs,
		}
	}

	workspace, err := interaction.collectWorkspace()
	if err != nil {
		return Config{}, err
	}
	defaultAgent, err := interaction.collectAgent(agents)
	if err != nil {
		return Config{}, err
	}

	effective := defaultConfig()
	effective.Workspace = workspace
	effective.DefaultAgent = defaultAgent
	effective.Telegram = telegramConfig
	effective.Slack = slackConfig
	if err := effective.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate setup: %w", err)
	}

	persisted := effective
	if telegramTokenFromEnvironment {
		persisted.Telegram.BotToken = ""
	}
	if slackAppTokenFromEnvironment {
		persisted.Slack.AppToken = ""
	}
	if slackBotTokenFromEnvironment {
		persisted.Slack.BotToken = ""
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

type selectedChannels struct {
	telegram bool
	slack    bool
}

func (i wizardInteraction) collectChannels() (selectedChannels, error) {
	fmt.Fprintln(i.output, "Channels:")
	fmt.Fprintln(i.output, "  1 — Telegram")
	fmt.Fprintln(i.output, "  2 — Slack")
	fmt.Fprintln(i.output, "  3 — Telegram and Slack")
	for {
		value, err := i.prompt("Choose Channels [1-3]: ")
		if err != nil {
			return selectedChannels{}, err
		}
		switch value {
		case "1":
			return selectedChannels{telegram: true}, nil
		case "2":
			return selectedChannels{slack: true}, nil
		case "3":
			return selectedChannels{telegram: true, slack: true}, nil
		default:
			fmt.Fprintln(i.output, "Choose 1 for Telegram, 2 for Slack, or 3 for both Channels.")
		}
	}
}

func (i wizardInteraction) collectTelegramChatID() (int64, error) {
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

func (i wizardInteraction) collectTelegramAllowedUserIDs() ([]int64, error) {
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

func (i wizardInteraction) collectTelegramToken(
	ctx context.Context,
	validator TelegramTokenValidator,
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

func (i wizardInteraction) collectSlackToken(
	ctx context.Context,
	envName, question, label, prefix string,
	validate func(context.Context, string) error,
) (token string, fromEnvironment bool, err error) {
	if value, ok := nonEmptyEnvironment(envName); ok {
		fmt.Fprintf(i.output, "Validating %s from %s...\n", label, envName)
		if !validSlackToken(value, prefix) {
			return "", false, fmt.Errorf("%s is not a valid %s", envName, label)
		}
		if err := validate(ctx, value); err != nil {
			return "", false, fmt.Errorf("%s is not a valid %s: %w", envName, label, err)
		}
		return value, true, nil
	}

	for {
		value, err := i.prompt(question)
		if err != nil {
			return "", false, err
		}
		if !validSlackToken(value, prefix) {
			fmt.Fprintf(i.output, "%s must start with %s and contain no spaces. Try again.\n", label, prefix)
			continue
		}
		if err := validate(ctx, value); err != nil {
			fmt.Fprintf(i.output, "%s rejected: %v. Try again.\n", label, err)
			continue
		}
		return value, false, nil
	}
}

func (i wizardInteraction) collectSlackChannelID() (string, error) {
	for {
		value, err := i.prompt("Slack channel ID: ")
		if err != nil {
			return "", err
		}
		if !validSlackChannelID(value) {
			fmt.Fprintln(i.output, "A Slack channel ID must begin with C or G and contain no spaces.")
			continue
		}
		return value, nil
	}
}

func (i wizardInteraction) collectSlackAllowedUserIDs() ([]string, error) {
	for {
		value, err := i.prompt("Allowed Slack user IDs (comma-separated): ")
		if err != nil {
			return nil, err
		}
		userIDs := strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t'
		})
		if validateSlackAllowedUserIDs(userIDs) != nil {
			fmt.Fprintln(i.output, "Enter one or more unique Slack user IDs.")
			continue
		}
		return userIDs, nil
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
# Slack app_token opens Socket Mode and bot_token authenticates Web API calls.
# For secret-managed deployments, leave either token empty and set
# AETHOS_SLACK_APP_TOKEN and AETHOS_SLACK_BOT_TOKEN instead. channel_id selects
# the Slack channel whose top level is the Assistant; only the allowlisted
# Slack user IDs may interact with aethos.
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
