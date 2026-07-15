package main

import (
	"strings"
	"testing"

	"github.com/aesoteric/aethos/internal/agent"
)

func TestRendererStreamsEventsReadably(t *testing.T) {
	tests := []struct {
		name   string
		events []agent.Event
		want   string
	}{
		{
			name: "full turn: thought, tool call, message",
			events: []agent.Event{
				agent.Thought{Text: "I should "},
				agent.Thought{Text: "read the file"},
				agent.ToolCallBegan{ID: "call-1", Title: "Read main.go", Kind: "read", Status: "in_progress"},
				agent.ToolCallProgressed{ID: "call-1", Status: "completed"},
				agent.Message{Text: "The file "},
				agent.Message{Text: "looks fine."},
			},
			want: "[thinking] I should read the file\n" +
				"[tool] Read main.go (read, in_progress)\n" +
				"[tool] call-1 → completed\n" +
				"The file looks fine.\n",
		},
		{
			name: "message chunks stream without decoration",
			events: []agent.Event{
				agent.Message{Text: "Hello"},
				agent.Message{Text: ", world"},
			},
			want: "Hello, world\n",
		},
		{
			name: "tool call without kind or status has no parens",
			events: []agent.Event{
				agent.ToolCallBegan{ID: "call-9", Title: "Do something"},
			},
			want: "[tool] Do something\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			r := &renderer{w: &out}
			for _, ev := range tt.events {
				r.render("session-1", ev)
			}
			r.finish()
			if out.String() != tt.want {
				t.Errorf("rendered output:\n%q\nwant:\n%q", out.String(), tt.want)
			}
		})
	}
}
