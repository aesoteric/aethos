// Package session holds aethos Sessions: one conversation between a
// user and one Agent, bound to a Workspace. This walking-skeleton
// version is minimal and in-memory; persistence, serial prompt queuing,
// and the live/dormant state machine arrive with later tickets.
package session

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/aesoteric/aethos/internal/agent"
)

// Session is one conversation between a user and one Agent.
type Session struct {
	id        string
	workspace string
	conn      *agent.Conn
}

// New opens a Session on conn rooted at the Workspace directory, which
// must be an absolute path.
func New(ctx context.Context, conn *agent.Conn, workspace string) (*Session, error) {
	if !filepath.IsAbs(workspace) {
		return nil, fmt.Errorf("workspace must be an absolute path, got %q", workspace)
	}
	id, err := conn.NewSession(ctx, workspace)
	if err != nil {
		return nil, err
	}
	return &Session{id: id, workspace: workspace, conn: conn}, nil
}

// ID is the session's identity as issued by the agent.
func (s *Session) ID() string { return s.id }

// Workspace is the directory the Session's agent reads and writes.
func (s *Session) Workspace() string { return s.workspace }

// Prompt dispatches one Prompt to the Session's agent and blocks until
// the turn ends, returning why the agent stopped.
func (s *Session) Prompt(ctx context.Context, text string) (agent.StopReason, error) {
	return s.conn.Prompt(ctx, s.id, text)
}
