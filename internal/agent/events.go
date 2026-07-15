// Package agent spawns and drives ACP-compatible coding agents on the
// user's behalf. It is the only aethos package that imports the ACP SDK;
// everything else consumes the aethos-owned event types defined here.
package agent

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

func (Thought) isEvent()            {}
func (Message) isEvent()            {}
func (ToolCallBegan) isEvent()      {}
func (ToolCallProgressed) isEvent() {}

// EventHandler receives every translated Event of a Conn in arrival
// order, tagged with the id of the session it belongs to. Handlers run
// on the connection's goroutine: blocking here blocks the agent stream.
type EventHandler func(sessionID string, ev Event)

// StopReason says why an agent ended a prompt turn.
type StopReason string

const StopEndTurn StopReason = "end_turn"
