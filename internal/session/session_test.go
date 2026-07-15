package session_test

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/session"
)

func TestSessionPromptFlowsThroughAgentToObservedEvents(t *testing.T) {
	scripted := []agent.Event{
		agent.Thought{Text: "reading the request"},
		agent.Message{Text: "done"},
	}
	script := agent.Script{Turns: []agent.Turn{{Events: scripted, Stop: agent.StopEndTurn}}}

	log := &agent.EventLog{}
	conn, err := agent.ConnectScript(t.Context(), slog.New(slog.DiscardHandler), log.Record, script)
	if err != nil {
		t.Fatalf("ConnectScript: %v", err)
	}
	defer conn.Close()

	workspace := t.TempDir()
	sess, err := session.New(t.Context(), conn, workspace)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	if sess.ID() == "" {
		t.Error("session has an empty id")
	}
	if sess.Workspace() != workspace {
		t.Errorf("Workspace() = %q, want %q", sess.Workspace(), workspace)
	}

	stop, err := sess.Prompt(t.Context(), "please do the thing")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != agent.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", stop, agent.StopEndTurn)
	}

	if _, got := log.Snapshot(); !reflect.DeepEqual(got, scripted) {
		t.Errorf("observed events = %#v, want %#v", got, scripted)
	}
}

func TestSessionNewRequiresAbsoluteWorkspace(t *testing.T) {
	conn, err := agent.ConnectScript(t.Context(), slog.New(slog.DiscardHandler),
		func(string, agent.Event) {}, agent.Script{})
	if err != nil {
		t.Fatalf("ConnectScript: %v", err)
	}
	defer conn.Close()

	if _, err := session.New(t.Context(), conn, "relative/path"); err == nil {
		t.Error("session.New accepted a relative Workspace path, want error")
	}
}
