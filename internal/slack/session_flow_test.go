package slack_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
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
		messageEnvelope("new-envelope", "U0123456789", "C0123456789", "new "+workspace+" | goose", ""),
	}})
	bridge := newSlackChannel(t, api)
	manager := openSlackSessionManager(t, bridge, &agent.Script{})

	stop := startSlackFlow(t, bridge, manager)

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
	if record.Agent != "goose" || record.Workspace != workspace {
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

	stop()
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
			wantReply: `agent "missing-agent" is not installed`,
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

			stop := startSlackFlow(t, bridge, manager)

			waitFor(t, func() bool { return api.hasPostContaining("C0123456789", test.wantReply) })
			records, err := manager.List(t.Context())
			if err != nil {
				t.Fatalf("list Slack Sessions: %v", err)
			}
			if len(records) != 0 {
				t.Errorf("created %d Sessions for invalid %s selection, want none", len(records), strings.ToLower(test.name))
			}

			stop()
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

	stop := startSlackFlow(t, bridge, manager)

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

	stop()
}

func TestSlackFlowListsEverySessionFromAssistant(t *testing.T) {
	workspace := t.TempDir()
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("first-new-envelope", "U0123456789", "C0123456789", "new", ""),
		messageEnvelope("second-new-envelope", "U0123456789", "C0123456789", "new "+workspace+" | goose", ""),
		messageEnvelope("sessions-envelope", "U0123456789", "C0123456789", "sessions", ""),
	}})
	bridge := newSlackChannelWithDefaults(t, api, workspace)
	manager := openSlackSessionManager(t, bridge, &agent.Script{})

	stop := startSlackFlow(t, bridge, manager)

	var records []session.Record
	waitFor(t, func() bool {
		var err error
		records, err = manager.List(t.Context())
		return err == nil && len(records) == 2
	})
	waitFor(t, func() bool {
		if !api.hasPost("C0123456789", "Sessions:") {
			return false
		}
		for _, record := range records {
			want := record.ID + "\nState: live\nAgent: " + string(record.Agent) + "\nName: (unnamed)"
			if !api.hasPost("C0123456789", want) {
				return false
			}
		}
		return true
	})
	for _, record := range records {
		want := record.ID + "\nState: live\nAgent: " + string(record.Agent) + "\nName: (unnamed)"
		if !api.hasPost("C0123456789", want) {
			t.Errorf("Slack Assistant did not list Session %q with state, Agent, and name", record.ID)
		}
	}

	stop()
}

func TestSlackFlowClosesSessionFromAssistant(t *testing.T) {
	flow := startOneSlackSessionFlow(t, &agent.Script{})
	flow.api.sendEnvelope(messageEnvelope(
		"close-envelope", "U0123456789", "C0123456789", "close "+flow.record.ID, "",
	))

	waitFor(t, func() bool {
		return flow.api.hasPost(
			"C0123456789", "Session closed: "+flow.record.ID+" ("+flow.record.ID+").",
		) && flow.api.hasUpdate(flow.threadTS, "New Session\nState: closed")
	})
	closed, err := flow.manager.Get(t.Context(), flow.record.ID)
	if err != nil {
		t.Fatalf("get closed Slack Session: %v", err)
	}
	if closed.State != session.Closed {
		t.Errorf("closed Session state = %q, want %q", closed.State, session.Closed)
	}

	flow.stop()
}

func TestSlackFlowAnswersCloseForMissingSession(t *testing.T) {
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("close-envelope", "U0123456789", "C0123456789", "close missing", ""),
	}})
	bridge := newSlackChannel(t, api)
	manager := openSlackSessionManager(t, bridge, &agent.Script{})

	stop := startSlackFlow(t, bridge, manager)

	waitFor(t, func() bool {
		return api.hasPost("C0123456789", "Session not found: missing")
	})

	stop()
}

func TestSlackFlowAnswersCloseWithoutSessionID(t *testing.T) {
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("close-envelope", "U0123456789", "C0123456789", "close", ""),
	}})
	bridge := newSlackChannel(t, api)
	manager := openSlackSessionManager(t, bridge, &agent.Script{})

	stop := startSlackFlow(t, bridge, manager)

	waitFor(t, func() bool {
		return api.hasPost("C0123456789", "Usage: close <Session ID>")
	})

	stop()
}

func TestSlackFlowAnswersCloseForAlreadyClosedSession(t *testing.T) {
	flow := startOneSlackSessionFlow(t, &agent.Script{})
	flow.api.sendEnvelope(messageEnvelope(
		"first-close-envelope", "U0123456789", "C0123456789", "close "+flow.record.ID, "",
	))
	waitFor(t, func() bool {
		return flow.api.hasPost(
			"C0123456789", "Session closed: "+flow.record.ID+" ("+flow.record.ID+").",
		)
	})
	flow.api.sendEnvelope(messageEnvelope(
		"second-close-envelope", "U0123456789", "C0123456789", "close "+flow.record.ID, "",
	))

	waitFor(t, func() bool {
		return flow.api.hasPost("C0123456789", "Session is already closed: "+flow.record.ID)
	})

	flow.stop()
}

func TestSlackFlowAnswersPromptInClosedSessionWithoutDispatch(t *testing.T) {
	dispatched := make(chan struct{}, 1)
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "do not dispatch",
		Started:    dispatched,
		Stop:       agent.StopEndTurn,
	}}}
	flow := startOneSlackSessionFlow(t, &script)
	flow.api.sendEnvelope(messageEnvelope(
		"close-envelope", "U0123456789", "C0123456789", "close "+flow.record.ID, "",
	))
	waitFor(t, func() bool {
		closed, err := flow.manager.Get(t.Context(), flow.record.ID)
		return err == nil && closed.State == session.Closed
	})
	flow.api.sendEnvelope(messageEnvelope(
		"closed-prompt-envelope", "U0123456789", "C0123456789", "do not dispatch", flow.threadTS,
	))

	waitFor(t, func() bool {
		return flow.api.hasThreadPost(
			flow.threadTS, "This Session is closed. Start a new Session to continue.",
		)
	})
	select {
	case <-dispatched:
		t.Fatal("Prompt in a closed Slack Session reached the Agent")
	default:
	}

	flow.stop()
}

func TestSlackFlowResumesDormantSessionAndSurfacesState(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "resume work",
		Events:     []agent.Event{agent.Message{Text: "resumed"}},
		Stop:       agent.StopEndTurn,
	}}}
	flow := startOneSlackSessionFlow(t, &script, session.WithIdleTimeout(250*time.Millisecond))
	waitFor(t, func() bool {
		record, err := flow.manager.Get(t.Context(), flow.record.ID)
		return err == nil && record.State == session.Dormant &&
			flow.api.hasUpdate(flow.threadTS, "New Session\nState: dormant")
	})
	flow.api.sendEnvelope(messageEnvelope(
		"resume-envelope", "U0123456789", "C0123456789", "resume work", flow.threadTS,
	))

	waitFor(t, func() bool {
		resumed, err := flow.manager.Get(t.Context(), flow.record.ID)
		return err == nil && resumed.State == session.Live &&
			flow.api.hasUpdate(flow.threadTS, "resume work\nState: live") &&
			flow.api.hasThreadPost(flow.threadTS, "resumed") &&
			flow.api.hasThreadPost(flow.threadTS, "Prompt finished: end_turn.")
	})

	flow.stop()
}

func TestSlackFlowAppliesAgentDrivenSessionRenameToRoot(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "inspect authentication",
		Events:     []agent.Event{agent.SessionRenamed{Name: "Review auth flow"}},
		Stop:       agent.StopEndTurn,
	}}}
	flow := startOneSlackSessionFlow(t, &script)
	flow.api.sendEnvelope(messageEnvelope(
		"prompt-envelope", "U0123456789", "C0123456789", "inspect authentication", flow.threadTS,
	))
	waitFor(t, func() bool {
		records, err := flow.manager.List(t.Context())
		return err == nil && len(records) == 1 && records[0].Name == "Review auth flow" &&
			flow.api.hasUpdate(flow.threadTS, "Review auth flow\nState: live")
	})

	flow.stop()
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

	stop := startSlackFlow(t, bridge, manager)

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

	stop()
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

	stop := startSlackFlow(t, bridge, manager)

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

	stop()
}

func TestSlackFlowCancelButtonStopsInflightPromptAndDisappears(t *testing.T) {
	started := make(chan struct{}, 1)
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "keep working",
		Started:    started,
		Events: []agent.Event{
			agent.Message{Text: "Working"},
			agent.Message{Text: "should not be sent"},
		},
		EventInterval: time.Hour,
		Stop:          agent.StopEndTurn,
	}}}
	flow := startOneSlackSessionFlow(t, &script)
	flow.api.sendEnvelope(messageEnvelope(
		"prompt-envelope", "U0123456789", "C0123456789", "keep working", flow.threadTS,
	))

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Slack Prompt did not reach the scripted Agent")
	}
	var draftTS, cancelValue string
	waitFor(t, func() bool {
		draftTS, cancelValue = flow.api.threadButton(flow.threadTS, "Working", "Cancel")
		return draftTS != "" && cancelValue != ""
	})
	flow.api.sendEnvelope(blockActionEnvelope(
		"cancel-envelope",
		"U0123456789",
		"C0123456789",
		draftTS,
		flow.threadTS,
		"cancel_prompt",
		cancelValue,
	))

	waitFor(t, func() bool {
		return flow.api.hasAcknowledgement("cancel-envelope") &&
			flow.api.hasThreadPost(flow.threadTS, "Prompt cancelled.") &&
			flow.api.hasUpdateWithoutButtons(draftTS, "Working")
	})
	if flow.api.hasThreadPost(flow.threadTS, "should not be sent") {
		t.Error("Slack Channel streamed Agent output after cancellation")
	}
	got, err := flow.manager.Get(t.Context(), flow.record.ID)
	if err != nil {
		t.Fatalf("get Session after Slack cancellation: %v", err)
	}
	if got.State != session.Live {
		t.Errorf("Session state after Slack cancellation = %q, want %q", got.State, session.Live)
	}

	flow.stop()
}

func TestSlackFlowCancelButtonIsAvailableBeforeAgentOutput(t *testing.T) {
	started := make(chan struct{}, 1)
	neverContinue := make(chan struct{})
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "work silently",
		Started:    started,
		Continue:   neverContinue,
		Stop:       agent.StopEndTurn,
	}}}
	flow := startOneSlackSessionFlow(t, &script)
	flow.api.sendEnvelope(messageEnvelope(
		"prompt-envelope", "U0123456789", "C0123456789", "work silently", flow.threadTS,
	))

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Slack Prompt did not reach the scripted Agent")
	}
	draftTS, cancelValue := flow.api.threadButton(flow.threadTS, "Prompt started.", "Cancel")
	if draftTS == "" || cancelValue == "" {
		t.Fatal("Slack draft did not expose Cancel before Agent output")
	}
	flow.api.sendEnvelope(blockActionEnvelope(
		"cancel-envelope",
		"U0123456789",
		"C0123456789",
		draftTS,
		flow.threadTS,
		"cancel_prompt",
		cancelValue,
	))

	waitFor(t, func() bool {
		return flow.api.hasAcknowledgement("cancel-envelope") &&
			flow.api.hasThreadPost(flow.threadTS, "Prompt cancelled.") &&
			flow.api.hasUpdateWithoutButtons(draftTS, "Prompt started.")
	})

	flow.stop()
}

func TestSlackFlowCancelButtonAnswersWhenPromptJustFinished(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "finish quickly",
		Events:     []agent.Event{agent.Message{Text: "Done"}},
		Stop:       agent.StopEndTurn,
	}}}
	flow := startOneSlackSessionFlow(t, &script)
	flow.api.sendEnvelope(messageEnvelope(
		"prompt-envelope", "U0123456789", "C0123456789", "finish quickly", flow.threadTS,
	))

	var draftTS, cancelValue string
	waitFor(t, func() bool {
		draftTS, cancelValue = flow.api.threadButton(flow.threadTS, "Done", "Cancel")
		return draftTS != "" && cancelValue != "" &&
			flow.api.hasUpdateWithoutButtons(draftTS, "Done") &&
			flow.api.hasThreadPost(flow.threadTS, "Prompt finished: end_turn.")
	})
	flow.api.sendEnvelope(blockActionEnvelope(
		"late-cancel-envelope",
		"U0123456789",
		"C0123456789",
		draftTS,
		flow.threadTS,
		"cancel_prompt",
		cancelValue,
	))

	waitFor(t, func() bool {
		return flow.api.hasAcknowledgement("late-cancel-envelope") &&
			flow.api.hasThreadPost(flow.threadTS, "No Prompt is currently running.")
	})

	flow.stop()
}

func TestSlackFlowPermissionButtonsResolveRequestAndShowDecision(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "update the config",
		Permissions: []agent.ScriptedPermission{{
			Request: agent.PermissionRequest{
				ToolCallID: "call-1",
				Title:      "Update config.toml",
				Kind:       "edit",
				Options: []agent.PermissionOption{
					{ID: "allow-once", Name: "Allow once", Kind: agent.PermissionAllowOnce},
					{ID: "reject-once", Name: "Reject once", Kind: agent.PermissionRejectOnce},
				},
			},
			WantOptionID: "allow-once",
		}},
		Events: []agent.Event{agent.Message{Text: "config updated"}},
		Stop:   agent.StopEndTurn,
	}}}
	flow := startOneSlackSessionFlow(t, &script)
	flow.api.sendEnvelope(messageEnvelope(
		"prompt-envelope", "U0123456789", "C0123456789", "update the config", flow.threadTS,
	))

	const permissionText = "Permission requested: Update config.toml\nKind: edit"
	var permissionTS, approveValue, denyValue string
	waitFor(t, func() bool {
		permissionTS, approveValue = flow.api.threadButton(
			flow.threadTS, permissionText, "Approve",
		)
		_, denyValue = flow.api.threadButton(flow.threadTS, permissionText, "Deny")
		return permissionTS != "" && approveValue != "" && denyValue != ""
	})
	flow.api.sendEnvelope(blockActionEnvelope(
		"approve-envelope",
		"U0123456789",
		"C0123456789",
		permissionTS,
		flow.threadTS,
		"resolve_permission",
		approveValue,
	))
	flow.api.sendEnvelope(blockActionEnvelope(
		"stale-deny-envelope",
		"U0123456789",
		"C0123456789",
		permissionTS,
		flow.threadTS,
		"resolve_permission",
		denyValue,
	))

	waitFor(t, func() bool {
		return flow.api.hasAcknowledgement("approve-envelope") &&
			flow.api.hasAcknowledgement("stale-deny-envelope") &&
			flow.api.hasUpdateWithoutButtons(
				permissionTS, permissionText+"\nOutcome: Approved",
			) &&
			flow.api.hasThreadPost(flow.threadTS, "config updated")
	})

	flow.stop()
}

func TestSlackFlowPermissionTimeoutRemovesButtonsAndShowsOutcome(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "update the config",
		Permissions: []agent.ScriptedPermission{{
			Request: agent.PermissionRequest{
				ToolCallID: "call-1",
				Title:      "Update config.toml",
				Kind:       "edit",
				Options: []agent.PermissionOption{
					{ID: "allow-once", Name: "Allow once", Kind: agent.PermissionAllowOnce},
					{ID: "reject-once", Name: "Reject once", Kind: agent.PermissionRejectOnce},
				},
			},
			WantOptionID: "reject-once",
		}},
		Events: []agent.Event{agent.Message{Text: "continued after denial"}},
		Stop:   agent.StopEndTurn,
	}}}
	flow := startOneSlackSessionFlow(t, &script, session.WithPermissionTimeout(25*time.Millisecond))
	flow.api.delayPermissionPosts(75 * time.Millisecond)
	flow.api.failPermissionUpdates(1)
	flow.api.sendEnvelope(messageEnvelope(
		"prompt-envelope", "U0123456789", "C0123456789", "update the config", flow.threadTS,
	))

	const permissionText = "Permission requested: Update config.toml\nKind: edit"
	var permissionTS string
	waitFor(t, func() bool {
		permissionTS, _ = flow.api.threadButton(flow.threadTS, permissionText, "Approve")
		return permissionTS != ""
	})
	waitFor(t, func() bool {
		return flow.api.hasUpdateWithoutButtons(
			permissionTS, permissionText+"\nOutcome: Timed out — denied",
		) && flow.api.permissionUpdateCount(
			permissionTS, permissionText+"\nOutcome: Timed out — denied",
		) >= 2 && flow.api.hasThreadPost(flow.threadTS, "continued after denial")
	})

	flow.stop()
}

func TestSlackFlowIgnoresNonAllowlistedPermissionButtonPress(t *testing.T) {
	script := agent.Script{Turns: []agent.Turn{{
		WantPrompt: "update the config",
		Permissions: []agent.ScriptedPermission{{
			Request: agent.PermissionRequest{
				ToolCallID: "call-1",
				Title:      "Update config.toml",
				Kind:       "edit",
				Options: []agent.PermissionOption{
					{ID: "allow-once", Name: "Allow once", Kind: agent.PermissionAllowOnce},
					{ID: "reject-once", Name: "Reject once", Kind: agent.PermissionRejectOnce},
				},
			},
			WantOptionID: "reject-once",
		}},
		Events: []agent.Event{agent.Message{Text: "continued after allowed user's denial"}},
		Stop:   agent.StopEndTurn,
	}}}
	flow := startOneSlackSessionFlow(t, &script)
	flow.api.sendEnvelope(messageEnvelope(
		"prompt-envelope", "U0123456789", "C0123456789", "update the config", flow.threadTS,
	))

	const permissionText = "Permission requested: Update config.toml\nKind: edit"
	var permissionTS, approveValue, denyValue string
	waitFor(t, func() bool {
		permissionTS, approveValue = flow.api.threadButton(flow.threadTS, permissionText, "Approve")
		_, denyValue = flow.api.threadButton(flow.threadTS, permissionText, "Deny")
		return permissionTS != "" && approveValue != "" && denyValue != ""
	})
	flow.api.sendEnvelope(blockActionEnvelope(
		"rejected-approve-envelope",
		"U9999999999",
		"C0123456789",
		permissionTS,
		flow.threadTS,
		"resolve_permission",
		approveValue,
	))
	waitFor(t, func() bool { return flow.api.hasAcknowledgement("rejected-approve-envelope") })
	flow.api.sendEnvelope(blockActionEnvelope(
		"allowed-deny-envelope",
		"U0123456789",
		"C0123456789",
		permissionTS,
		flow.threadTS,
		"resolve_permission",
		denyValue,
	))

	waitFor(t, func() bool {
		return flow.api.hasAcknowledgement("allowed-deny-envelope") &&
			flow.api.hasUpdateWithoutButtons(
				permissionTS, permissionText+"\nOutcome: Denied",
			) && flow.api.hasThreadPost(
			flow.threadTS, "continued after allowed user's denial",
		)
	})

	flow.stop()
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

	stop := startSlackFlow(t, bridge, manager)

	waitFor(t, func() bool {
		return api.hasThreadPostContaining(
			threadTS,
			"Prompt finished with error:",
		)
	})

	stop()
}

type slackSessionFlow struct {
	api      *slackFixtureAPI
	manager  *session.Manager
	record   session.Record
	threadTS string
	stop     func()
}

func startOneSlackSessionFlow(
	t *testing.T,
	script *agent.Script,
	options ...session.Option,
) slackSessionFlow {
	t.Helper()
	const threadTS = "1750000001.000001"
	api := newSlackFixtureAPI(t, socketScript{envelopes: []string{
		`{"type":"hello","connection_info":{"app_id":"A0123456789"}}`,
		messageEnvelope("new-envelope", "U0123456789", "C0123456789", "new", ""),
	}})
	bridge := newSlackChannelWithDefaults(
		t, api, t.TempDir(), slack.WithWriteInterval(5*time.Millisecond),
	)
	manager := openSlackSessionManager(t, bridge, script, options...)
	stop := startSlackFlow(t, bridge, manager)

	var record session.Record
	waitFor(t, func() bool {
		records, err := manager.List(t.Context())
		if err != nil || len(records) != 1 ||
			!api.hasThreadPost(threadTS, "Session ready. Send a Prompt in this thread.") {
			return false
		}
		record = records[0]
		return true
	})
	return slackSessionFlow{
		api: api, manager: manager, record: record, threadTS: threadTS, stop: stop,
	}
}

func startSlackFlow(t *testing.T, bridge *slack.Channel, manager *session.Manager) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, manager) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("run Slack Channel: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func openSlackSessionManager(
	t *testing.T,
	bridge *slack.Channel,
	script *agent.Script,
	options ...session.Option,
) *session.Manager {
	t.Helper()
	manager, err := session.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "aethos.db"),
		func(ctx context.Context, _ session.AgentRef, handlers agent.Handlers) (*agent.Conn, error) {
			return agent.ConnectScript(ctx, slog.New(slog.DiscardHandler), handlers, script)
		},
		bridge,
		options...,
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
