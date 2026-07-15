// Package channel defines the seam through which user-facing Channels receive
// aethos events. Telegram and REST/SSE will implement this interface; Memory is
// the human-edge test fake described by the v1 specification.
package channel

import (
	"context"
	"sync"

	"github.com/aesoteric/aethos/internal/agent"
)

// Event is an observable Agent event addressed to an aethos Session.
type Event struct {
	SessionID  string
	AgentEvent agent.Event
}

// Channel delivers user-visible events for Sessions.
type Channel interface {
	Send(context.Context, Event) error
}

// Prompt is inbound text addressed to an existing Session.
type Prompt struct {
	SessionID string
	Text      string
}

// PromptTarget accepts inbound Prompts from a Channel.
type PromptTarget interface {
	Prompt(context.Context, string, string) (agent.StopReason, error)
}

// Memory is an in-memory Channel for flow tests.
type Memory struct {
	mu     sync.Mutex
	events []Event
}

// Send records an event in delivery order.
func (m *Memory) Send(_ context.Context, event Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	return nil
}

// Inject delivers an inbound Prompt through the human-edge seam.
func (m *Memory) Inject(ctx context.Context, target PromptTarget, prompt Prompt) (agent.StopReason, error) {
	return target.Prompt(ctx, prompt.SessionID, prompt.Text)
}

// Snapshot returns a copy of every recorded event in delivery order.
func (m *Memory) Snapshot() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}
