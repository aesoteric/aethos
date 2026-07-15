package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
)

// State is the persisted lifecycle state of a Session.
type State string

const (
	// Live means an Agent connection is attached to the Session.
	Live State = "live"
	// Dormant means the record is durable but no Agent connection is attached.
	Dormant State = "dormant"
)

// Owner identifies the person or machine that created a Session. Channel
// disambiguates identities from different user-facing Channels.
type Owner struct {
	Channel string
	ID      string
}

// Create contains the durable attributes of a new Session.
type Create struct {
	Agent     string
	Workspace string
	Owner     Owner
}

// Record is the durable, caller-visible state of a Session.
type Record struct {
	ID             string
	Agent          string
	Workspace      string
	Owner          Owner
	State          State
	CreatedAt      time.Time
	LastActivityAt time.Time
}

// Connect starts one configured Agent and attaches an event handler to it.
// Production supplies a subprocess connector; flow tests supply the scripted
// ACP Agent connector.
type Connect func(context.Context, string, agent.EventHandler) (*agent.Conn, error)

// ErrClosed is returned to in-flight and queued Prompts when the Session
// manager shuts down. Queued Prompts are intentionally not durable.
var ErrClosed = errors.New("session manager is closed")

// Manager owns durable Session records, their live Agent connections, and
// prompt serialization.
type Manager struct {
	db      *sql.DB
	connect Connect
	channel channel.Channel
	ctx     context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex
	sessions map[string]*managedSession
	closed   bool
	workers  sync.WaitGroup
}

type managedSession struct {
	mu             sync.Mutex
	record         Record
	agentSessionID string
	conn           *agent.Conn
	queue          []*promptCall
	working        bool
	closing        bool
}

type promptCall struct {
	ctx    context.Context
	text   string
	result chan promptResult
}

type promptResult struct {
	stop agent.StopReason
	err  error
}

// Open opens the Session database, applies versioned migrations, and recovers
// records left live by an interrupted process as dormant.
func Open(ctx context.Context, databasePath string, connect Connect, ch channel.Channel) (*Manager, error) {
	if !filepath.IsAbs(databasePath) {
		return nil, fmt.Errorf("database path must be absolute, got %q", databasePath)
	}
	if connect == nil {
		return nil, fmt.Errorf("agent connector is required")
	}
	if ch == nil {
		return nil, fmt.Errorf("channel is required")
	}

	db, err := openDatabase(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	m := &Manager{
		db:       db,
		connect:  connect,
		channel:  ch,
		ctx:      lifecycle,
		cancel:   cancel,
		sessions: make(map[string]*managedSession),
	}
	if err := m.load(ctx); err != nil {
		cancel()
		return nil, errors.Join(err, closeDatabase(db))
	}
	return m, nil
}

// Create starts an Agent Session and persists its ownership and lifecycle
// metadata before returning it to the caller.
func (m *Manager) Create(ctx context.Context, create Create) (Record, error) {
	if err := validateCreate(create); err != nil {
		return Record{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Record{}, fmt.Errorf("create Session: %w", ErrClosed)
	}
	m.mu.Unlock()

	id, err := newID()
	if err != nil {
		return Record{}, fmt.Errorf("create Session identity: %w", err)
	}
	conn, err := m.connect(ctx, create.Agent, func(eventCtx context.Context, _ string, event agent.Event) error {
		return m.channel.Send(eventCtx, channel.Event{SessionID: id, AgentEvent: event})
	})
	if err != nil {
		return Record{}, fmt.Errorf("connect Agent %q: %w", create.Agent, err)
	}
	agentSessionID, err := conn.NewSession(ctx, create.Workspace)
	if err != nil {
		return Record{}, errors.Join(err, closeAgent(conn))
	}

	now := time.Now().UTC()
	record := Record{
		ID:             id,
		Agent:          create.Agent,
		Workspace:      create.Workspace,
		Owner:          create.Owner,
		State:          Live,
		CreatedAt:      now,
		LastActivityAt: now,
	}
	if err := insertRecord(ctx, m.db, record, agentSessionID); err != nil {
		return Record{}, errors.Join(err, closeAgent(conn))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		_, demoteErr := m.db.ExecContext(ctx, `UPDATE sessions SET state = ? WHERE id = ?`, Dormant, id)
		return Record{}, errors.Join(
			fmt.Errorf("create Session: %w", ErrClosed),
			closeAgent(conn),
			annotate("demote interrupted Session", demoteErr),
		)
	}
	m.sessions[id] = &managedSession{record: record, agentSessionID: agentSessionID, conn: conn}
	return record, nil
}

// Prompt queues one Prompt for a Session, transparently resuming a dormant
// Agent connection before dispatch. Concurrent calls are processed in the
// order they enter the Session's queue.
func (m *Manager) Prompt(ctx context.Context, id, text string) (agent.StopReason, error) {
	call := &promptCall{ctx: ctx, text: text, result: make(chan promptResult, 1)}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", fmt.Errorf("prompt Session %q: %w", id, ErrClosed)
	}
	managed, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("prompt Session %q: %w", id, sql.ErrNoRows)
	}
	managed.mu.Lock()
	managed.queue = append(managed.queue, call)
	if !managed.working {
		managed.working = true
		m.workers.Add(1)
		go m.work(id, managed)
	}
	managed.mu.Unlock()
	m.mu.Unlock()

	select {
	case result := <-call.result:
		return result.stop, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (m *Manager) work(id string, managed *managedSession) {
	defer m.workers.Done()
	for {
		managed.mu.Lock()
		if len(managed.queue) == 0 {
			managed.working = false
			managed.mu.Unlock()
			return
		}
		call := managed.queue[0]
		managed.queue[0] = nil
		managed.queue = managed.queue[1:]
		managed.mu.Unlock()

		stop, err := m.runPrompt(call.ctx, id, managed, call.text)
		call.result <- promptResult{stop: stop, err: err}
	}
}

func (m *Manager) runPrompt(ctx context.Context, id string, managed *managedSession, text string) (agent.StopReason, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	stopLifecycleCancel := context.AfterFunc(m.ctx, cancel)
	defer func() {
		stopLifecycleCancel()
		cancel()
	}()
	if m.ctx.Err() != nil {
		return "", ErrClosed
	}

	conn, err := m.liveConnection(turnCtx, id, managed)
	if err != nil {
		if m.ctx.Err() != nil {
			return "", ErrClosed
		}
		return "", err
	}
	stop, err := conn.Prompt(turnCtx, managed.agentSessionID, text)
	if m.ctx.Err() != nil {
		return "", ErrClosed
	}
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	if _, err := m.db.ExecContext(turnCtx, `UPDATE sessions SET last_activity_at = ? WHERE id = ?`, now.UnixNano(), id); err != nil {
		if m.ctx.Err() != nil {
			return "", ErrClosed
		}
		return "", fmt.Errorf("record Session %q activity: %w", id, err)
	}
	managed.mu.Lock()
	managed.record.LastActivityAt = now
	managed.mu.Unlock()
	return stop, nil
}

func (m *Manager) liveConnection(ctx context.Context, id string, managed *managedSession) (*agent.Conn, error) {
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closing {
		return nil, ErrClosed
	}
	if managed.conn != nil {
		return managed.conn, nil
	}

	var resuming atomic.Bool
	resuming.Store(true)
	conn, err := m.connect(ctx, managed.record.Agent, func(eventCtx context.Context, _ string, event agent.Event) error {
		if resuming.Load() {
			return nil
		}
		return m.channel.Send(eventCtx, channel.Event{SessionID: id, AgentEvent: event})
	})
	if err != nil {
		return nil, fmt.Errorf("connect Agent %q: %w", managed.record.Agent, err)
	}
	if err := conn.ResumeSession(ctx, managed.agentSessionID, managed.record.Workspace); err != nil {
		return nil, errors.Join(err, closeAgent(conn))
	}
	resuming.Store(false)
	if _, err := m.db.ExecContext(ctx, `UPDATE sessions SET state = ? WHERE id = ?`, Live, id); err != nil {
		return nil, errors.Join(
			fmt.Errorf("mark Session %q live: %w", id, err),
			closeAgent(conn),
		)
	}
	managed.conn = conn
	managed.record.State = Live
	return conn, nil
}

// Get returns the latest durable record for id.
func (m *Manager) Get(_ context.Context, id string) (Record, error) {
	m.mu.Lock()
	managed, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return Record{}, fmt.Errorf("get Session %q: %w", id, sql.ErrNoRows)
	}
	managed.mu.Lock()
	defer managed.mu.Unlock()
	return managed.record, nil
}

// Close drops pending work, closes live Agent connections, demotes every live
// Session to dormant, and closes the database.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	var pending []*promptCall
	var connections []*agent.Conn
	for _, managed := range m.sessions {
		managed.mu.Lock()
		managed.closing = true
		pending = append(pending, managed.queue...)
		managed.queue = nil
		if managed.conn != nil {
			connections = append(connections, managed.conn)
			managed.conn = nil
		}
		managed.mu.Unlock()
	}
	m.mu.Unlock()

	for _, call := range pending {
		call.result <- promptResult{err: ErrClosed}
	}
	var closeErr error
	for _, conn := range connections {
		closeErr = errors.Join(closeErr, closeAgent(conn))
	}
	m.workers.Wait()
	if _, err := m.db.Exec(`UPDATE sessions SET state = ? WHERE state = ?`, Dormant, Live); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("demote live Sessions: %w", err))
	} else {
		for _, managed := range m.sessions {
			managed.mu.Lock()
			if managed.record.State == Live {
				managed.record.State = Dormant
			}
			managed.mu.Unlock()
		}
	}
	return errors.Join(closeErr, closeDatabase(m.db))
}

func (m *Manager) load(ctx context.Context) (returnErr error) {
	if _, err := m.db.ExecContext(ctx, `UPDATE sessions SET state = ? WHERE state = ?`, Dormant, Live); err != nil {
		return fmt.Errorf("recover live Sessions: %w", err)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT id, agent_session_id, agent, workspace, owner_channel, owner_id,
		       state, created_at, last_activity_at
		FROM sessions`)
	if err != nil {
		return fmt.Errorf("load Sessions: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, annotate("close Session rows", rows.Close())) }()

	for rows.Next() {
		var managed managedSession
		var state string
		var createdAt, lastActivityAt int64
		if err := rows.Scan(
			&managed.record.ID,
			&managed.agentSessionID,
			&managed.record.Agent,
			&managed.record.Workspace,
			&managed.record.Owner.Channel,
			&managed.record.Owner.ID,
			&state,
			&createdAt,
			&lastActivityAt,
		); err != nil {
			return fmt.Errorf("read Session record: %w", err)
		}
		managed.record.State = State(state)
		managed.record.CreatedAt = time.Unix(0, createdAt).UTC()
		managed.record.LastActivityAt = time.Unix(0, lastActivityAt).UTC()
		m.sessions[managed.record.ID] = &managed
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load Sessions: %w", err)
	}
	return nil
}

func validateCreate(create Create) error {
	if strings.TrimSpace(create.Agent) == "" {
		return fmt.Errorf("agent is required")
	}
	if !filepath.IsAbs(create.Workspace) {
		return fmt.Errorf("workspace must be an absolute path, got %q", create.Workspace)
	}
	if strings.TrimSpace(create.Owner.Channel) == "" || strings.TrimSpace(create.Owner.ID) == "" {
		return fmt.Errorf("owner identity requires a Channel and ID")
	}
	return nil
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func closeAgent(conn *agent.Conn) error {
	return annotate("close Agent", conn.Close())
}

func closeDatabase(db *sql.DB) error {
	return annotate("close Session database", db.Close())
}

func annotate(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
