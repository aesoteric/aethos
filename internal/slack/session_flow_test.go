package slack_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/session"
	"github.com/aesoteric/aethos/internal/slack"
)

func TestSlackFlowCreatesSessionBoundToFreshThread(t *testing.T) {
	workspace := t.TempDir()
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("new-envelope", "U0123456789", "C0123456789", "new "+workspace+" | codex-acp", ""),
	}})
	bridge := newSlackChannel(t, api)
	manager := openSlackSessionManager(t, bridge, &agent.Script{})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	waitFor(t, func() bool {
		records, err := manager.List(t.Context())
		return err == nil && len(records) == 1 &&
			api.hasThreadPost(records[0].TopicKey, "Session ready. Send a Prompt in this thread.")
	})
	records, err := manager.List(t.Context())
	if err != nil {
		t.Fatalf("list Slack Sessions: %v", err)
	}
	record := records[0]
	if record.Agent != "codex-acp" || record.Workspace != workspace {
		t.Errorf("Session selection = Agent %q Workspace %q, want command choices", record.Agent, record.Workspace)
	}
	if record.Owner != (session.Owner{Channel: "slack", ID: "U0123456789"}) {
		t.Errorf("Session owner = %#v, want allowlisted Slack user", record.Owner)
	}
	if record.TopicKey != "1750000001.000001" {
		t.Errorf("Session Topic key = %q, want root message timestamp", record.TopicKey)
	}
	if !api.hasPost("C0123456789", "New Session\nState: live") {
		t.Error("Slack Channel did not post a Session root carrying its name and state")
	}
	if !api.hasThreadPost(record.TopicKey, "Session ready. Send a Prompt in this thread.") {
		t.Error("Slack Channel did not post the ready notice in the new Session thread")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run Slack Channel: %v", err)
	}
}

func TestSlackFlowAnswersInvalidSessionSelectionsInChannel(t *testing.T) {
	workspace := t.TempDir()
	tests := []struct {
		name      string
		selection string
		wantReply string
	}{
		{
			name:      "Workspace",
			selection: filepath.Join(workspace, "missing") + " | codex-acp",
			wantReply: "open Workspace",
		},
		{
			name:      "Agent",
			selection: workspace + " | missing-agent",
			wantReply: `Agent "missing-agent" is not installed`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
				`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
				messageEnvelope("new-envelope", "U0123456789", "C0123456789", "new "+test.selection, ""),
			}})
			bridge := newSlackChannelWithDefaults(t, api, workspace)
			manager := openSlackSessionManager(t, bridge, &agent.Script{})

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- bridge.Run(ctx, manager) }()

			waitFor(t, func() bool { return api.hasPostContaining("C0123456789", test.wantReply) })
			records, err := manager.List(t.Context())
			if err != nil {
				t.Fatalf("list Slack Sessions: %v", err)
			}
			if len(records) != 0 {
				t.Errorf("created %d Sessions for invalid %s selection, want none", len(records), strings.ToLower(test.name))
			}

			cancel()
			if err := <-done; err != nil {
				t.Fatalf("run Slack Channel: %v", err)
			}
		})
	}
}

func TestSlackFlowCreatesSessionWithDefaults(t *testing.T) {
	workspace := t.TempDir()
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("new-envelope", "U0123456789", "C0123456789", "new", ""),
	}})
	bridge := newSlackChannelWithDefaults(t, api, workspace)
	manager := openSlackSessionManager(t, bridge, &agent.Script{})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	waitFor(t, func() bool {
		records, err := manager.List(t.Context())
		return err == nil && len(records) == 1
	})
	records, err := manager.List(t.Context())
	if err != nil {
		t.Fatalf("list Slack Sessions: %v", err)
	}
	if records[0].Agent != "codex-acp" || records[0].Workspace != workspace {
		t.Errorf("Session selection = Agent %q Workspace %q, want configured defaults", records[0].Agent, records[0].Workspace)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run Slack Channel: %v", err)
	}
}

func TestSlackFlowDispatchesThreadPromptsSerially(t *testing.T) {
	workspace := t.TempDir()
	firstStarted := make(chan struct{}, 1)
	continueFirst := make(chan struct{})
	secondStarted := make(chan struct{}, 1)
	script := agent.Script{Turns: []agent.Turn{
		{
			WantPrompt: "first Prompt",
			Started:    firstStarted,
			Continue:   continueFirst,
			Stop:       agent.StopEndTurn,
		},
		{
			WantPrompt: "second Prompt",
			Started:    secondStarted,
			Stop:       agent.StopEndTurn,
		},
	}}
	const threadTS = "1750000001.000001"
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("new-envelope", "U0123456789", "C0123456789", "new", ""),
		messageEnvelope("first-prompt-envelope", "U0123456789", "C0123456789", "first Prompt", threadTS),
		messageEnvelope("second-prompt-envelope", "U0123456789", "C0123456789", "second Prompt", threadTS),
	}})
	bridge := newSlackChannelWithDefaults(t, api, workspace)
	manager := openSlackSessionManager(t, bridge, &script)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first Slack Prompt did not reach the scripted Agent")
	}
	waitFor(t, func() bool { return api.hasAcknowledgement("second-prompt-envelope") })
	select {
	case <-secondStarted:
		t.Fatal("second Slack Prompt started before the first Prompt finished")
	default:
	}
	close(continueFirst)
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("queued Slack Prompt did not start after the first Prompt finished")
	}
	waitFor(t, func() bool {
		records, err := manager.List(t.Context())
		return err == nil && len(records) == 1 && records[0].Name == "first Prompt" &&
			api.hasUpdate(threadTS, "first Prompt\nState: live")
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run Slack Channel: %v", err)
	}
}

func TestSlackFlowStreamsChunkedOutputAndSurfacesPromptLifecycle(t *testing.T) {
	workspace := t.TempDir()
	continuation := strings.Repeat("x", 4001)
	firstChunk := "Hello" + strings.Repeat("x", 3995)
	secondChunk := strings.Repeat("x", 6)
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt:    "stream output",
		Events:        []agent.Event{agent.Message{Text: "Hello"}, agent.Message{Text: continuation}},
		EventInterval: 100 * time.Millisecond,
		Stop:          agent.StopEndTurn,
	}}}
	const threadTS = "1750000001.000001"
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("new-envelope", "U0123456789", "C0123456789", "new", ""),
		messageEnvelope("prompt-envelope", "U0123456789", "C0123456789", "stream output", threadTS),
	}})
	bridge := newSlackChannelWithDefaults(t, api, workspace, slack.WithWriteInterval(5*time.Millisecond))
	manager := openSlackSessionManager(t, bridge, &script)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	waitFor(t, func() bool {
		return api.hasThreadPost(threadTS, "Prompt started.") &&
			api.hasThreadPost(threadTS, "Prompt finished: end_turn.") &&
			api.hasThreadPost(threadTS, secondChunk)
	})
	draftTS := api.threadPostTimestamp(threadTS, "Hello")
	if draftTS == "" {
		t.Fatal("Slack Channel did not post the first streaming draft")
	}
	if !api.hasUpdate(draftTS, firstChunk) {
		t.Error("Slack Channel did not throttle later Agent output through a draft edit")
	}
	if !api.updatePrecedesThreadPost(draftTS, firstChunk, threadTS, "Prompt finished: end_turn.") {
		t.Error("PromptFinished was posted before the final draft edit")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run Slack Channel: %v", err)
	}
}

func TestSlackFlowSurfacesPromptFinishedError(t *testing.T) {
	workspace := t.TempDir()
	script := agent.Script{Turns: []agent.Turn{{WantPrompt: "expected Prompt"}}}
	const threadTS = "1750000001.000001"
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("new-envelope", "U0123456789", "C0123456789", "new", ""),
		messageEnvelope("prompt-envelope", "U0123456789", "C0123456789", "unexpected Prompt", threadTS),
	}})
	bridge := newSlackChannelWithDefaults(t, api, workspace, slack.WithWriteInterval(5*time.Millisecond))
	manager := openSlackSessionManager(t, bridge, &script)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()

	waitFor(t, func() bool {
		return api.hasThreadPostContaining(
			threadTS,
			"Prompt finished with error:",
		)
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run Slack Channel: %v", err)
	}
}

func openSlackSessionManager(t *testing.T, bridge *slack.Channel, script *agent.Script) *session.Manager {
	t.Helper()
	manager, err := session.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "aethos.db"),
		func(ctx context.Context, _ session.AgentRef, handlers agent.Handlers) (*agent.Conn, error) {
			return agent.ConnectScript(ctx, slog.New(slog.DiscardHandler), handlers, script)
		},
		bridge,
	)
	if err != nil {
		t.Fatalf("open Slack Session manager: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close Slack Session manager: %v", err)
		}
	})
	return manager
}
