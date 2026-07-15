// Package agent spawns and drives ACP-compatible coding agents on the
// user's behalf. It is the only aethos package that imports the ACP SDK;
// everything else consumes the aethos-owned event types defined here.
package agent

import "context"

// Event is one observable occurrence streamed from an Agent during a
// prompt turn, translated from the ACP protocol into aethos vocabulary.
type Event interface{ isEvent() }

// Thought is a chunk of the agent's internal reasoning.
type Thought struct{ Text string }

// Message is a chunk of the agent's response text.
type Message struct{ Text string }

// ToolCallBegan announces a tool call the agent has initiated.
type ToolCallBegan struct {
	ID     string
	Title  string
	Kind   string // tool category as reported by the agent: read, edit, execute, …
	Status string
}

// ToolCallProgressed reports a change to an existing tool call. Empty
// fields were not part of the update.
type ToolCallProgressed struct {
	ID     string
	Title  string
	Status string
}

// PermissionOptionKind describes whether an Agent-provided choice allows or
// rejects the requested action, once or for the rest of the Session.
type PermissionOptionKind string

const (
	PermissionAllowOnce    PermissionOptionKind = "allow_once"
	PermissionAllowAlways  PermissionOptionKind = "allow_always"
	PermissionRejectOnce   PermissionOptionKind = "reject_once"
	PermissionRejectAlways PermissionOptionKind = "reject_always"
)

// PermissionOption is one Agent-provided response available to the user.
type PermissionOption struct {
	ID   string
	Name string
	Kind PermissionOptionKind
}

// PermissionRequest is an aethos-owned representation of an ACP permission
// request. Input is the Agent's structured description of the risky action.
type PermissionRequest struct {
	ToolCallID string
	Title      string
	Kind       string
	Input      any
	Options    []PermissionOption
}

// PermissionRequested is a PermissionRequest with the aethos-owned identity
// that a Channel uses to resolve it.
type PermissionRequested struct {
	ID string
	PermissionRequest
}

// PermissionDecision is the outcome returned to the Agent. A cancelled
// response is used when the surrounding Prompt ends before a user answers.
type PermissionDecision struct {
	OptionID  string
	Cancelled bool
}

// Crashed reports that the Agent connection exited unexpectedly. The Session
// manager demotes the durable Session before delivering this event.
type Crashed struct{ Error string }

func (Thought) isEvent()             {}
func (Message) isEvent()             {}
func (ToolCallBegan) isEvent()       {}
func (ToolCallProgressed) isEvent()  {}
func (PermissionRequested) isEvent() {}
func (Crashed) isEvent()             {}

// EventHandler receives every translated Event of a Conn in arrival order,
// tagged with the id of the session it belongs to. Handlers run on the
// connection's goroutine: blocking here blocks the Agent stream, and returning
// an error fails the current protocol operation.
type EventHandler func(context.Context, string, Event) error

// PermissionHandler blocks an ACP permission request until aethos's
// permission gate returns a decision.
type PermissionHandler func(context.Context, string, PermissionRequest) (PermissionDecision, error)

// Handlers are the aethos callbacks attached to one Agent connection.
type Handlers struct {
	Event      EventHandler
	Permission PermissionHandler
}

// StopReason says why an agent ended a prompt turn.
type StopReason string

const StopEndTurn StopReason = "end_turn"
