package main

import (
	"fmt"
	"io"

	"github.com/aesoteric/aethos/internal/agent"
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

func (r *renderer) render(sessionID string, ev agent.Event) {
	switch e := ev.(type) {
	case agent.Thought:
		if r.mode != modeThought {
			r.breakLine()
			fmt.Fprint(r.w, "[thinking] ")
			r.mode = modeThought
		}
		fmt.Fprint(r.w, e.Text)
	case agent.Message:
		if r.mode != modeMessage {
			r.breakLine()
			r.mode = modeMessage
		}
		fmt.Fprint(r.w, e.Text)
	case agent.ToolCallBegan:
		r.breakLine()
		fmt.Fprintf(r.w, "[tool] %s%s\n", e.Title, toolMeta(e.Kind, e.Status))
	case agent.ToolCallProgressed:
		r.breakLine()
		line := "[tool] " + e.ID
		if e.Status != "" {
			line += " → " + e.Status
		}
		if e.Title != "" {
			line += " (" + e.Title + ")"
		}
		fmt.Fprintln(r.w, line)
	}
}

// finish closes any text still streaming so the shell prompt lands on a
// fresh line.
func (r *renderer) finish() {
	r.breakLine()
}

func (r *renderer) breakLine() {
	if r.mode != modeNone {
		fmt.Fprintln(r.w)
		r.mode = modeNone
	}
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
