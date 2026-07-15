package main

import (
	"context"
	"fmt"
	"io"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
)

const (
	modeNone    = ""
	modeThought = "thought"
	modeMessage = "message"
)

// renderer formats a Session's event stream for the dev prompt command.
// It tracks which kind of text is mid-stream so labels appear once per
// block instead of once per chunk.
type renderer struct {
	w    io.Writer
	mode string
}

// Send implements channel.Channel.
func (r *renderer) Send(ctx context.Context, event channel.Event) error {
	return r.render(ctx, event.SessionID, event.AgentEvent)
}

func (r *renderer) render(_ context.Context, _ string, ev agent.Event) error {
	switch e := ev.(type) {
	case agent.Thought:
		if r.mode != modeThought {
			if err := r.breakLine(); err != nil {
				return err
			}
			if _, err := fmt.Fprint(r.w, "[thinking] "); err != nil {
				return err
			}
			r.mode = modeThought
		}
		_, err := fmt.Fprint(r.w, e.Text)
		return err
	case agent.Message:
		if r.mode != modeMessage {
			if err := r.breakLine(); err != nil {
				return err
			}
			r.mode = modeMessage
		}
		_, err := fmt.Fprint(r.w, e.Text)
		return err
	case agent.ToolCallBegan:
		if err := r.breakLine(); err != nil {
			return err
		}
		_, err := fmt.Fprintf(r.w, "[tool] %s%s\n", e.Title, toolMeta(e.Kind, e.Status))
		return err
	case agent.ToolCallProgressed:
		if err := r.breakLine(); err != nil {
			return err
		}
		line := "[tool] " + e.ID
		if e.Status != "" {
			line += " → " + e.Status
		}
		if e.Title != "" {
			line += " (" + e.Title + ")"
		}
		_, err := fmt.Fprintln(r.w, line)
		return err
	case agent.Crashed:
		if err := r.breakLine(); err != nil {
			return err
		}
		_, err := fmt.Fprintf(r.w, "[agent crashed] %s\n", e.Error)
		return err
	}
	return nil
}

// finish closes any text still streaming so the shell prompt lands on a
// fresh line.
func (r *renderer) finish() error {
	return r.breakLine()
}

func (r *renderer) breakLine() error {
	if r.mode != modeNone {
		if _, err := fmt.Fprintln(r.w); err != nil {
			return err
		}
		r.mode = modeNone
	}
	return nil
}

func toolMeta(kind, status string) string {
	switch {
	case kind != "" && status != "":
		return " (" + kind + ", " + status + ")"
	case kind != "":
		return " (" + kind + ")"
	case status != "":
		return " (" + status + ")"
	}
	return ""
}
