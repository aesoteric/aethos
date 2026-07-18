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
	"text/tabwriter"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/agentcatalog"
	channeltypes "github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/config"
	"github.com/aesoteric/aethos/internal/rest"
	"github.com/aesoteric/aethos/internal/session"
	"github.com/aesoteric/aethos/internal/telegram"
)

// usage deliberately omits the hidden dev command.
const usage = `aethos — self-hosted bridge from ACP coding agents to messaging platforms

usage: aethos [-data-dir <directory>]
       aethos version
       aethos agents [-data-dir <directory>]
       aethos agents install [-data-dir <directory>] <agent-id>

Install an Agent before first run; aethos then creates a commented config.toml
with an interactive setup wizard. The data directory defaults to ~/.aethos and
can also be set with AETHOS_DATA_DIR.
`

// Release builds replace these values through linker flags. Development builds
// retain explicit identities so their output cannot be mistaken for a release.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

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
	registryClient := &http.Client{Timeout: 30 * time.Second}
	return runWithRegistry(
		ctx, logger, args, stdin, stdout, stderr, telegramClient,
		agentcatalog.NewRegistry("", registryClient),
	)
}

func runWithRegistry(
	ctx context.Context,
	logger *slog.Logger,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	telegramClient *telegram.Client,
	registry *agentcatalog.Registry,
) error {
	if len(args) == 1 && args[0] == "version" {
		if _, err := fmt.Fprintf(stdout, "aethos %s (commit %s, built %s)\n", version, commit, buildDate); err != nil {
			return fmt.Errorf("write version: %w", err)
		}
		return nil
	}
	if len(args) >= 2 && args[0] == "dev" && args[1] == "prompt" {
		return devPrompt(ctx, logger, args[2:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "agents" {
		return agentsCommand(ctx, args[1:], stdout, stderr, registry)
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
	catalog, err := agentcatalog.Open(paths.AgentCatalogFile, paths.AgentsDir)
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
		installed, installedErr := catalog.Installed()
		if installedErr != nil {
			return installedErr
		}
		if len(installed) == 0 {
			return fmt.Errorf("start first-run setup: no Agents are installed; run aethos agents install <agent-id> first")
		}
		choices := make([]config.AgentChoice, 0, len(installed))
		for _, installedAgent := range installed {
			choices = append(choices, config.AgentChoice{
				ID: installedAgent.ID, Name: installedAgent.Name, Type: string(installedAgent.Type),
			})
		}
		configured, err = config.RunWizard(ctx, stdin, stdout, paths, telegramClient, choices)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("inspect config %q: %w", paths.ConfigFile, err)
	}
	if _, err := catalog.Resolve(configured.DefaultAgent); err != nil {
		return fmt.Errorf(
			"default Agent %q is not installed; run aethos agents install %s: %w",
			configured.DefaultAgent, configured.DefaultAgent, err,
		)
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory %q: %w", paths.DataDir, err)
	}

	routes := make(map[string]channeltypes.Channel)
	runners := make([]channelRunner, 0, 2)
	var bridge *telegram.Channel
	if configured.Telegram != nil {
		if telegramClient == nil {
			return fmt.Errorf("start Telegram Channel: client is unavailable")
		}
		bridge, err = telegram.Open(
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
				Agents:         catalog,
			},
		)
		if err != nil {
			return err
		}
		routes["telegram"] = bridge
		runners = append(runners, func(runCtx context.Context, manager *session.Manager) error {
			return bridge.Run(runCtx, manager)
		})
	}
	if configured.REST != nil {
		api, restErr := rest.New(rest.Settings{
			ListenAddress: configured.REST.ListenAddress,
			BearerToken:   configured.REST.BearerToken,
			Identity:      "api",
			Agents:        catalog,
		}, logger)
		if restErr != nil {
			return errors.Join(restErr, closeTelegram(bridge))
		}
		routes["rest"] = api
		runners = append(runners, func(runCtx context.Context, manager *session.Manager) error {
			return api.Run(runCtx, manager)
		})
	}
	if len(runners) == 0 {
		// The Slack runtime arrives in issue #19. Until then, a valid Slack-only
		// deployment remains live without constructing an unconfigured Channel.
		<-ctx.Done()
		return nil
	}
	var manager *session.Manager
	events, err := channeltypes.NewRouter(
		func(eventCtx context.Context, sessionID string) (string, error) {
			record, lookupErr := manager.Get(eventCtx, sessionID)
			return record.Owner.Channel, lookupErr
		},
		routes,
	)
	if err != nil {
		return errors.Join(err, closeTelegram(bridge))
	}
	manager, err = session.Open(
		ctx,
		paths.DatabaseFile,
		agentConnector(logger, catalog),
		events,
		session.WithIdleTimeout(time.Duration(configured.IdleTimeout)),
		session.WithPermissionTimeout(time.Duration(configured.Permissions.Timeout)),
		session.WithAutoApprove(configured.Permissions.AutoApprove...),
	)
	if err != nil {
		return errors.Join(err, closeTelegram(bridge))
	}
	runErr := runChannels(ctx, manager, runners...)
	return errors.Join(runErr, manager.Close(), closeTelegram(bridge))
}

func agentsCommand(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	registry *agentcatalog.Registry,
) error {
	if registry == nil {
		return fmt.Errorf("ACP Agent registry client is unavailable")
	}
	if len(args) > 0 && args[0] == "install" {
		return installAgentCommand(ctx, args[1:], stdout, stderr, registry)
	}
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDirFlag := fs.String("data-dir", "", "directory containing the local Agent catalog")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aethos agents [-data-dir <directory>]")
		return errUsage
	}
	if _, err := config.ResolveDataDir(*dataDirFlag); err != nil {
		return err
	}
	agents, err := registry.List(ctx)
	if err != nil {
		return err
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ID\tNAME\tTYPE\tDESCRIPTION"); err != nil {
		return fmt.Errorf("write Agent list: %w", err)
	}
	for _, registryAgent := range agents {
		if _, err := fmt.Fprintf(
			table, "%s\t%s\t%s\t%s\n",
			registryAgent.ID, registryAgent.Name, registryAgent.Type(), registryAgent.Description,
		); err != nil {
			return fmt.Errorf("write Agent list: %w", err)
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write Agent list: %w", err)
	}
	return nil
}

func installAgentCommand(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	registry *agentcatalog.Registry,
) error {
	fs := flag.NewFlagSet("agents install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDirFlag := fs.String("data-dir", "", "directory containing the local Agent catalog")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: aethos agents install [-data-dir <directory>] <agent-id>")
		return errUsage
	}
	agentID := fs.Arg(0)
	agents, err := registry.List(ctx)
	if err != nil {
		return err
	}
	var selected *agentcatalog.RegistryAgent
	for index := range agents {
		if agents[index].ID == agentID {
			selected = &agents[index]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("agent %q was not found in the ACP registry", agentID)
	}
	dataDir, err := config.ResolveDataDir(*dataDirFlag)
	if err != nil {
		return err
	}
	paths, err := config.NewPaths(dataDir)
	if err != nil {
		return err
	}
	catalog, err := agentcatalog.Open(paths.AgentCatalogFile, paths.AgentsDir)
	if err != nil {
		return err
	}
	installed, err := catalog.Install(ctx, *selected, nil)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Installed %s (%s) via %s.\n", installed.Name, installed.ID, installed.Type); err != nil {
		return fmt.Errorf("write Agent installation result: %w", err)
	}
	return nil
}

type channelRunner func(context.Context, *session.Manager) error

func runChannels(ctx context.Context, manager *session.Manager, runners ...channelRunner) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(runners))
	for _, run := range runners {
		go func() { results <- run(runCtx, manager) }()
	}
	errs := make([]error, 0, len(runners))
	errs = append(errs, <-results)
	cancel()
	for range len(runners) - 1 {
		errs = append(errs, <-results)
	}
	return errors.Join(errs...)
}

func closeTelegram(bridge *telegram.Channel) error {
	if bridge == nil {
		return nil
	}
	return bridge.Close()
}

// devPrompt is the hidden tracer-bullet command: spawn a locally
// installed ACP agent, open a Session, dispatch one Prompt, and stream
// the agent's output to stdout as it happens.
func devPrompt(ctx context.Context, logger *slog.Logger, args []string, stdout, stderr io.Writer) error {
	_, err := devPromptWithConnector(ctx, logger, args, stdout, stderr, directAgentConnector(logger))
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
			Agent:     session.AgentRef(*agentCmd),
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

func agentConnector(logger *slog.Logger, catalog *agentcatalog.Catalog) session.Connect {
	return func(ctx context.Context, ref session.AgentRef, handlers agent.Handlers) (*agent.Conn, error) {
		if catalog == nil {
			return nil, fmt.Errorf("agent catalog is unavailable")
		}
		installed, err := catalog.Resolve(string(ref))
		if err != nil {
			return nil, err
		}
		return agent.SpawnLaunch(ctx, logger, handlers, installed.Launch)
	}
}

func directAgentConnector(logger *slog.Logger) session.Connect {
	return func(ctx context.Context, ref session.AgentRef, handlers agent.Handlers) (*agent.Conn, error) {
		args := strings.Fields(string(ref))
		if len(args) == 0 {
			return nil, fmt.Errorf("agent command is empty")
		}
		return agent.Spawn(ctx, logger, handlers, args[0], args[1:]...)
	}
}
