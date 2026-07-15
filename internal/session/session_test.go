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
	"time"

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

func TestCancelStopsCurrentPromptAndSessionRemainsUsable(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	neverContinue := make(chan struct{})
	script := agent.Script{Turns: []agent.Turn{
		{WantPrompt: "stuck", Started: firstStarted, Continue: neverContinue, Stop: agent.StopEndTurn},
		{WantPrompt: "try again", Events: []agent.Event{agent.Message{Text: "ready"}}, Stop: agent.StopEndTurn},
	}}
	memory := &channel.Memory{}
	manager, err := session.Open(t.Context(), t.TempDir()+"/aethos.db", scriptedConnector(&script), memory)
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

	firstResult := make(chan error, 1)
	go func() {
		_, err := manager.Prompt(t.Context(), record.ID, "stuck")
		firstResult <- err
	}()
	<-firstStarted

	if err := manager.Cancel(t.Context(), record.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Prompt error = %v, want context.Canceled", err)
	}
	stop, err := manager.Prompt(t.Context(), record.ID, "try again")
	if err != nil {
		t.Fatalf("Prompt after Cancel: %v", err)
	}
	if stop != agent.StopEndTurn {
		t.Errorf("stop reason after Cancel = %q, want %q", stop, agent.StopEndTurn)
	}
	got, err := manager.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != session.Live {
		t.Errorf("State = %q, want %q", got.State, session.Live)
	}
	wantEvents := []channel.Event{{SessionID: record.ID, AgentEvent: agent.Message{Text: "ready"}}}
	if events := memory.Snapshot(); !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("events = %#v, want %#v", events, wantEvents)
	}
}

func TestCloseSessionArchivesRecordAndRejectsPrompt(t *testing.T) {
	database := t.TempDir() + "/aethos.db"
	manager, err := session.Open(t.Context(), database, scriptedConnector(&agent.Script{}), &channel.Memory{})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	record, err := manager.Create(t.Context(), session.Create{
		Agent: "codex-acp", Workspace: t.TempDir(), Owner: session.Owner{Channel: "rest", ID: "ci"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed, err := manager.CloseSession(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if closed.State != session.Closed {
		t.Errorf("closed State = %q, want %q", closed.State, session.Closed)
	}
	if _, err := manager.Prompt(t.Context(), record.ID, "resume me"); !errors.Is(err, session.ErrSessionClosed) {
		t.Errorf("Prompt to closed Session error = %v, want ErrSessionClosed", err)
	}
	listed, err := manager.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d Sessions, want 1", len(listed))
	}
	if listed[0].ID != record.ID || listed[0].State != session.Closed ||
		listed[0].Agent != record.Agent || listed[0].Workspace != record.Workspace || listed[0].LastActivityAt.IsZero() {
		t.Errorf("listed Session = %#v, want archived record with status fields", listed[0])
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("manager Close: %v", err)
	}

	reopened, err := session.Open(t.Context(), database, scriptedConnector(&agent.Script{}), &channel.Memory{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get archived Session: %v", err)
	}
	if got.State != session.Closed {
		t.Errorf("persisted State = %q, want %q", got.State, session.Closed)
	}
}

func TestCloseSessionStopsInflightAndQueuedPrompts(t *testing.T) {
	started := make(chan struct{}, 1)
	neverContinue := make(chan struct{})
	script := agent.Script{Turns: []agent.Turn{
		{WantPrompt: "in flight", Started: started, Continue: neverContinue, Stop: agent.StopEndTurn},
		{WantPrompt: "queued", Events: []agent.Event{agent.Message{Text: "must not run"}}, Stop: agent.StopEndTurn},
	}}
	memory := &channel.Memory{}
	manager, err := session.Open(t.Context(), t.TempDir()+"/aethos.db", scriptedConnector(&script), memory)
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

	startPrompt := func(text string) <-chan error {
		t.Helper()
		entered := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			close(entered)
			_, err := manager.Prompt(t.Context(), record.ID, text)
			result <- err
		}()
		<-entered
		runtime.Gosched()
		return result
	}
	inFlight := startPrompt("in flight")
	<-started
	queued := startPrompt("queued")

	if _, err := manager.CloseSession(t.Context(), record.ID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	for name, result := range map[string]<-chan error{"in-flight Prompt": inFlight, "queued Prompt": queued} {
		if err := <-result; !errors.Is(err, session.ErrSessionClosed) {
			t.Errorf("%s error = %v, want ErrSessionClosed", name, err)
		}
	}
	got, err := manager.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != session.Closed {
		t.Errorf("State = %q, want %q", got.State, session.Closed)
	}
	if events := memory.Snapshot(); len(events) != 0 {
		t.Errorf("events after close = %#v, want none", events)
	}
}

func TestIdleTimeoutDemotesLiveSessionAndNextPromptResumesIt(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "back to work",
		Events:     []agent.Event{agent.Message{Text: "resumed"}},
		Stop:       agent.StopEndTurn,
	}}}
	memory := &channel.Memory{}
	manager, err := session.Open(
		t.Context(),
		t.TempDir()+"/aethos.db",
		scriptedConnector(&script),
		memory,
		session.WithIdleTimeout(25*time.Millisecond),
	)
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

	waitFor(t, func() bool {
		got, err := manager.Get(t.Context(), record.ID)
		return err == nil && got.State == session.Dormant
	})
	stop, err := manager.Prompt(t.Context(), record.ID, "back to work")
	if err != nil {
		t.Fatalf("Prompt after idle timeout: %v", err)
	}
	if stop != agent.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", stop, agent.StopEndTurn)
	}
	got, err := manager.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get resumed Session: %v", err)
	}
	if got.State != session.Live {
		t.Errorf("resumed State = %q, want %q", got.State, session.Live)
	}
	wantEvents := []channel.Event{{SessionID: record.ID, AgentEvent: agent.Message{Text: "resumed"}}}
	if events := memory.Snapshot(); !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("events = %#v, want %#v", events, wantEvents)
	}
}

func TestIdleTimeoutStartsAfterPromptFinishes(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "keep working", Started: started, Continue: release, Stop: agent.StopEndTurn,
	}}}
	const idleTimeout = 25 * time.Millisecond
	manager, err := session.Open(
		t.Context(),
		t.TempDir()+"/aethos.db",
		scriptedConnector(&script),
		&channel.Memory{},
		session.WithIdleTimeout(idleTimeout),
	)
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

	result := make(chan error, 1)
	go func() {
		_, err := manager.Prompt(t.Context(), record.ID, "keep working")
		result <- err
	}()
	<-started
	timer := time.NewTimer(3 * idleTimeout)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("test context ended before idle timeout elapsed")
	}
	got, err := manager.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get during Prompt: %v", err)
	}
	if got.State != session.Live {
		t.Errorf("State during Prompt = %q, want %q", got.State, session.Live)
	}

	close(release)
	if err := <-result; err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitFor(t, func() bool {
		got, err := manager.Get(t.Context(), record.ID)
		return err == nil && got.State == session.Dormant
	})
}

func TestAgentCrashDemotesSessionSurfacesEventAndKeepsRecord(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{
		{WantPrompt: "crash now", Crash: true},
		{WantPrompt: "resume", Events: []agent.Event{agent.Message{Text: "back"}}, Stop: agent.StopEndTurn},
	}}
	memory := &channel.Memory{}
	manager, err := session.Open(t.Context(), t.TempDir()+"/aethos.db", scriptedConnector(&script), memory)
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

	if _, err := manager.Prompt(t.Context(), record.ID, "crash now"); err == nil {
		t.Fatal("Prompt after Agent crash returned nil error")
	}
	waitFor(t, func() bool {
		got, err := manager.Get(t.Context(), record.ID)
		return err == nil && got.State == session.Dormant && len(memory.Snapshot()) == 1
	})
	got, err := manager.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get after crash: %v", err)
	}
	if got.ID != record.ID || got.Agent != record.Agent || got.Workspace != record.Workspace || got.Owner != record.Owner {
		t.Errorf("record after crash = %#v, want original durable fields", got)
	}
	events := memory.Snapshot()
	crashed, ok := events[0].AgentEvent.(agent.Crashed)
	if !ok || crashed.Error == "" {
		t.Fatalf("crash event = %#v, want agent.Crashed with an error", events[0])
	}

	if _, err := manager.Prompt(t.Context(), record.ID, "resume"); err != nil {
		t.Fatalf("Prompt after crash: %v", err)
	}
	got, err = manager.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get after resume: %v", err)
	}
	if got.State != session.Live {
		t.Errorf("State after resume = %q, want %q", got.State, session.Live)
	}
}

func TestAgentCrashWhileIdleDemotesSession(t *testing.T) {
	exit := make(chan error, 1)
	script := agent.Script{Exit: exit}
	memory := &channel.Memory{}
	manager, err := session.Open(t.Context(), t.TempDir()+"/aethos.db", scriptedConnector(&script), memory)
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

	exit <- errors.New("process exited with status 2")
	waitFor(t, func() bool {
		got, err := manager.Get(t.Context(), record.ID)
		return err == nil && got.State == session.Dormant && len(memory.Snapshot()) == 1
	})
	events := memory.Snapshot()
	crashed, ok := events[0].AgentEvent.(agent.Crashed)
	if !ok || !strings.Contains(crashed.Error, "status 2") {
		t.Errorf("crash event = %#v, want idle process exit", events[0])
	}
}

func TestSessionStateTransitionMatrix(t *testing.T) {
	tests := []struct {
		from session.State
		to   session.State
		want bool
	}{
		{from: session.Live, to: session.Live, want: false},
		{from: session.Live, to: session.Dormant, want: true},
		{from: session.Live, to: session.Closed, want: true},
		{from: session.Dormant, to: session.Live, want: true},
		{from: session.Dormant, to: session.Dormant, want: false},
		{from: session.Dormant, to: session.Closed, want: true},
		{from: session.Closed, to: session.Live, want: false},
		{from: session.Closed, to: session.Dormant, want: false},
		{from: session.Closed, to: session.Closed, want: false},
	}

	for _, tt := range tests {
		t.Run(string(tt.from)+"_to_"+string(tt.to), func(t *testing.T) {
			if got := tt.from.CanTransitionTo(tt.to); got != tt.want {
				t.Errorf("%q.CanTransitionTo(%q) = %t, want %t", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestDormantSessionCanCloseAndClosedIsTerminal(t *testing.T) {
	database := t.TempDir() + "/aethos.db"
	first, err := session.Open(t.Context(), database, scriptedConnector(&agent.Script{}), &channel.Memory{})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	record, err := first.Create(t.Context(), session.Create{
		Agent: "codex-acp", Workspace: t.TempDir(), Owner: session.Owner{Channel: "telegram", ID: "42"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	manager, err := session.Open(t.Context(), database, scriptedConnector(&agent.Script{}), &channel.Memory{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer manager.Close()
	dormant, err := manager.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("Get dormant Session: %v", err)
	}
	if dormant.State != session.Dormant {
		t.Fatalf("State after reopen = %q, want %q", dormant.State, session.Dormant)
	}
	if _, err := manager.CloseSession(t.Context(), record.ID); err != nil {
		t.Fatalf("CloseSession from dormant: %v", err)
	}
	if _, err := manager.CloseSession(t.Context(), record.ID); !errors.Is(err, session.ErrInvalidTransition) {
		t.Errorf("second CloseSession error = %v, want ErrInvalidTransition", err)
	}
	if _, err := manager.Prompt(t.Context(), record.ID, "resume"); !errors.Is(err, session.ErrSessionClosed) {
		t.Errorf("Prompt to closed Session error = %v, want ErrSessionClosed", err)
	}
}

func TestShutdownDoesNotTransitionDormantOrClosedSessions(t *testing.T) {
	database := t.TempDir() + "/aethos.db"
	manager, err := session.Open(
		t.Context(),
		database,
		scriptedConnector(&agent.Script{}),
		&channel.Memory{},
		session.WithIdleTimeout(25*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	dormant, err := manager.Create(t.Context(), session.Create{
		Agent: "codex-acp", Workspace: t.TempDir(), Owner: session.Owner{Channel: "telegram", ID: "42"},
	})
	if err != nil {
		t.Fatalf("Create dormant fixture: %v", err)
	}
	waitFor(t, func() bool {
		got, err := manager.Get(t.Context(), dormant.ID)
		return err == nil && got.State == session.Dormant
	})
	closed, err := manager.Create(t.Context(), session.Create{
		Agent: "codex-acp", Workspace: t.TempDir(), Owner: session.Owner{Channel: "telegram", ID: "42"},
	})
	if err != nil {
		t.Fatalf("Create closed fixture: %v", err)
	}
	if _, err := manager.CloseSession(t.Context(), closed.ID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("manager Close: %v", err)
	}

	reopened, err := session.Open(t.Context(), database, scriptedConnector(&agent.Script{}), &channel.Memory{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	for id, want := range map[string]session.State{dormant.ID: session.Dormant, closed.ID: session.Closed} {
		got, err := reopened.Get(t.Context(), id)
		if err != nil {
			t.Errorf("Get %s Session: %v", want, err)
			continue
		}
		if got.State != want {
			t.Errorf("Session %q State after shutdown = %q, want %q", id, got.State, want)
		}
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

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition was not met before timeout")
		case <-ticker.C:
		}
	}
}
