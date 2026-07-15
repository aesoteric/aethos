// Command aethos is a self-hosted bridge from ACP coding agents to
// messaging platforms: one static binary, one data directory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/config"
	"github.com/aesoteric/aethos/internal/session"
	"github.com/aesoteric/aethos/internal/telegram"
)

// usage deliberately omits the hidden dev command.
const usage = `aethos — self-hosted bridge from ACP coding agents to messaging platforms

usage: aethos [-data-dir <directory>]

On first run, aethos creates a commented config.toml with an interactive
setup wizard. The data directory defaults to ~/.aethos and can also be set
with AETHOS_DATA_DIR.
`

// errUsage signals that usage help was printed and no further error
// output is wanted.
var errUsage = errors.New("usage shown")

func main() {
	logger := newLogger(os.Stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	telegramClient := telegram.NewClient(telegram.APIBaseURL, &http.Client{Timeout: 15 * time.Second})
	err := run(ctx, logger, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, telegramClient)
	switch {
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		logger.Error("aethos failed", "error", err)
		os.Exit(1)
	}
}

// newLogger builds the structured JSON logger; AETHOS_LOG_LEVEL
// (debug|info|warn|error) selects the level, defaulting to info.
func newLogger(w io.Writer) *slog.Logger {
	var level slog.Level
	raw := os.Getenv("AETHOS_LOG_LEVEL")
	err := level.UnmarshalText([]byte(raw))
	if err != nil {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
	if err != nil && raw != "" {
		logger.Warn("ignoring invalid AETHOS_LOG_LEVEL", "value", raw)
	}
	return logger
}

func run(
	ctx context.Context,
	logger *slog.Logger,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	telegramClient *telegram.Client,
) error {
	if len(args) >= 2 && args[0] == "dev" && args[1] == "prompt" {
		return devPrompt(ctx, logger, args[2:], stdout, stderr)
	}

	fs := flag.NewFlagSet("aethos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	dataDirFlag := fs.String("data-dir", "", "directory containing aethos config, database, and logs")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errUsage
	}

	dataDir, err := config.ResolveDataDir(*dataDirFlag)
	if err != nil {
		return err
	}
	paths, err := config.NewPaths(dataDir)
	if err != nil {
		return err
	}

	var configured config.Config
	_, err = os.Stat(paths.ConfigFile)
	switch {
	case err == nil:
		configured, err = config.Load(paths.ConfigFile)
		if err != nil {
			return err
		}
	case errors.Is(err, os.ErrNotExist):
		if telegramClient == nil {
			return fmt.Errorf("start first-run setup: Telegram token validator is unavailable")
		}
		configured, err = config.RunWizard(ctx, stdin, stdout, paths, telegramClient)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("inspect config %q: %w", paths.ConfigFile, err)
	}
	if telegramClient == nil {
		return fmt.Errorf("start Telegram Channel: client is unavailable")
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory %q: %w", paths.DataDir, err)
	}

	bridge, err := telegram.Open(
		ctx,
		paths.DatabaseFile,
		telegramClient,
		logger,
		telegram.Settings{
			Token:          configured.Telegram.BotToken,
			ChatID:         configured.Telegram.ChatID,
			AllowedUserIDs: configured.Telegram.AllowedUserIDs,
			DefaultAgent:   configured.DefaultAgent,
			Workspace:      configured.Workspace,
		},
	)
	if err != nil {
		return err
	}
	manager, err := session.Open(
		ctx,
		paths.DatabaseFile,
		agentConnector(logger),
		bridge,
		session.WithIdleTimeout(time.Duration(configured.IdleTimeout)),
		session.WithPermissionTimeout(time.Duration(configured.Permissions.Timeout)),
		session.WithAutoApprove(configured.Permissions.AutoApprove...),
	)
	if err != nil {
		return errors.Join(err, bridge.Close())
	}
	runErr := bridge.Run(ctx, manager)
	return errors.Join(runErr, manager.Close(), bridge.Close())
}

// devPrompt is the hidden tracer-bullet command: spawn a locally
// installed ACP agent, open a Session, dispatch one Prompt, and stream
// the agent's output to stdout as it happens.
func devPrompt(ctx context.Context, logger *slog.Logger, args []string, stdout, stderr io.Writer) error {
	_, err := devPromptWithConnector(ctx, logger, args, stdout, stderr, agentConnector(logger))
	return err
}

func devPromptWithConnector(
	ctx context.Context,
	logger *slog.Logger,
	args []string,
	stdout, stderr io.Writer,
	connect session.Connect,
) (sessionID string, returnErr error) {
	fs := flag.NewFlagSet("dev prompt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentCmd := fs.String("agent", "", `agent command to spawn, e.g. "npx @zed-industries/claude-code-acp"`)
	workspace := fs.String("workspace", ".", "Workspace directory for the Session")
	dataDirFlag := fs.String("data-dir", "", "directory containing the durable Session database")
	resumeID := fs.String("session", "", "existing Session to resume")
	if err := fs.Parse(args); err != nil {
		return "", errUsage
	}

	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" || (*resumeID == "" && len(strings.Fields(*agentCmd)) == 0) {
		fmt.Fprintln(stderr, `usage: aethos dev prompt [-data-dir <dir>] (-agent <command> [-workspace <dir>] | -session <id>) <prompt text>`)
		return "", errUsage
	}

	dataDir, err := config.ResolveDataDir(*dataDirFlag)
	if err != nil {
		return "", err
	}
	paths, err := config.NewPaths(dataDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return "", fmt.Errorf("create data directory %q: %w", paths.DataDir, err)
	}
	idleTimeout := config.DefaultIdleTimeout
	var managerOptions []session.Option
	configured, configErr := config.Load(paths.ConfigFile)
	switch {
	case configErr == nil:
		idleTimeout = time.Duration(configured.IdleTimeout)
		managerOptions = append(
			managerOptions,
			session.WithPermissionTimeout(time.Duration(configured.Permissions.Timeout)),
			session.WithAutoApprove(configured.Permissions.AutoApprove...),
		)
	case errors.Is(configErr, os.ErrNotExist):
	default:
		return "", configErr
	}

	r := &renderer{w: stdout}
	managerOptions = append(managerOptions, session.WithIdleTimeout(idleTimeout))
	manager, err := session.Open(
		ctx,
		paths.DatabaseFile,
		connect,
		r,
		managerOptions...,
	)
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, manager.Close()) }()

	var record session.Record
	if *resumeID != "" {
		record, err = manager.Get(ctx, *resumeID)
	} else {
		ws, resolveErr := filepath.Abs(*workspace)
		if resolveErr != nil {
			return "", fmt.Errorf("resolve workspace: %w", resolveErr)
		}
		record, err = manager.Create(ctx, session.Create{
			Agent:     *agentCmd,
			Workspace: ws,
			Owner:     session.Owner{Channel: "dev", ID: "local"},
		})
	}
	if err != nil {
		return "", err
	}
	logger.Info("session opened", "session", record.ID, "workspace", record.Workspace, "agent", record.Agent)

	stop, err := manager.Prompt(ctx, record.ID, text)
	if err != nil {
		return "", err
	}
	if err := r.finish(); err != nil {
		return "", fmt.Errorf("finish Agent output: %w", err)
	}
	logger.Info("turn ended", "session", record.ID, "stop", string(stop))
	return record.ID, nil
}

func agentConnector(logger *slog.Logger) session.Connect {
	return func(ctx context.Context, command string, handlers agent.Handlers) (*agent.Conn, error) {
		args := strings.Fields(command)
		if len(args) == 0 {
			return nil, fmt.Errorf("agent command is empty")
		}
		return agent.Spawn(ctx, logger, handlers, args[0], args[1:]...)
	}
}
