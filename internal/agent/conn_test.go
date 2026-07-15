package agent_test

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/aesoteric/aethos/internal/agent"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestPromptStreamsScriptedEventsInOrder(t *testing.T) {
	scripted := []agent.Event{
		agent.Thought{Text: "planning the change"},
		agent.ToolCallBegan{ID: "call-1", Title: "Read main.go", Kind: "read", Status: "in_progress"},
		agent.ToolCallProgressed{ID: "call-1", Status: "completed"},
		agent.Message{Text: "Here is "},
		agent.Message{Text: "the answer."},
	}
	script := agent.Script{Turns: []agent.Turn{{Events: scripted, Stop: agent.StopEndTurn}}}

	log := &agent.EventLog{}
	conn, err := agent.ConnectScript(t.Context(), discardLogger(), log.Record, script)
	if err != nil {
		t.Fatalf("ConnectScript: %v", err)
	}
	defer conn.Close()

	sessionID, err := conn.NewSession(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sessionID == "" {
		t.Fatal("NewSession returned an empty session id")
	}

	stop, err := conn.Prompt(t.Context(), sessionID, "hello")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != agent.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", stop, agent.StopEndTurn)
	}

	sessions, events := log.Snapshot()
	if !reflect.DeepEqual(events, scripted) {
		t.Errorf("observed events do not match the script\n got: %#v\nwant: %#v", events, scripted)
	}
	for i, sid := range sessions {
		if sid != sessionID {
			t.Errorf("event %d delivered for session %q, want %q", i, sid, sessionID)
		}
	}
}

func TestEachPromptConsumesOneScriptedTurn(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{
		{Events: []agent.Event{agent.Message{Text: "first turn"}}, Stop: agent.StopEndTurn},
		{Events: []agent.Event{agent.Message{Text: "second turn"}}, Stop: agent.StopEndTurn},
	}}

	log := &agent.EventLog{}
	conn, err := agent.ConnectScript(t.Context(), discardLogger(), log.Record, script)
	if err != nil {
		t.Fatalf("ConnectScript: %v", err)
	}
	defer conn.Close()

	sessionID, err := conn.NewSession(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	for turn := 0; turn < 2; turn++ {
		if _, err := conn.Prompt(t.Context(), sessionID, "go"); err != nil {
			t.Fatalf("Prompt %d: %v", turn+1, err)
		}
	}

	_, events := log.Snapshot()
	want := []agent.Event{
		agent.Message{Text: "first turn"},
		agent.Message{Text: "second turn"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("observed events = %#v, want %#v", events, want)
	}
}
