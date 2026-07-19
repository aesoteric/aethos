package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	sdk "github.com/coder/acp-go-sdk"
)

// Conn is one live connection to a running Agent, either spawned as a
// subprocess (Spawn) or wired over in-process pipes (ConnectScript).
type Conn struct {
	sdk          *sdk.ClientSideConnection
	capabilities sdk.AgentCapabilities
	logger       *slog.Logger
	handlers     Handlers
	shutdown     func() error
	lifecycle    *connectionLifecycle
	closeOnce    sync.Once
	closeErr     error
	eventMu      sync.Mutex
	eventErrors  map[string]error
}

// Launch is the complete subprocess definition for an installed Agent.
type Launch struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Clone returns a launch definition whose mutable fields do not alias the
// original.
func (launch Launch) Clone() Launch {
	launch.Args = append([]string(nil), launch.Args...)
	if launch.Env != nil {
		environment := make(map[string]string, len(launch.Env))
		for key, value := range launch.Env {
			environment[key] = value
		}
		launch.Env = environment
	}
	return launch
}

type connectionLifecycle struct {
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func newConnectionLifecycle() *connectionLifecycle {
	return &connectionLifecycle{done: make(chan struct{})}
}

func (l *connectionLifecycle) finish(err error) {
	l.once.Do(func() {
		l.mu.Lock()
		l.err = err
		l.mu.Unlock()
		close(l.done)
	})
}

func (l *connectionLifecycle) exitError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Spawn launches an ACP agent subprocess, connects over its stdio, and
// performs the protocol handshake. The agent's stderr is forwarded to
// the logger at debug level.
func Spawn(
	ctx context.Context,
	logger *slog.Logger,
	handlers Handlers,
	name string,
	args ...string,
) (*Conn, error) {
	return SpawnLaunch(ctx, logger, handlers, Launch{Command: name, Args: args})
}

// SpawnLaunch launches an ACP Agent from one typed launch definition.
func SpawnLaunch(
	ctx context.Context,
	logger *slog.Logger,
	handlers Handlers,
	launch Launch,
) (*Conn, error) {
	// Conn owns the subprocess lifetime. The caller's context bounds startup and
	// protocol operations; Close is the single place that terminates the Agent.
	cmd := exec.Command(launch.Command, launch.Args...)
	if len(launch.Env) > 0 {
		keys := make([]string, 0, len(launch.Env))
		for key := range launch.Env {
			if key == "" || strings.ContainsRune(key, '=') {
				return nil, fmt.Errorf("invalid Agent environment variable name %q", key)
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		base := os.Environ()
		cmd.Env = make([]string, 0, len(base)+len(keys))
		for _, assignment := range base {
			key, _, _ := strings.Cut(assignment, "=")
			if _, overridden := launch.Env[key]; !overridden {
				cmd.Env = append(cmd.Env, assignment)
			}
		}
		for _, key := range keys {
			cmd.Env = append(cmd.Env, key+"="+launch.Env[key])
		}
	}
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
		return nil, fmt.Errorf("spawn agent %q: %w", launch.Command, err)
	}
	lifecycle := newConnectionLifecycle()
	go func() {
		lifecycle.finish(cmd.Wait())
	}()
	// Teardown errors are discarded deliberately: killing the agent makes
	// Wait report "signal: killed" on every clean shutdown.
	shutdown := func() error {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-lifecycle.done
		return nil
	}
	return connect(ctx, logger, handlers, stdin, stdout, shutdown, lifecycle)
}

// connect wires the client side of an ACP connection over the given
// pipes and performs the initialize handshake.
func connect(
	ctx context.Context,
	logger *slog.Logger,
	handlers Handlers,
	peerIn io.Writer,
	peerOut io.Reader,
	shutdown func() error,
	lifecycle *connectionLifecycle,
) (*Conn, error) {
	c := &Conn{logger: logger, handlers: handlers, shutdown: shutdown, lifecycle: lifecycle}
	c.sdk = sdk.NewClientSideConnection(&client{conn: c}, peerIn, peerOut)
	c.sdk.SetLogger(logger)
	initialized, err := c.sdk.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientInfo:      &sdk.Implementation{Name: "aethos", Version: "dev"},
	})
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("initialize Agent: %w", err),
			wrapError("close Agent", c.Close()),
		)
	}
	c.capabilities = initialized.AgentCapabilities
	return c, nil
}

// Done closes when the Agent connection exits, either unexpectedly or after
// Close. ExitError reports the process or transport error, if any.
func (c *Conn) Done() <-chan struct{} { return c.lifecycle.done }

// ExitError returns the error that ended the Agent connection. Callers should
// wait for Done before reading it.
func (c *Conn) ExitError() error { return c.lifecycle.exitError() }

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

// ResumeSession attaches this connection to an Agent Session created by an
// earlier Agent process. The non-replaying ACP resume method is preferred;
// Agents that only implement the older load capability use that instead.
func (c *Conn) ResumeSession(ctx context.Context, sessionID, workspace string) error {
	switch {
	case c.capabilities.SessionCapabilities.Resume != nil:
		_, err := c.sdk.ResumeSession(ctx, sdk.ResumeSessionRequest{
			SessionId:  sdk.SessionId(sessionID),
			Cwd:        workspace,
			McpServers: []sdk.McpServer{},
		})
		if err != nil {
			return fmt.Errorf("resume session: %w", err)
		}
		return nil
	case c.capabilities.LoadSession:
		_, err := c.sdk.LoadSession(ctx, sdk.LoadSessionRequest{
			SessionId:  sdk.SessionId(sessionID),
			Cwd:        workspace,
			McpServers: []sdk.McpServer{},
		})
		if err != nil {
			return fmt.Errorf("load session: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("resume session: Agent supports neither session/resume nor session/load")
	}
}

// Prompt dispatches one Prompt to a session and blocks until the turn
// ends. Events stream to the EventHandler while it runs; the SDK
// guarantees every event sent before the turn ended has been handled by
// the time Prompt returns.
func (c *Conn) Prompt(ctx context.Context, sessionID, text string) (StopReason, error) {
	c.clearEventError(sessionID)
	resp, err := c.sdk.Prompt(ctx, sdk.PromptRequest{
		SessionId: sdk.SessionId(sessionID),
		Prompt:    []sdk.ContentBlock{sdk.TextBlock(text)},
	})
	eventErr := c.takeEventError(sessionID)
	if err != nil || eventErr != nil {
		return "", errors.Join(wrapError("prompt", err), eventErr)
	}
	return StopReason(resp.StopReason), nil
}

// Cancel asks the Agent to stop the Prompt currently running in sessionID.
func (c *Conn) Cancel(ctx context.Context, sessionID string) error {
	if err := c.sdk.Cancel(ctx, sdk.CancelNotification{SessionId: sdk.SessionId(sessionID)}); err != nil {
		return fmt.Errorf("cancel prompt: %w", err)
	}
	return nil
}

func (c *Conn) clearEventError(sessionID string) {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	delete(c.eventErrors, sessionID)
}

func (c *Conn) recordEventError(sessionID string, err error) {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	if c.eventErrors == nil {
		c.eventErrors = make(map[string]error)
	}
	c.eventErrors[sessionID] = errors.Join(c.eventErrors[sessionID], err)
}

func (c *Conn) takeEventError(sessionID string) error {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	err := c.eventErrors[sessionID]
	delete(c.eventErrors, sessionID)
	return err
}

func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// Close tears down the connection and, for spawned agents, the subprocess.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		if c.shutdown != nil {
			c.closeErr = c.shutdown()
		}
	})
	return c.closeErr
}

// client receives the agent's ACP callbacks and translates session
// updates into aethos events.
type client struct{ conn *Conn }

var _ sdk.Client = (*client)(nil)

func (cl *client) SessionUpdate(ctx context.Context, n sdk.SessionNotification) error {
	if ev, ok := translate(n.Update); ok {
		sessionID := string(n.SessionId)
		if cl.conn.handlers.Event == nil {
			return nil
		}
		if err := cl.conn.handlers.Event(ctx, sessionID, ev); err != nil {
			cl.conn.recordEventError(sessionID, err)
		}
	}
	return nil
}

func (cl *client) RequestPermission(ctx context.Context, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	request := translatePermissionRequest(p)
	handler := cl.conn.handlers.Permission
	if handler == nil {
		handler = rejectPermission
	}
	response, err := handler(ctx, string(p.SessionId), request)
	if err != nil {
		return sdk.RequestPermissionResponse{}, err
	}
	if response.Cancelled || response.OptionID == "" {
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}, nil
	}
	return sdk.RequestPermissionResponse{
		Outcome: sdk.NewRequestPermissionOutcomeSelected(sdk.PermissionOptionId(response.OptionID)),
	}, nil
}

func translatePermissionRequest(p sdk.RequestPermissionRequest) PermissionRequest {
	request := PermissionRequest{
		ToolCallID: string(p.ToolCall.ToolCallId),
		Input:      p.ToolCall.RawInput,
		Options:    make([]PermissionOption, 0, len(p.Options)),
	}
	if p.ToolCall.Title != nil {
		request.Title = *p.ToolCall.Title
	}
	if p.ToolCall.Kind != nil {
		request.Kind = string(*p.ToolCall.Kind)
	}
	for _, option := range p.Options {
		request.Options = append(request.Options, PermissionOption{
			ID:   string(option.OptionId),
			Name: option.Name,
			Kind: PermissionOptionKind(option.Kind),
		})
	}
	return request
}

func rejectPermission(_ context.Context, _ string, request PermissionRequest) (PermissionDecision, error) {
	for _, option := range request.Options {
		if option.Kind == PermissionRejectOnce || option.Kind == PermissionRejectAlways {
			return PermissionDecision{OptionID: option.ID}, nil
		}
	}
	return PermissionDecision{Cancelled: true}, nil
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
	case u.SessionInfoUpdate != nil && u.SessionInfoUpdate.Title != nil:
		title := strings.TrimSpace(*u.SessionInfoUpdate.Title)
		if title == "" {
			return nil, false
		}
		return SessionInfoUpdated{Title: title}, true
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
