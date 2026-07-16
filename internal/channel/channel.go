// Package channel defines the seam through which user-facing Channels receive
// aethos events. Telegram and REST/SSE will implement this interface; Memory is
// the human-edge test fake described by the v1 specification.
package channel

import (
	"context"
	"fmt"
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

// OwnerLookup returns the owning Channel name for a Session.
type OwnerLookup func(context.Context, string) (string, error)

// Router sends each Session event only to the Channel recorded as its owner.
type Router struct {
	owner  OwnerLookup
	routes map[string]Channel
}

// NewRouter builds an immutable owner-based Channel router.
func NewRouter(owner OwnerLookup, routes map[string]Channel) (*Router, error) {
	if owner == nil {
		return nil, fmt.Errorf("session owner lookup is required")
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("at least one Channel route is required")
	}
	copied := make(map[string]Channel, len(routes))
	for name, target := range routes {
		if name == "" || target == nil {
			return nil, fmt.Errorf("Channel route name and target are required")
		}
		copied[name] = target
	}
	return &Router{owner: owner, routes: copied}, nil
}

// Send implements Channel.
func (r *Router) Send(ctx context.Context, event Event) error {
	owner, err := r.owner(ctx, event.SessionID)
	if err != nil {
		return err
	}
	target := r.routes[owner]
	if target == nil {
		return fmt.Errorf("no Channel registered for Session owner %q", owner)
	}
	return target.Send(ctx, event)
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

// PermissionResponse is an inbound Channel answer to a pending permission
// request.
type PermissionResponse struct {
	RequestID string
	OptionID  string
}

// PermissionTarget accepts answers to pending permission requests.
type PermissionTarget interface {
	ResolvePermission(context.Context, string, string) error
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

// InjectPermission resolves a pending permission request through the human-edge
// seam.
func (m *Memory) InjectPermission(ctx context.Context, target PermissionTarget, response PermissionResponse) error {
	return target.ResolvePermission(ctx, response.RequestID, response.OptionID)
}

// Snapshot returns a copy of every recorded event in delivery order.
func (m *Memory) Snapshot() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}
