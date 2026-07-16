// Package sessionstate defines the shared durable lifecycle states of a
// Session without coupling the Session manager to a user-facing Channel.
package sessionstate

// State is the durable lifecycle state of a Session.
type State string

const (
	// Live means an Agent connection is attached to the Session.
	Live State = "live"
	// Dormant means the record is durable but no Agent connection is attached.
	Dormant State = "dormant"
	// Closed means the Session was deliberately archived and cannot resume.
	Closed State = "closed"
)

// CanTransitionTo reports whether next is a valid lifecycle transition from
// state. Creation enters Live directly; Closed is terminal.
func (state State) CanTransitionTo(next State) bool {
	switch state {
	case Live:
		return next == Dormant || next == Closed
	case Dormant:
		return next == Live || next == Closed
	case Closed:
		return false
	default:
		return false
	}
}
