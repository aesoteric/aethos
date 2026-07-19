package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// Turn scripts one prompt turn of a scripted fake agent: the events it
// streams and how the turn ends.
type Turn struct {
	WantPrompt  string
	WantHistory []string
	Started     chan<- struct{}
	Continue    <-chan struct{}
	Permissions []ScriptedPermission
	Events      []Event
	// EventInterval spaces scripted events so flow tests can observe streaming
	// behavior at a real Channel's write boundary.
	EventInterval time.Duration
	Stop          StopReason
	Crash         bool
}

// ScriptedPermission asks the real ACP client for permission during a scripted
// turn and verifies the response observed by the fake Agent.
type ScriptedPermission struct {
	Request       PermissionRequest
	WantOptionID  string
	WantCancelled bool
}

// Script is a canned Agent performance shared across connections. Sharing is
// deliberate: it models an Agent's durable Session context across restarts.
type Script struct {
	Turns []Turn
	// Exit simulates the scripted Agent process exiting independently of a
	// Prompt. Sending an error tears down the active scripted connection.
	Exit <-chan error

	mu       sync.Mutex
	next     int
	sessions int
	history  map[string][]string
}

// ConnectScript wires a scripted fake agent — built on the SDK's agent
// side — to a Conn over in-process pipes. The fake speaks the same ACP
// protocol a real agent subprocess would, so tests driving the returned
// Conn exercise the production path end to end. This is the agent-edge
// test seam from the v1 spec; it lives here so test packages never
// import the ACP SDK themselves.
func ConnectScript(
	ctx context.Context,
	logger *slog.Logger,
	handlers Handlers,
	script *Script,
) (*Conn, error) {
	if script == nil {
		return nil, fmt.Errorf("script is required")
	}
	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()

	fake := &scriptedAgent{
		script:   script,
		attached: make(map[string]bool),
		active:   make(map[string]chan struct{}),
	}
	lifecycle := newConnectionLifecycle()
	crash := func(err error) {
		if err == nil {
			err = errors.New("scripted Agent subprocess exited")
		}
		clientCloseErr := clientToAgentW.CloseWithError(err)
		agentCloseErr := agentToClientW.CloseWithError(err)
		lifecycle.finish(errors.Join(err, clientCloseErr, agentCloseErr))
	}
	fake.crash = crash
	if script.Exit != nil {
		go func() {
			select {
			case err, ok := <-script.Exit:
				if ok {
					crash(err)
				}
			case <-lifecycle.done:
			}
		}()
	}
	fake.conn = sdk.NewAgentSideConnection(fake, agentToClientW, clientToAgentR)
	fake.conn.SetLogger(logger)

	shutdown := func() error {
		err := errors.Join(clientToAgentW.Close(), agentToClientW.Close())
		lifecycle.finish(nil)
		return err
	}
	return connect(ctx, logger, handlers, clientToAgentW, agentToClientR, shutdown, lifecycle)
}

// ServeScript runs the scripted ACP Agent on a subprocess-style stdio edge.
// It lets integration tests exercise the real spawn path without a live Agent.
func ServeScript(
	ctx context.Context,
	logger *slog.Logger,
	script *Script,
	input io.Reader,
	output io.Writer,
) error {
	if script == nil {
		return fmt.Errorf("script is required")
	}
	fake := &scriptedAgent{
		script:   script,
		attached: make(map[string]bool),
		active:   make(map[string]chan struct{}),
	}
	fake.crash = func(error) {}
	fake.conn = sdk.NewAgentSideConnection(fake, output, input)
	fake.conn.SetLogger(logger)
	select {
	case <-fake.conn.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EventLog is a race-safe event collector for flow tests: pass Record as
// the EventHandler, drive the Conn, then assert on Snapshot. It belongs
// to the same agent-edge test harness as Script.
type EventLog struct {
	mu       sync.Mutex
	sessions []string
	events   []Event
}

// Record implements EventHandler.
func (l *EventLog) Record(_ context.Context, sessionID string, ev Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessions = append(l.sessions, sessionID)
	l.events = append(l.events, ev)
	return nil
}

// Snapshot returns copies of the recorded session ids and events, in
// delivery order.
func (l *EventLog) Snapshot() (sessions []string, events []Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.sessions...), append([]Event(nil), l.events...)
}

// scriptedAgent implements the SDK's agent side by replaying a Script.
type scriptedAgent struct {
	conn *sdk.AgentSideConnection

	mu       sync.Mutex
	script   *Script
	attached map[string]bool
	active   map[string]chan struct{}
	crash    func(error)
}

var _ sdk.Agent = (*scriptedAgent)(nil)

func (a *scriptedAgent) Initialize(ctx context.Context, p sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		AgentCapabilities: sdk.AgentCapabilities{
			SessionCapabilities: sdk.SessionCapabilities{Resume: &sdk.SessionResumeCapabilities{}},
		},
	}, nil
}

func (a *scriptedAgent) NewSession(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	a.script.mu.Lock()
	a.script.sessions++
	id := fmt.Sprintf("scripted-session-%d", a.script.sessions)
	if a.script.history == nil {
		a.script.history = make(map[string][]string)
	}
	a.script.history[id] = nil
	a.script.mu.Unlock()

	a.mu.Lock()
	a.attached[id] = true
	a.mu.Unlock()
	return sdk.NewSessionResponse{SessionId: sdk.SessionId(id)}, nil
}

func (a *scriptedAgent) Prompt(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	sessionID := string(p.SessionId)
	a.mu.Lock()
	attached := a.attached[sessionID]
	a.mu.Unlock()
	if !attached {
		return sdk.PromptResponse{}, fmt.Errorf("scripted agent: session %q is not attached", sessionID)
	}

	text := promptText(p.Prompt)
	a.script.mu.Lock()
	if a.script.next >= len(a.script.Turns) {
		prompt := a.script.next + 1
		a.script.mu.Unlock()
		return sdk.PromptResponse{}, fmt.Errorf("scripted agent: no turn scripted for prompt %d", prompt)
	}
	turn := a.script.Turns[a.script.next]
	a.script.next++
	a.script.history[sessionID] = append(a.script.history[sessionID], text)
	history := append([]string(nil), a.script.history[sessionID]...)
	a.script.mu.Unlock()

	if turn.WantPrompt != "" && text != turn.WantPrompt {
		return sdk.PromptResponse{}, fmt.Errorf("scripted agent: Prompt = %q, want %q", text, turn.WantPrompt)
	}
	if turn.WantHistory != nil && !slices.Equal(history, turn.WantHistory) {
		return sdk.PromptResponse{}, fmt.Errorf("scripted agent: history = %q, want %q", history, turn.WantHistory)
	}

	cancelled := make(chan struct{})
	a.mu.Lock()
	a.active[sessionID] = cancelled
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.active[sessionID] == cancelled {
			delete(a.active, sessionID)
		}
		a.mu.Unlock()
	}()
	if turn.Started != nil {
		select {
		case turn.Started <- struct{}{}:
		case <-ctx.Done():
			return sdk.PromptResponse{}, ctx.Err()
		case <-cancelled:
			return sdk.PromptResponse{}, context.Canceled
		}
	}
	if turn.Continue != nil {
		select {
		case <-turn.Continue:
		case <-ctx.Done():
			return sdk.PromptResponse{}, ctx.Err()
		case <-cancelled:
			return sdk.PromptResponse{}, context.Canceled
		}
	}
	if turn.Crash {
		err := errors.New("scripted Agent subprocess crashed")
		a.crash(err)
		return sdk.PromptResponse{}, err
	}
	for _, permission := range turn.Permissions {
		response, err := a.conn.RequestPermission(ctx, scriptPermissionRequest(p.SessionId, permission.Request))
		if err != nil {
			return sdk.PromptResponse{}, fmt.Errorf("scripted Agent permission request: %w", err)
		}
		if permission.WantCancelled {
			if response.Outcome.Cancelled == nil {
				return sdk.PromptResponse{}, fmt.Errorf("scripted Agent permission outcome was selected, want cancelled")
			}
			continue
		}
		if response.Outcome.Selected == nil {
			return sdk.PromptResponse{}, fmt.Errorf("scripted Agent permission outcome was cancelled, want option %q", permission.WantOptionID)
		}
		if got := string(response.Outcome.Selected.OptionId); got != permission.WantOptionID {
			return sdk.PromptResponse{}, fmt.Errorf("scripted Agent permission option = %q, want %q", got, permission.WantOptionID)
		}
	}

	for index, ev := range turn.Events {
		if err := a.conn.SessionUpdate(ctx, sdk.SessionNotification{
			SessionId: p.SessionId,
			Update:    untranslate(ev),
		}); err != nil {
			return sdk.PromptResponse{}, err
		}
		if turn.EventInterval > 0 && index+1 < len(turn.Events) {
			timer := time.NewTimer(turn.EventInterval)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return sdk.PromptResponse{}, ctx.Err()
			case <-cancelled:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return sdk.PromptResponse{}, context.Canceled
			}
		}
	}
	return sdk.PromptResponse{StopReason: sdk.StopReason(turn.Stop)}, nil
}

func scriptPermissionRequest(sessionID sdk.SessionId, request PermissionRequest) sdk.RequestPermissionRequest {
	toolCall := sdk.ToolCallUpdate{
		ToolCallId: sdk.ToolCallId(request.ToolCallID),
		RawInput:   request.Input,
	}
	if request.Title != "" {
		toolCall.Title = sdk.Ptr(request.Title)
	}
	if request.Kind != "" {
		toolCall.Kind = sdk.Ptr(sdk.ToolKind(request.Kind))
	}
	options := make([]sdk.PermissionOption, 0, len(request.Options))
	for _, option := range request.Options {
		options = append(options, sdk.PermissionOption{
			OptionId: sdk.PermissionOptionId(option.ID),
			Name:     option.Name,
			Kind:     sdk.PermissionOptionKind(option.Kind),
		})
	}
	return sdk.RequestPermissionRequest{
		SessionId: sessionID,
		ToolCall:  toolCall,
		Options:   options,
	}
}

func promptText(blocks []sdk.ContentBlock) string {
	var text string
	for _, block := range blocks {
		text += contentText(block)
	}
	return text
}

// untranslate is the inverse of translate: it lets the scripted fake
// speak real protocol from events scripted in aethos vocabulary.
func untranslate(ev Event) sdk.SessionUpdate {
	switch e := ev.(type) {
	case Thought:
		return sdk.UpdateAgentThoughtText(e.Text)
	case Message:
		return sdk.UpdateAgentMessageText(e.Text)
	case SessionRenamed:
		return sdk.SessionUpdate{SessionInfoUpdate: &sdk.SessionSessionInfoUpdate{
			SessionUpdate: "session_info_update",
			Title:         sdk.Ptr(e.Name),
		}}
	case ToolCallBegan:
		var opts []sdk.ToolCallStartOpt
		if e.Kind != "" {
			opts = append(opts, sdk.WithStartKind(sdk.ToolKind(e.Kind)))
		}
		if e.Status != "" {
			opts = append(opts, sdk.WithStartStatus(sdk.ToolCallStatus(e.Status)))
		}
		return sdk.StartToolCall(sdk.ToolCallId(e.ID), e.Title, opts...)
	case ToolCallProgressed:
		var opts []sdk.ToolCallUpdateOpt
		if e.Title != "" {
			opts = append(opts, sdk.WithUpdateTitle(e.Title))
		}
		if e.Status != "" {
			opts = append(opts, sdk.WithUpdateStatus(sdk.ToolCallStatus(e.Status)))
		}
		return sdk.UpdateToolCall(sdk.ToolCallId(e.ID), opts...)
	}
	panic(fmt.Sprintf("scripted agent cannot express event type %T", ev))
}

func (a *scriptedAgent) Authenticate(ctx context.Context, p sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodAuthenticate)
}

func (a *scriptedAgent) Logout(ctx context.Context, p sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodLogout)
}

func (a *scriptedAgent) Cancel(ctx context.Context, p sdk.CancelNotification) error {
	id := string(p.SessionId)
	a.mu.Lock()
	cancelled := a.active[id]
	if cancelled != nil {
		delete(a.active, id)
	}
	a.mu.Unlock()
	if cancelled != nil {
		close(cancelled)
	}
	return nil
}

func (a *scriptedAgent) CloseSession(ctx context.Context, p sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionClose)
}

func (a *scriptedAgent) ListSessions(ctx context.Context, p sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionList)
}

func (a *scriptedAgent) ResumeSession(ctx context.Context, p sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	id := string(p.SessionId)
	a.script.mu.Lock()
	_, exists := a.script.history[id]
	a.script.mu.Unlock()
	if !exists {
		return sdk.ResumeSessionResponse{}, fmt.Errorf("scripted agent: session %q does not exist", id)
	}
	a.mu.Lock()
	a.attached[id] = true
	a.mu.Unlock()
	return sdk.ResumeSessionResponse{}, nil
}

func (a *scriptedAgent) SetSessionConfigOption(ctx context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	return sdk.SetSessionConfigOptionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionSetConfigOption)
}

func (a *scriptedAgent) SetSessionMode(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionSetMode)
}
