package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/permission"
	"github.com/aesoteric/aethos/internal/sessionstate"
)

// State is the persisted lifecycle state of a Session.
type State = sessionstate.State

const (
	Live    = sessionstate.Live
	Dormant = sessionstate.Dormant
	Closed  = sessionstate.Closed
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
	TopicID   int64
}

// Record is the durable, caller-visible state of a Session.
type Record struct {
	ID             string
	Name           string
	Agent          string
	Workspace      string
	Owner          Owner
	TopicID        int64
	State          State
	CreatedAt      time.Time
	LastActivityAt time.Time
}

// Connect starts one configured Agent and attaches an event handler to it.
// Production supplies a subprocess connector; flow tests supply the scripted
// ACP Agent connector.
type Connect func(context.Context, string, agent.Handlers) (*agent.Conn, error)

// Option configures Session lifecycle behavior when a Manager opens.
type Option func(*openOptions) error

type openOptions struct {
	idleTimeout       time.Duration
	permissionTimeout time.Duration
	autoApprove       []string
}

// WithIdleTimeout demotes live Sessions after they have had no Prompt work for
// timeout. A zero timeout disables automatic idle demotion.
func WithIdleTimeout(timeout time.Duration) Option {
	return func(options *openOptions) error {
		if timeout < 0 {
			return fmt.Errorf("idle timeout cannot be negative")
		}
		options.idleTimeout = timeout
		return nil
	}
}

// WithPermissionTimeout sets the fail-safe deadline for unanswered Agent
// permission requests.
func WithPermissionTimeout(timeout time.Duration) Option {
	return func(options *openOptions) error {
		if timeout <= 0 {
			return fmt.Errorf("permission timeout must be greater than zero")
		}
		options.permissionTimeout = timeout
		return nil
	}
}

// WithAutoApprove permits the listed exact Agent tool kinds without surfacing
// a request to the Channel.
func WithAutoApprove(kinds ...string) Option {
	return func(options *openOptions) error {
		options.autoApprove = append([]string(nil), kinds...)
		return nil
	}
}

// ErrClosed is returned to in-flight and queued Prompts when the Session
// manager shuts down. Queued Prompts are intentionally not durable.
var ErrClosed = errors.New("session manager is closed")

// ErrNoPrompt is returned when cancellation is requested for a Session that
// has no Prompt in flight.
var ErrNoPrompt = errors.New("session has no Prompt in flight")

// ErrSessionClosed is returned when an operation would resume or mutate an
// explicitly archived Session.
var ErrSessionClosed = errors.New("session is closed")

// ErrInvalidTransition is returned when a lifecycle operation attempts an
// edge outside the live/dormant/closed state machine.
var ErrInvalidTransition = errors.New("invalid Session state transition")

// Manager owns durable Session records, their live Agent connections, and
// prompt serialization.
type Manager struct {
	db      *sql.DB
	connect Connect
	channel channel.Channel
	ctx     context.Context
	cancel  context.CancelFunc

	idleTimeout time.Duration
	permissions *permission.Gate

	mu            sync.Mutex
	sessions      map[string]*managedSession
	closed        bool
	backgroundErr error
	workers       sync.WaitGroup
}

type managedSession struct {
	mu             sync.Mutex
	record         Record
	agentSessionID string
	conn           *agent.Conn
	queue          []*promptCall
	current        *promptCall
	working        bool
	closing        bool
	idleTimer      *time.Timer
}

type promptCall struct {
	ctx    context.Context
	text   string
	result chan promptResult
	done   chan struct{}
	cancel atomic.Bool
	closed atomic.Bool
}

type promptResult struct {
	stop agent.StopReason
	err  error
}

// Open opens the Session database, applies versioned migrations, and recovers
// records left live by an interrupted process as dormant.
func Open(ctx context.Context, databasePath string, connect Connect, ch channel.Channel, option ...Option) (*Manager, error) {
	if !filepath.IsAbs(databasePath) {
		return nil, fmt.Errorf("database path must be absolute, got %q", databasePath)
	}
	if connect == nil {
		return nil, fmt.Errorf("agent connector is required")
	}
	if ch == nil {
		return nil, fmt.Errorf("channel is required")
	}
	options := openOptions{permissionTimeout: permission.DefaultTimeout}
	for _, configure := range option {
		if configure == nil {
			return nil, fmt.Errorf("session option is required")
		}
		if err := configure(&options); err != nil {
			return nil, fmt.Errorf("configure Session manager: %w", err)
		}
	}

	db, err := openDatabase(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	var manager *Manager
	gate, err := permission.New(
		options.permissionTimeout,
		options.autoApprove,
		func(eventCtx context.Context, sessionID string, request agent.PermissionRequested) error {
			return ch.Send(eventCtx, channel.Event{SessionID: sessionID, AgentEvent: request})
		},
		func(err error) { manager.recordBackgroundError(err) },
	)
	if err != nil {
		return nil, errors.Join(err, closeDatabase(db))
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	manager = &Manager{
		db:          db,
		connect:     connect,
		channel:     ch,
		ctx:         lifecycle,
		cancel:      cancel,
		idleTimeout: options.idleTimeout,
		permissions: gate,
		sessions:    make(map[string]*managedSession),
	}
	if err := manager.load(ctx); err != nil {
		cancel()
		return nil, errors.Join(err, closeDatabase(db))
	}
	return manager, nil
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
	conn, err := m.connect(ctx, create.Agent, agent.Handlers{
		Event: func(eventCtx context.Context, _ string, event agent.Event) error {
			return m.channel.Send(eventCtx, channel.Event{SessionID: id, AgentEvent: event})
		},
		Permission: m.permissionHandler(id),
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
		TopicID:        create.TopicID,
		State:          Live,
		CreatedAt:      now,
		LastActivityAt: now,
	}
	if err := insertRecord(ctx, m.db, record, agentSessionID); err != nil {
		return Record{}, errors.Join(err, closeAgent(conn))
	}

	m.mu.Lock()
	if m.closed {
		_, demoteErr := m.db.ExecContext(ctx, `UPDATE sessions SET state = ? WHERE id = ?`, Dormant, id)
		m.mu.Unlock()
		return Record{}, errors.Join(
			fmt.Errorf("create Session: %w", ErrClosed),
			closeAgent(conn),
			annotate("demote interrupted Session", demoteErr),
		)
	}
	managed := &managedSession{record: record, agentSessionID: agentSessionID, conn: conn}
	m.sessions[id] = managed
	managed.mu.Lock()
	m.armIdleLocked(id, managed)
	managed.mu.Unlock()
	m.mu.Unlock()
	m.surfaceState(id, Live)
	go m.watchConnection(id, managed, conn)
	return record, nil
}

// Prompt queues one Prompt for a Session, transparently resuming a dormant
// Agent connection before dispatch. Concurrent calls are processed in the
// order they enter the Session's queue.
func (m *Manager) Prompt(ctx context.Context, id, text string) (agent.StopReason, error) {
	call := &promptCall{
		ctx:    ctx,
		text:   text,
		result: make(chan promptResult, 1),
		done:   make(chan struct{}),
	}

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
	if managed.record.State == Closed {
		managed.mu.Unlock()
		m.mu.Unlock()
		return "", fmt.Errorf("prompt Session %q: %w", id, ErrSessionClosed)
	}
	m.stopIdleLocked(managed)
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
			m.armIdleLocked(id, managed)
			managed.mu.Unlock()
			return
		}
		call := managed.queue[0]
		managed.queue[0] = nil
		managed.queue = managed.queue[1:]
		managed.current = call
		managed.mu.Unlock()

		stop, err := m.runPrompt(call.ctx, id, managed, call)
		call.result <- promptResult{stop: stop, err: err}
		managed.mu.Lock()
		managed.current = nil
		close(call.done)
		managed.mu.Unlock()
	}
}

// Cancel stops the Prompt currently running for id and waits until the
// Session worker has released it so another Prompt can begin immediately.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("cancel Prompt for Session %q: %w", id, ErrClosed)
	}
	managed, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("cancel Prompt for Session %q: %w", id, sql.ErrNoRows)
	}
	managed.mu.Lock()
	m.mu.Unlock()
	if managed.record.State == Closed {
		managed.mu.Unlock()
		return fmt.Errorf("cancel Prompt for Session %q: %w", id, ErrSessionClosed)
	}
	call := managed.current
	conn := managed.conn
	if call == nil || conn == nil {
		managed.mu.Unlock()
		return fmt.Errorf("cancel Prompt for Session %q: %w", id, ErrNoPrompt)
	}
	agentSessionID := managed.agentSessionID
	call.cancel.Store(true)
	if err := conn.Cancel(ctx, agentSessionID); err != nil {
		call.cancel.Store(false)
		managed.mu.Unlock()
		return fmt.Errorf("cancel Prompt for Session %q: %w", id, err)
	}
	managed.mu.Unlock()
	select {
	case <-call.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ResolvePermission answers one pending Agent permission request. Repeating a
// completed answer is idempotent.
func (m *Manager) ResolvePermission(ctx context.Context, response channel.PermissionResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.permissions.Resolve(response.RequestID, response.OptionID)
}

func (m *Manager) permissionHandler(id string) agent.PermissionHandler {
	return func(permissionCtx context.Context, _ string, request agent.PermissionRequest) (agent.PermissionDecision, error) {
		result, err := m.permissions.Request(permissionCtx, id, request)
		if err != nil {
			return agent.PermissionDecision{}, err
		}
		if result.NotifyOutcome {
			if sendErr := m.channel.Send(m.ctx, channel.Event{
				SessionID: id,
				AgentEvent: agent.PermissionResolved{
					ID:        result.RequestID,
					OptionID:  result.Decision.OptionID,
					TimedOut:  result.TimedOut,
					Cancelled: result.Decision.Cancelled,
				},
			}); sendErr != nil && m.ctx.Err() == nil {
				m.recordBackgroundError(fmt.Errorf("finish permission request for Session %q: %w", id, sendErr))
			}
		}
		return result.Decision, nil
	}
}

func (m *Manager) recordBackgroundError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backgroundErr = errors.Join(m.backgroundErr, err)
}

func (m *Manager) runPrompt(ctx context.Context, id string, managed *managedSession, call *promptCall) (stop agent.StopReason, returnErr error) {
	turnCtx, cancel := context.WithCancel(ctx)
	stopLifecycleCancel := context.AfterFunc(m.ctx, cancel)
	defer func() {
		stopLifecycleCancel()
		cancel()
	}()
	if m.ctx.Err() != nil {
		return "", ErrClosed
	}
	if err := m.sendLifecycle(turnCtx, channel.LifecycleEvent{
		SessionID:    id,
		SessionEvent: channel.PromptStarted{Prompt: call.text},
	}); err != nil {
		return "", err
	}
	defer func() {
		finished := channel.PromptFinished{StopReason: stop}
		if returnErr != nil {
			finished.Error = returnErr.Error()
		}
		if err := m.sendLifecycle(m.ctx, channel.LifecycleEvent{
			SessionID:    id,
			SessionEvent: finished,
		}); err != nil && m.ctx.Err() == nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("finish Prompt lifecycle for Session %q: %w", id, err))
		}
	}()

	conn, err := m.liveConnection(turnCtx, id, managed)
	if err != nil {
		if m.ctx.Err() != nil {
			return "", ErrClosed
		}
		return "", err
	}
	stop, err = conn.Prompt(turnCtx, managed.agentSessionID, call.text)
	if m.ctx.Err() != nil {
		return "", ErrClosed
	}
	if call.closed.Load() {
		return "", ErrSessionClosed
	}
	if err != nil {
		if call.cancel.Load() {
			return "", context.Canceled
		}
		select {
		case <-conn.Done():
			m.connectionEnded(id, managed, conn)
		default:
		}
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

func (m *Manager) sendLifecycle(ctx context.Context, event channel.LifecycleEvent) error {
	lifecycle, ok := m.channel.(channel.LifecycleChannel)
	if !ok {
		return nil
	}
	return lifecycle.SendLifecycle(ctx, event)
}

func (m *Manager) surfaceState(id string, state State) {
	if err := m.sendLifecycle(m.ctx, channel.LifecycleEvent{
		SessionID:    id,
		SessionEvent: channel.SessionStateChanged{State: state},
	}); err != nil && m.ctx.Err() == nil {
		m.recordBackgroundError(fmt.Errorf("surface %s state for Session %q: %w", state, id, err))
	}
}

func (m *Manager) liveConnection(ctx context.Context, id string, managed *managedSession) (*agent.Conn, error) {
	managed.mu.Lock()
	if managed.closing {
		managed.mu.Unlock()
		return nil, ErrSessionClosed
	}
	if managed.conn != nil {
		conn := managed.conn
		managed.mu.Unlock()
		return conn, nil
	}
	if !managed.record.State.CanTransitionTo(Live) {
		state := managed.record.State
		managed.mu.Unlock()
		return nil, fmt.Errorf("resume Session %q from %q: %w", id, state, ErrInvalidTransition)
	}

	var resuming atomic.Bool
	resuming.Store(true)
	conn, err := m.connect(ctx, managed.record.Agent, agent.Handlers{
		Event: func(eventCtx context.Context, _ string, event agent.Event) error {
			if resuming.Load() {
				return nil
			}
			return m.channel.Send(eventCtx, channel.Event{SessionID: id, AgentEvent: event})
		},
		Permission: m.permissionHandler(id),
	})
	if err != nil {
		connectErr := fmt.Errorf("connect Agent %q: %w", managed.record.Agent, err)
		managed.mu.Unlock()
		return nil, connectErr
	}
	if err := conn.ResumeSession(ctx, managed.agentSessionID, managed.record.Workspace); err != nil {
		managed.mu.Unlock()
		return nil, errors.Join(err, closeAgent(conn))
	}
	resuming.Store(false)
	if _, err := m.db.ExecContext(ctx, `UPDATE sessions SET state = ? WHERE id = ?`, Live, id); err != nil {
		managed.mu.Unlock()
		return nil, errors.Join(
			fmt.Errorf("mark Session %q live: %w", id, err),
			closeAgent(conn),
		)
	}
	managed.conn = conn
	managed.record.State = Live
	managed.mu.Unlock()
	m.surfaceState(id, Live)
	go m.watchConnection(id, managed, conn)
	return conn, nil
}

func (m *Manager) watchConnection(id string, managed *managedSession, conn *agent.Conn) {
	<-conn.Done()
	m.connectionEnded(id, managed, conn)
}

func (m *Manager) connectionEnded(id string, managed *managedSession, conn *agent.Conn) {
	m.mu.Lock()
	if m.closed || m.sessions[id] != managed {
		m.mu.Unlock()
		return
	}
	managed.mu.Lock()
	if managed.conn != conn || managed.record.State != Live {
		managed.mu.Unlock()
		m.mu.Unlock()
		return
	}

	now := time.Now().UTC()
	if _, err := m.db.ExecContext(m.ctx, `UPDATE sessions SET state = ?, last_activity_at = ? WHERE id = ?`, Dormant, now.UnixNano(), id); err != nil {
		managed.mu.Unlock()
		m.backgroundErr = errors.Join(m.backgroundErr, fmt.Errorf("demote crashed Session %q: %w", id, err))
		m.mu.Unlock()
		return
	}
	m.stopIdleLocked(managed)
	managed.conn = nil
	managed.record.State = Dormant
	managed.record.LastActivityAt = now
	managed.mu.Unlock()
	m.mu.Unlock()
	m.surfaceState(id, Dormant)

	exitErr := conn.ExitError()
	if err := closeAgent(conn); err != nil {
		m.mu.Lock()
		m.backgroundErr = errors.Join(m.backgroundErr, fmt.Errorf("clean up crashed Session %q: %w", id, err))
		m.mu.Unlock()
	}
	message := "Agent subprocess exited unexpectedly"
	if exitErr != nil {
		message = exitErr.Error()
	}
	if err := m.channel.Send(m.ctx, channel.Event{
		SessionID:  id,
		AgentEvent: agent.Crashed{Error: message},
	}); err != nil {
		m.mu.Lock()
		m.backgroundErr = errors.Join(m.backgroundErr, fmt.Errorf("surface crashed Session %q: %w", id, err))
		m.mu.Unlock()
	}
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

// FindByTopic returns the Session durably bound to a Telegram Topic.
func (m *Manager) FindByTopic(_ context.Context, topicID int64) (Record, error) {
	if topicID <= 0 {
		return Record{}, fmt.Errorf("find Session for Topic %d: %w", topicID, sql.ErrNoRows)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, managed := range m.sessions {
		managed.mu.Lock()
		if managed.record.TopicID == topicID {
			record := managed.record
			managed.mu.Unlock()
			return record, nil
		}
		managed.mu.Unlock()
	}
	return Record{}, fmt.Errorf("find Session for Topic %d: %w", topicID, sql.ErrNoRows)
}

// Rename records the user-visible name derived from a Session's first Prompt.
func (m *Manager) Rename(ctx context.Context, id, name string) (Record, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Record{}, fmt.Errorf("rename Session %q: name is required", id)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Record{}, fmt.Errorf("rename Session %q: %w", id, ErrClosed)
	}
	managed, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return Record{}, fmt.Errorf("rename Session %q: %w", id, sql.ErrNoRows)
	}
	managed.mu.Lock()
	m.mu.Unlock()
	defer managed.mu.Unlock()
	if managed.record.State == Closed {
		return Record{}, fmt.Errorf("rename Session %q: %w", id, ErrSessionClosed)
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE sessions SET name = ? WHERE id = ?`, name, id); err != nil {
		return Record{}, fmt.Errorf("rename Session %q: %w", id, err)
	}
	managed.record.Name = name
	return managed.record, nil
}

// List returns every Session, including archived Sessions, in creation order.
func (m *Manager) List(_ context.Context) ([]Record, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("list Sessions: %w", ErrClosed)
	}
	managed := make([]*managedSession, 0, len(m.sessions))
	for _, one := range m.sessions {
		managed = append(managed, one)
	}
	m.mu.Unlock()

	records := make([]Record, 0, len(managed))
	for _, one := range managed {
		one.mu.Lock()
		records = append(records, one.record)
		one.mu.Unlock()
	}
	slices.SortFunc(records, func(a, b Record) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	return records, nil
}

// CloseSession deliberately archives id, releases its Agent connection, and
// leaves the durable record available to Get and List.
func (m *Manager) CloseSession(ctx context.Context, id string) (Record, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Record{}, fmt.Errorf("close Session %q: %w", id, ErrClosed)
	}
	managed, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return Record{}, fmt.Errorf("close Session %q: %w", id, sql.ErrNoRows)
	}
	managed.mu.Lock()
	m.mu.Unlock()
	if !managed.record.State.CanTransitionTo(Closed) {
		state := managed.record.State
		managed.mu.Unlock()
		return Record{}, fmt.Errorf("close Session %q from %q: %w", id, state, ErrInvalidTransition)
	}

	now := time.Now().UTC()
	if _, err := m.db.ExecContext(ctx, `UPDATE sessions SET state = ?, last_activity_at = ? WHERE id = ?`, Closed, now.UnixNano(), id); err != nil {
		managed.mu.Unlock()
		return Record{}, fmt.Errorf("archive Session %q: %w", id, err)
	}
	conn := managed.conn
	current := managed.current
	if current != nil {
		current.closed.Store(true)
	}
	pending := managed.queue
	managed.queue = nil
	for _, call := range pending {
		call.closed.Store(true)
	}
	managed.conn = nil
	managed.closing = true
	m.stopIdleLocked(managed)
	managed.record.State = Closed
	managed.record.LastActivityAt = now
	record := managed.record
	managed.mu.Unlock()

	for _, call := range pending {
		call.result <- promptResult{err: ErrSessionClosed}
		close(call.done)
	}
	var closeErr error
	if conn != nil {
		if err := closeAgent(conn); err != nil {
			closeErr = err
		}
	}
	if current != nil {
		select {
		case <-current.done:
		case <-ctx.Done():
			closeErr = errors.Join(closeErr, ctx.Err())
		}
	}
	if err := m.sendLifecycle(m.ctx, channel.LifecycleEvent{
		SessionID:    id,
		SessionEvent: channel.SessionStateChanged{State: Closed},
	}); err != nil && m.ctx.Err() == nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("surface closed state for Session %q: %w", id, err))
	}
	return record, closeErr
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
		m.stopIdleLocked(managed)
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
		close(call.done)
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
	m.mu.Lock()
	backgroundErr := m.backgroundErr
	m.mu.Unlock()
	return errors.Join(closeErr, backgroundErr, closeDatabase(m.db))
}

func (m *Manager) armIdleLocked(id string, managed *managedSession) {
	if m.idleTimeout == 0 || managed.closing || managed.record.State != Live || managed.conn == nil {
		return
	}
	m.stopIdleLocked(managed)
	managed.idleTimer = time.AfterFunc(m.idleTimeout, func() {
		m.demoteIdle(id, managed)
	})
}

func (m *Manager) stopIdleLocked(managed *managedSession) {
	if managed.idleTimer != nil {
		managed.idleTimer.Stop()
		managed.idleTimer = nil
	}
}

func (m *Manager) demoteIdle(id string, managed *managedSession) {
	m.mu.Lock()
	if m.closed || m.sessions[id] != managed {
		m.mu.Unlock()
		return
	}
	managed.mu.Lock()
	managed.idleTimer = nil
	if managed.working || len(managed.queue) != 0 || managed.record.State != Live || managed.conn == nil {
		m.armIdleLocked(id, managed)
		managed.mu.Unlock()
		m.mu.Unlock()
		return
	}
	if _, err := m.db.ExecContext(m.ctx, `UPDATE sessions SET state = ? WHERE id = ?`, Dormant, id); err != nil {
		m.armIdleLocked(id, managed)
		managed.mu.Unlock()
		m.backgroundErr = errors.Join(m.backgroundErr, fmt.Errorf("demote idle Session %q: %w", id, err))
		m.mu.Unlock()
		return
	}
	conn := managed.conn
	managed.conn = nil
	managed.record.State = Dormant
	managed.mu.Unlock()
	m.mu.Unlock()

	if err := closeAgent(conn); err != nil {
		m.mu.Lock()
		m.backgroundErr = errors.Join(m.backgroundErr, fmt.Errorf("demote idle Session %q: %w", id, err))
		m.mu.Unlock()
	}
	m.surfaceState(id, Dormant)
}

func (m *Manager) load(ctx context.Context) (returnErr error) {
	if _, err := m.db.ExecContext(ctx, `UPDATE sessions SET state = ? WHERE state = ?`, Dormant, Live); err != nil {
		return fmt.Errorf("recover live Sessions: %w", err)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT id, name, agent_session_id, agent, workspace, owner_channel, owner_id,
		       topic_id, state, created_at, last_activity_at
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
			&managed.record.Name,
			&managed.agentSessionID,
			&managed.record.Agent,
			&managed.record.Workspace,
			&managed.record.Owner.Channel,
			&managed.record.Owner.ID,
			&managed.record.TopicID,
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
	if create.TopicID < 0 {
		return fmt.Errorf("topic ID cannot be negative")
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
