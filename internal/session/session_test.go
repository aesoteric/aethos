package session_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/session"
)

func TestSessionRecordSurvivesShutdownAsDormant(t *testing.T) {
	script := agent.Script{}
	connect := scriptedConnector(&script)
	database := t.TempDir() + "/aethos.db"

	manager, err := session.Open(t.Context(), database, connect, &channel.Memory{})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	created, err := manager.Create(t.Context(), session.Create{
		Agent:     "codex-acp",
		Workspace: t.TempDir(),
		Owner:     session.Owner{Channel: "telegram", ID: "42"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := session.Open(t.Context(), database, connect, &channel.Memory{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
	if got.Agent != "codex-acp" {
		t.Errorf("Agent = %q, want codex-acp", got.Agent)
	}
	if got.Workspace != created.Workspace {
		t.Errorf("Workspace = %q, want %q", got.Workspace, created.Workspace)
	}
	if got.Owner != (session.Owner{Channel: "telegram", ID: "42"}) {
		t.Errorf("Owner = %#v, want Telegram user 42", got.Owner)
	}
	if got.State != session.Dormant {
		t.Errorf("State = %q, want %q", got.State, session.Dormant)
	}
	if got.CreatedAt.IsZero() || got.LastActivityAt.IsZero() {
		t.Errorf("timestamps were not persisted: created=%v last_activity=%v", got.CreatedAt, got.LastActivityAt)
	}
}

func TestPromptToDormantSessionResumesWithFullContext(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{
		{
			WantPrompt:  "remember that the answer is blue",
			WantHistory: []string{"remember that the answer is blue"},
			Events:      []agent.Event{agent.Message{Text: "I'll remember."}},
			Stop:        agent.StopEndTurn,
		},
		{
			WantPrompt:  "what is the answer?",
			WantHistory: []string{"remember that the answer is blue", "what is the answer?"},
			Events:      []agent.Event{agent.Message{Text: "blue"}},
			Stop:        agent.StopEndTurn,
		},
	}}
	connect := scriptedConnector(&script)
	database := t.TempDir() + "/aethos.db"
	firstChannel := &channel.Memory{}

	first, err := session.Open(t.Context(), database, connect, firstChannel)
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	record, err := first.Create(t.Context(), session.Create{
		Agent:     "codex-acp",
		Workspace: t.TempDir(),
		Owner:     session.Owner{Channel: "telegram", ID: "42"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := firstChannel.Inject(t.Context(), first, channel.Prompt{
		SessionID: record.ID,
		Text:      "remember that the answer is blue",
	}); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	secondChannel := &channel.Memory{}
	second, err := session.Open(t.Context(), database, connect, secondChannel)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	stop, err := secondChannel.Inject(t.Context(), second, channel.Prompt{
		SessionID: record.ID,
		Text:      "what is the answer?",
	})
	if err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	if stop != agent.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", stop, agent.StopEndTurn)
	}
	got := secondChannel.Snapshot()
	want := []channel.Event{{SessionID: record.ID, AgentEvent: agent.Message{Text: "blue"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events after resume = %#v, want %#v", got, want)
	}
	resumed, err := second.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get resumed Session: %v", err)
	}
	if resumed.State != session.Live {
		t.Errorf("State = %q, want %q", resumed.State, session.Live)
	}
}

func TestConcurrentPromptsRunInArrivalOrder(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	script := agent.Script{Turns: []agent.Turn{
		{WantPrompt: "first", Started: firstStarted, Continue: releaseFirst, Events: []agent.Event{agent.Message{Text: "1"}}, Stop: agent.StopEndTurn},
		{WantPrompt: "second", Events: []agent.Event{agent.Message{Text: "2"}}, Stop: agent.StopEndTurn},
		{WantPrompt: "third", Events: []agent.Event{agent.Message{Text: "3"}}, Stop: agent.StopEndTurn},
		{WantPrompt: "fourth", Events: []agent.Event{agent.Message{Text: "4"}}, Stop: agent.StopEndTurn},
	}}
	connect := scriptedConnector(&script)
	memory := &channel.Memory{}
	manager, err := session.Open(t.Context(), t.TempDir()+"/aethos.db", connect, memory)
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	defer manager.Close()
	record, err := manager.Create(t.Context(), session.Create{
		Agent: "codex-acp", Workspace: t.TempDir(), Owner: session.Owner{Channel: "rest", ID: "ci"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })
	startPrompt := func(text string) <-chan error {
		t.Helper()
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			close(entered)
			_, err := memory.Inject(t.Context(), manager, channel.Prompt{SessionID: record.ID, Text: text})
			done <- err
		}()
		<-entered
		runtime.Gosched()
		return done
	}

	done := []<-chan error{startPrompt("first")}
	<-firstStarted
	done = append(done, startPrompt("second"), startPrompt("third"), startPrompt("fourth"))
	close(releaseFirst)
	for index, result := range done {
		if err := <-result; err != nil {
			t.Errorf("Prompt %d: %v", index+1, err)
		}
	}

	got := memory.Snapshot()
	want := []channel.Event{
		{SessionID: record.ID, AgentEvent: agent.Message{Text: "1"}},
		{SessionID: record.ID, AgentEvent: agent.Message{Text: "2"}},
		{SessionID: record.ID, AgentEvent: agent.Message{Text: "3"}},
		{SessionID: record.ID, AgentEvent: agent.Message{Text: "4"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events = %#v, want FIFO delivery %#v", got, want)
	}
}

func TestShutdownDropsQueuedPromptsAndLeavesSessionDormant(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	neverContinue := make(chan struct{})
	script := agent.Script{Turns: []agent.Turn{
		{WantPrompt: "in flight", Started: firstStarted, Continue: neverContinue, Stop: agent.StopEndTurn},
		{WantPrompt: "queued", Events: []agent.Event{agent.Message{Text: "must not run"}}, Stop: agent.StopEndTurn},
	}}
	connect := scriptedConnector(&script)
	database := t.TempDir() + "/aethos.db"
	memory := &channel.Memory{}
	manager, err := session.Open(t.Context(), database, connect, memory)
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	record, err := manager.Create(t.Context(), session.Create{
		Agent: "codex-acp", Workspace: t.TempDir(), Owner: session.Owner{Channel: "rest", ID: "ci"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })
	startPrompt := func(text string) <-chan error {
		done := make(chan error, 1)
		entered := make(chan struct{})
		go func() {
			close(entered)
			_, err := memory.Inject(t.Context(), manager, channel.Prompt{SessionID: record.ID, Text: text})
			done <- err
		}()
		<-entered
		runtime.Gosched()
		return done
	}

	inFlight := startPrompt("in flight")
	<-firstStarted
	queued := startPrompt("queued")
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for name, result := range map[string]<-chan error{"in-flight Prompt": inFlight, "queued Prompt": queued} {
		if err := <-result; !errors.Is(err, session.ErrClosed) {
			t.Errorf("%s error = %v, want ErrClosed", name, err)
		}
	}
	if got := memory.Snapshot(); len(got) != 0 {
		t.Errorf("events after shutdown = %#v, want none", got)
	}

	reopened, err := session.Open(t.Context(), database, connect, &channel.Memory{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != session.Dormant {
		t.Errorf("State = %q, want %q", got.State, session.Dormant)
	}
}

func TestOpenRejectsDatabaseFromNewerAethos(t *testing.T) {
	database := t.TempDir() + "/aethos.db"
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("set fixture version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}

	connect := func(ctx context.Context, command string, onEvent agent.EventHandler) (*agent.Conn, error) {
		return nil, errors.New("connector must not run while opening the database")
	}
	_, err = session.Open(t.Context(), database, connect, &channel.Memory{})
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("session.Open newer database = %v, want version error", err)
	}
}

func TestChannelFailureFailsPrompt(t *testing.T) {
	want := errors.New("Channel unavailable")
	script := agent.Script{Turns: []agent.Turn{{
		Events: []agent.Event{agent.Message{Text: "undeliverable"}},
		Stop:   agent.StopEndTurn,
	}}}
	connect := scriptedConnector(&script)
	manager, err := session.Open(t.Context(), t.TempDir()+"/aethos.db", connect, errorChannel{err: want})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	defer manager.Close()
	record, err := manager.Create(t.Context(), session.Create{
		Agent: "codex-acp", Workspace: t.TempDir(), Owner: session.Owner{Channel: "telegram", ID: "42"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := manager.Prompt(t.Context(), record.ID, "hello"); !errors.Is(err, want) {
		t.Errorf("Prompt error = %v, want Channel failure", err)
	}
}

type errorChannel struct{ err error }

func (c errorChannel) Send(context.Context, channel.Event) error { return c.err }

func scriptedConnector(script *agent.Script) session.Connect {
	return func(ctx context.Context, _ string, onEvent agent.EventHandler) (*agent.Conn, error) {
		return agent.ConnectScript(ctx, slog.New(slog.DiscardHandler), onEvent, script)
	}
}
