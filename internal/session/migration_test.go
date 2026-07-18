package session_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/session"

	_ "modernc.org/sqlite"
)

// versionTwoStatements is the sessions schema exactly as the previous release
// shipped it, before Topic bindings became owner-scoped keys.
var versionTwoStatements = []string{
	`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		agent_session_id TEXT NOT NULL,
		agent TEXT NOT NULL,
		workspace TEXT NOT NULL,
		owner_channel TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		state TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		last_activity_at INTEGER NOT NULL
	) STRICT;`,
	`ALTER TABLE sessions ADD COLUMN name TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE sessions ADD COLUMN topic_id INTEGER NOT NULL DEFAULT 0;`,
	`CREATE UNIQUE INDEX sessions_topic_id ON sessions(topic_id) WHERE topic_id != 0;`,
	`PRAGMA user_version = 2;`,
}

func seedVersionTwoDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open version-two database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close version-two database: %v", err)
		}
	}()
	for _, statement := range versionTwoStatements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("apply version-two schema: %v", err)
		}
	}
	now := time.Now().UnixNano()
	insert := `INSERT INTO sessions (
		id, name, agent_session_id, agent, workspace, owner_channel, owner_id,
		topic_id, state, created_at, last_activity_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := db.Exec(insert,
		"sess-tg", "Investigate flaky tests", "acp-1", "scripted", "/workspace/tg",
		"telegram", "123456789", 202, "dormant", now, now,
	); err != nil {
		t.Fatalf("seed Telegram-bound Session: %v", err)
	}
	if _, err := db.Exec(insert,
		"sess-rest", "", "acp-2", "scripted", "/workspace/rest",
		"rest", "automation", 0, "dormant", now, now,
	); err != nil {
		t.Fatalf("seed unbound Session: %v", err)
	}
}

func TestTopicKeyMigrationPreservesBindingsAndScopesLookupByChannel(t *testing.T) {
	database := filepath.Join(t.TempDir(), "aethos.db")
	seedVersionTwoDatabase(t, database)

	script := agent.Script{}
	manager, err := session.Open(t.Context(), database, scriptedConnector(&script), &channel.Memory{})
	if err != nil {
		t.Fatalf("open Session manager on version-two database: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close Session manager: %v", err)
		}
	})

	bound, err := manager.FindByTopic(t.Context(), "telegram", "202")
	if err != nil {
		t.Fatalf("find migrated Telegram binding: %v", err)
	}
	if bound.ID != "sess-tg" || bound.Name != "Investigate flaky tests" || bound.TopicKey != "202" {
		t.Errorf("migrated Session = %q/%q/%q, want sess-tg bound to Topic key 202",
			bound.ID, bound.Name, bound.TopicKey)
	}

	if _, err := manager.FindByTopic(t.Context(), "slack", "202"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Topic lookup for another Channel's key = %v, want no rows", err)
	}

	unbound, err := manager.Get(t.Context(), "sess-rest")
	if err != nil {
		t.Fatalf("get unbound Session: %v", err)
	}
	if unbound.TopicKey != "" {
		t.Errorf("unbound Session Topic key = %q, want empty", unbound.TopicKey)
	}

	if _, err := manager.Create(t.Context(), session.Create{
		Agent:     "scripted",
		Workspace: t.TempDir(),
		Owner:     session.Owner{Channel: "telegram", ID: "123456789"},
		TopicKey:  "202",
	}); err == nil {
		t.Error("creating a second telegram Session with Topic key 202 succeeded, want rejection")
	}

	slackBound, err := manager.Create(t.Context(), session.Create{
		Agent:     "scripted",
		Workspace: t.TempDir(),
		Owner:     session.Owner{Channel: "slack", ID: "U024BE7LH"},
		TopicKey:  "202",
	})
	if err != nil {
		t.Fatalf("create slack Session reusing another Channel's key: %v", err)
	}
	found, err := manager.FindByTopic(t.Context(), "slack", "202")
	if err != nil {
		t.Fatalf("find slack Session by Topic key: %v", err)
	}
	if found.ID != slackBound.ID {
		t.Errorf("slack Topic key 202 resolves to %q, want %q", found.ID, slackBound.ID)
	}
}
