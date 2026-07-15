package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"

	sdk "github.com/coder/acp-go-sdk"
)

// Conn is one live connection to a running Agent, either spawned as a
// subprocess (Spawn) or wired over in-process pipes (ConnectScript).
type Conn struct {
	sdk      *sdk.ClientSideConnection
	logger   *slog.Logger
	onEvent  EventHandler
	shutdown func() error
}

// Spawn launches an ACP agent subprocess, connects over its stdio, and
// performs the protocol handshake. The agent's stderr is forwarded to
// the logger at debug level.
func Spawn(ctx context.Context, logger *slog.Logger, onEvent EventHandler, name string, args ...string) (*Conn, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("agent stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("agent stdout: %w", err)
	}
	cmd.Stderr = &logWriter{logger: logger}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn agent %q: %w", name, err)
	}
	// Teardown errors are discarded deliberately: killing the agent makes
	// Wait report "signal: killed" on every clean shutdown.
	shutdown := func() error {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return nil
	}
	return connect(ctx, logger, onEvent, stdin, stdout, shutdown)
}

// connect wires the client side of an ACP connection over the given
// pipes and performs the initialize handshake.
func connect(ctx context.Context, logger *slog.Logger, onEvent EventHandler, peerIn io.Writer, peerOut io.Reader, shutdown func() error) (*Conn, error) {
	c := &Conn{logger: logger, onEvent: onEvent, shutdown: shutdown}
	c.sdk = sdk.NewClientSideConnection(&client{conn: c}, peerIn, peerOut)
	c.sdk.SetLogger(logger)
	if _, err := c.sdk.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientInfo:      &sdk.Implementation{Name: "aethos", Version: "dev"},
	}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize agent: %w", err)
	}
	return c, nil
}

// NewSession opens a new ACP session rooted at the Workspace directory,
// which must be an absolute path.
func (c *Conn) NewSession(ctx context.Context, workspace string) (string, error) {
	resp, err := c.sdk.NewSession(ctx, sdk.NewSessionRequest{
		Cwd:        workspace,
		McpServers: []sdk.McpServer{},
	})
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	return string(resp.SessionId), nil
}

// Prompt dispatches one Prompt to a session and blocks until the turn
// ends. Events stream to the EventHandler while it runs; the SDK
// guarantees every event sent before the turn ended has been handled by
// the time Prompt returns.
func (c *Conn) Prompt(ctx context.Context, sessionID, text string) (StopReason, error) {
	resp, err := c.sdk.Prompt(ctx, sdk.PromptRequest{
		SessionId: sdk.SessionId(sessionID),
		Prompt:    []sdk.ContentBlock{sdk.TextBlock(text)},
	})
	if err != nil {
		return "", fmt.Errorf("prompt: %w", err)
	}
	return StopReason(resp.StopReason), nil
}

// Close tears down the connection and, for spawned agents, the subprocess.
func (c *Conn) Close() error {
	if c.shutdown == nil {
		return nil
	}
	return c.shutdown()
}

// client receives the agent's ACP callbacks and translates session
// updates into aethos events.
type client struct{ conn *Conn }

var _ sdk.Client = (*client)(nil)

func (cl *client) SessionUpdate(ctx context.Context, n sdk.SessionNotification) error {
	if ev, ok := translate(n.Update); ok {
		cl.conn.onEvent(string(n.SessionId), ev)
	}
	return nil
}

// RequestPermission answers with a rejection: the permission gate is a
// later Module, and until it exists the fail-safe default is denial.
func (cl *client) RequestPermission(ctx context.Context, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	cl.conn.logger.Warn("denying permission request: no permission gate yet",
		"session", p.SessionId, "tool", p.ToolCall.ToolCallId)
	for _, opt := range p.Options {
		if opt.Kind == sdk.PermissionOptionKindRejectOnce {
			return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{
				Selected: &sdk.RequestPermissionOutcomeSelected{OptionId: opt.OptionId},
			}}, nil
		}
	}
	return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{
		Cancelled: &sdk.RequestPermissionOutcomeCancelled{},
	}}, nil
}

// aethos advertises no fs or terminal capabilities, so a conforming
// agent never calls the methods below.

func (cl *client) ReadTextFile(ctx context.Context, p sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodFsReadTextFile)
}

func (cl *client) WriteTextFile(ctx context.Context, p sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodFsWriteTextFile)
}

func (cl *client) CreateTerminal(ctx context.Context, p sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalCreate)
}

func (cl *client) KillTerminal(ctx context.Context, p sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalKill)
}

func (cl *client) TerminalOutput(ctx context.Context, p sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalOutput)
}

func (cl *client) ReleaseTerminal(ctx context.Context, p sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalRelease)
}

func (cl *client) WaitForTerminalExit(ctx context.Context, p sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalWaitForExit)
}

// translate converts one ACP session update into an aethos Event.
// Updates aethos doesn't model yet (plans, mode changes, …) return ok=false.
func translate(u sdk.SessionUpdate) (Event, bool) {
	switch {
	case u.AgentThoughtChunk != nil:
		return Thought{Text: contentText(u.AgentThoughtChunk.Content)}, true
	case u.AgentMessageChunk != nil:
		return Message{Text: contentText(u.AgentMessageChunk.Content)}, true
	case u.ToolCall != nil:
		return ToolCallBegan{
			ID:     string(u.ToolCall.ToolCallId),
			Title:  u.ToolCall.Title,
			Kind:   string(u.ToolCall.Kind),
			Status: string(u.ToolCall.Status),
		}, true
	case u.ToolCallUpdate != nil:
		ev := ToolCallProgressed{ID: string(u.ToolCallUpdate.ToolCallId)}
		if u.ToolCallUpdate.Title != nil {
			ev.Title = *u.ToolCallUpdate.Title
		}
		if u.ToolCallUpdate.Status != nil {
			ev.Status = string(*u.ToolCallUpdate.Status)
		}
		return ev, true
	}
	return nil, false
}

// contentText extracts a content block's text; non-text blocks (images,
// resources) have no aethos rendering yet.
func contentText(c sdk.ContentBlock) string {
	if c.Text != nil {
		return c.Text.Text
	}
	return ""
}

// logWriter forwards an agent subprocess's stderr to structured logging.
type logWriter struct{ logger *slog.Logger }

func (w *logWriter) Write(p []byte) (int, error) {
	if line := strings.TrimSpace(string(p)); line != "" {
		w.logger.Debug("agent stderr", "output", line)
	}
	return len(p), nil
}
