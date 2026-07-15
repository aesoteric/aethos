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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/session"
)

// usage deliberately omits the hidden dev command.
const usage = `aethos — self-hosted bridge from ACP coding agents to messaging platforms

No user-facing commands are available yet; the first release is under
construction. See https://github.com/aesoteric/aethos.
`

// errUsage signals that usage help was printed and no further error
// output is wanted.
var errUsage = errors.New("usage shown")

func main() {
	logger := newLogger(os.Stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, logger, os.Args[1:], os.Stdout, os.Stderr)
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

func run(ctx context.Context, logger *slog.Logger, args []string, stdout, stderr io.Writer) error {
	if len(args) >= 2 && args[0] == "dev" && args[1] == "prompt" {
		return devPrompt(ctx, logger, args[2:], stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return errUsage
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
