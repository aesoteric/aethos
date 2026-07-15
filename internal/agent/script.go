package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	sdk "github.com/coder/acp-go-sdk"
)

// Turn scripts one prompt turn of a scripted fake agent: the events it
// streams and how the turn ends.
type Turn struct {
	Events []Event
	Stop   StopReason
}

// Script is a canned agent performance; each Prompt consumes one Turn.
type Script struct {
	Turns []Turn
}

// ConnectScript wires a scripted fake agent — built on the SDK's agent
// side — to a Conn over in-process pipes. The fake speaks the same ACP
// protocol a real agent subprocess would, so tests driving the returned
// Conn exercise the production path end to end. This is the agent-edge
// test seam from the v1 spec; it lives here so test packages never
// import the ACP SDK themselves.
func ConnectScript(ctx context.Context, logger *slog.Logger, onEvent EventHandler, script Script) (*Conn, error) {
	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()

	fake := &scriptedAgent{script: script}
	fake.conn = sdk.NewAgentSideConnection(fake, agentToClientW, clientToAgentR)
	fake.conn.SetLogger(logger)

	shutdown := func() error {
		_ = clientToAgentW.Close()
		_ = agentToClientW.Close()
		return nil
	}
	return connect(ctx, logger, onEvent, clientToAgentW, agentToClientR, shutdown)
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
func (l *EventLog) Record(sessionID string, ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessions = append(l.sessions, sessionID)
	l.events = append(l.events, ev)
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
	script   Script
	next     int
	sessions int
}

var _ sdk.Agent = (*scriptedAgent)(nil)

func (a *scriptedAgent) Initialize(ctx context.Context, p sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{ProtocolVersion: sdk.ProtocolVersionNumber}, nil
}

func (a *scriptedAgent) NewSession(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions++
	id := fmt.Sprintf("scripted-session-%d", a.sessions)
	return sdk.NewSessionResponse{SessionId: sdk.SessionId(id)}, nil
}

func (a *scriptedAgent) Prompt(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	a.mu.Lock()
	if a.next >= len(a.script.Turns) {
		prompt := a.next + 1
		a.mu.Unlock()
		return sdk.PromptResponse{}, fmt.Errorf("scripted agent: no turn scripted for prompt %d", prompt)
	}
	turn := a.script.Turns[a.next]
	a.next++
	a.mu.Unlock()

	for _, ev := range turn.Events {
		if err := a.conn.SessionUpdate(ctx, sdk.SessionNotification{
			SessionId: p.SessionId,
			Update:    untranslate(ev),
		}); err != nil {
			return sdk.PromptResponse{}, err
		}
	}
	return sdk.PromptResponse{StopReason: sdk.StopReason(turn.Stop)}, nil
}

// untranslate is the inverse of translate: it lets the scripted fake
// speak real protocol from events scripted in aethos vocabulary.
func untranslate(ev Event) sdk.SessionUpdate {
	switch e := ev.(type) {
	case Thought:
		return sdk.UpdateAgentThoughtText(e.Text)
	case Message:
		return sdk.UpdateAgentMessageText(e.Text)
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
	return nil
}

func (a *scriptedAgent) CloseSession(ctx context.Context, p sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionClose)
}

func (a *scriptedAgent) ListSessions(ctx context.Context, p sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionList)
}

func (a *scriptedAgent) ResumeSession(ctx context.Context, p sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	return sdk.ResumeSessionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionResume)
}

func (a *scriptedAgent) SetSessionConfigOption(ctx context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	return sdk.SetSessionConfigOptionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionSetConfigOption)
}

func (a *scriptedAgent) SetSessionMode(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionSetMode)
}
