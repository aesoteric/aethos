package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

var migrations = []string{
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
	`ALTER TABLE sessions ADD COLUMN name TEXT NOT NULL DEFAULT '';
	 ALTER TABLE sessions ADD COLUMN topic_id INTEGER NOT NULL DEFAULT 0;
	 CREATE UNIQUE INDEX sessions_topic_id ON sessions(topic_id) WHERE topic_id != 0;`,
	`ALTER TABLE sessions ADD COLUMN topic_key TEXT NOT NULL DEFAULT '';
	 UPDATE sessions SET topic_key = CAST(topic_id AS TEXT) WHERE topic_id != 0;
	 DROP INDEX sessions_topic_id;
	 ALTER TABLE sessions DROP COLUMN topic_id;
	 CREATE UNIQUE INDEX sessions_topic_key ON sessions(owner_channel, topic_key) WHERE topic_key != '';`,
}

func openDatabase(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open Session database %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return nil, errors.Join(
			fmt.Errorf("configure Session database: %w", err),
			closeDatabase(db),
		)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, errors.Join(err, closeDatabase(db))
	}
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB) (returnErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Session database migration: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("roll back Session database migration: %w", err))
		}
	}()

	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read Session database version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("session database version %d is newer than supported version %d", version, len(migrations))
	}
	for index := version; index < len(migrations); index++ {
		if _, err := tx.ExecContext(ctx, migrations[index]); err != nil {
			return fmt.Errorf("apply Session database migration %d: %w", index+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, index+1)); err != nil {
			return fmt.Errorf("record Session database migration %d: %w", index+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Session database migration: %w", err)
	}
	return nil
}

func insertRecord(ctx context.Context, db *sql.DB, record Record, agentSessionID string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO sessions (
			id, name, agent_session_id, agent, workspace, owner_channel, owner_id,
			topic_key, state, created_at, last_activity_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID,
		record.Name,
		agentSessionID,
		record.Agent,
		record.Workspace,
		record.Owner.Channel,
		record.Owner.ID,
		record.TopicKey,
		record.State,
		record.CreatedAt.UnixNano(),
		record.LastActivityAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("persist Session %q: %w", record.ID, err)
	}
	return nil
}
