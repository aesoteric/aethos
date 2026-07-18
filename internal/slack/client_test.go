package slack_test

import (
	"testing"

	"github.com/aesoteric/aethos/internal/slack"
)

func TestClientCallsSlackMessageEndpoints(t *testing.T) {
	api := newSlackFixtureAPI(t)
	client := slack.NewClient(api.server.URL+"/api", api.server.Client())

	posted, err := client.PostMessage(
		t.Context(), "xoxb-test-token", "C0123456789", "1750000000.000001", "Working on it.",
	)
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if posted.Channel != "C0123456789" || posted.TS != "1750000001.000001" {
		t.Errorf("posted message = %#v, want Slack channel and timestamp", posted)
	}
	if err := client.UpdateMessage(
		t.Context(), "xoxb-test-token", "C0123456789", posted.TS, "Done.",
	); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}

	if !api.hasCall("chat.postMessage", "Bearer xoxb-test-token", map[string]any{
		"channel": "C0123456789", "thread_ts": "1750000000.000001", "text": "Working on it.",
	}) {
		t.Error("chat.postMessage did not receive the expected authenticated JSON request")
	}
	if !api.hasCall("chat.update", "Bearer xoxb-test-token", map[string]any{
		"channel": "C0123456789", "ts": "1750000001.000001", "text": "Done.",
	}) {
		t.Error("chat.update did not receive the expected authenticated JSON request")
	}
}
