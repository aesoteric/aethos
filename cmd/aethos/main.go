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
	tokenValidator config.TokenValidator,
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

	_, err = os.Stat(paths.ConfigFile)
	switch {
	case err == nil:
		_, err := config.Load(paths.ConfigFile)
		return err
	case errors.Is(err, os.ErrNotExist):
		if tokenValidator == nil {
			return fmt.Errorf("start first-run setup: Telegram token validator is unavailable")
		}
		_, err := config.RunWizard(ctx, stdin, stdout, paths, tokenValidator)
		return err
	default:
		return fmt.Errorf("inspect config %q: %w", paths.ConfigFile, err)
	}
}

// devPrompt is the hidden tracer-bullet command: spawn a locally
// installed ACP agent, open a Session, dispatch one Prompt, and stream
// the agent's output to stdout as it happens.
func devPrompt(ctx context.Context, logger *slog.Logger, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dev prompt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentCmd := fs.String("agent", "", `agent command to spawn, e.g. "npx @zed-industries/claude-code-acp"`)
	workspace := fs.String("workspace", ".", "Workspace directory for the Session")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	argv := strings.Fields(*agentCmd)
	if len(argv) == 0 || text == "" {
		fmt.Fprintln(stderr, `usage: aethos dev prompt -agent <command> [-workspace <dir>] <prompt text>`)
		return errUsage
	}

	ws, err := filepath.Abs(*workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	r := &renderer{w: stdout}
	conn, err := agent.Spawn(ctx, logger, r.render, argv[0], argv[1:]...)
	if err != nil {
		return err
	}
	defer conn.Close()

	sess, err := session.New(ctx, conn, ws)
	if err != nil {
		return err
	}
	logger.Info("session opened", "session", sess.ID(), "workspace", ws, "agent", *agentCmd)

	stop, err := sess.Prompt(ctx, text)
	if err != nil {
		return err
	}
	r.finish()
	logger.Info("turn ended", "session", sess.ID(), "stop", string(stop))
	return nil
}
