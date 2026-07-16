// Package rest implements the authenticated HTTP Channel used by automation
// clients to control aethos Sessions.
package rest

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/permission"
	"github.com/aesoteric/aethos/internal/session"
	"github.com/aesoteric/aethos/internal/sessionstate"
)

const maxRequestBody = 1 << 20

// Settings contains the network and identity values needed by the REST
// Channel at runtime.
type Settings struct {
	ListenAddress string
	BearerToken   string
	Identity      string
}

type sessionTarget interface {
	Cancel(context.Context, string) error
	Create(context.Context, session.Create) (session.Record, error)
	Get(context.Context, string) (session.Record, error)
	List(context.Context) ([]session.Record, error)
	Prompt(context.Context, string, string) (agent.StopReason, error)
	ResolvePermission(context.Context, channel.PermissionResponse) error
}

// Channel translates authenticated HTTP requests into Session operations.
type Channel struct {
	settings Settings
	logger   *slog.Logger

	streamMu       sync.Mutex
	streams        map[string]map[uint64]*eventSubscriber
	nextSubscriber uint64
}

type eventSubscriber struct {
	events chan sseEvent
	done   <-chan struct{}
}

type sseEvent struct {
	name     string
	data     []byte
	terminal bool
}

type permissionOptionEvent struct {
	ID   string                     `json:"id"`
	Name string                     `json:"name"`
	Kind agent.PermissionOptionKind `json:"kind"`
}

// New validates settings and prepares a REST Channel.
func New(settings Settings, logger *slog.Logger) (*Channel, error) {
	if strings.TrimSpace(settings.ListenAddress) == "" {
		return nil, fmt.Errorf("REST listen address is required")
	}
	if strings.TrimSpace(settings.BearerToken) == "" {
		return nil, fmt.Errorf("REST bearer token is required")
	}
	if strings.TrimSpace(settings.Identity) == "" {
		return nil, fmt.Errorf("REST API identity is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	return &Channel{
		settings: settings,
		logger:   logger,
		streams:  make(map[string]map[uint64]*eventSubscriber),
	}, nil
}

// Run listens for automation requests until ctx is cancelled.
func (c *Channel) Run(ctx context.Context, sessions sessionTarget) error {
	if sessions == nil {
		return fmt.Errorf("session target is required")
	}
	listener, err := net.Listen("tcp", c.settings.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for REST Channel on %q: %w", c.settings.ListenAddress, err)
	}
	server := &http.Server{
		Handler:           c.Handler(sessions),
		ReadHeaderTimeout: 5 * time.Second,
	}
	c.logger.Info("REST Channel listening", "address", listener.Addr().String())
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve REST Channel: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, server.Close())
		}
		serveErr := <-served
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(shutdownErr, serveErr)
	}
}

// Handler returns the real HTTP handler for the supplied Session target.
func (c *Channel) Handler(sessions sessionTarget) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			c.health(w, r, sessions)
			return
		}
		if !c.authenticated(r) {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			c.createSession(w, r, sessions)
		case r.Method == http.MethodGet && r.URL.Path == "/sessions":
			c.listSessions(w, r, sessions)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			c.streamSession(w, r, sessions)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/permissions/"):
			c.answerPermission(w, r, sessions)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/sessions/"):
			c.promptSession(w, r, sessions)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sessions/"):
			c.getSession(w, r, sessions)
		default:
			writeError(w, http.StatusNotFound, "endpoint not found")
		}
	})
}

func (c *Channel) answerPermission(w http.ResponseWriter, r *http.Request, sessions sessionTarget) {
	requestID := strings.TrimPrefix(r.URL.Path, "/permissions/")
	if requestID == "" || strings.Contains(requestID, "/") {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	var input struct {
		OptionID string `json:"option_id"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(input.OptionID) == "" {
		writeError(w, http.StatusBadRequest, "option_id is required")
		return
	}
	if err := sessions.ResolvePermission(r.Context(), channel.PermissionResponse{
		RequestID: requestID,
		OptionID:  input.OptionID,
	}); err != nil {
		switch {
		case errors.Is(err, permission.ErrUnknownRequest):
			writeError(w, http.StatusNotFound, "permission request not found")
		case errors.Is(err, permission.ErrUnknownOption):
			writeError(w, http.StatusBadRequest, "permission option was not offered")
		default:
			writeSessionError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Message string `json:"message"`
	}{Message: "Permission response recorded"})
}

func (c *Channel) streamSession(w http.ResponseWriter, r *http.Request, sessions sessionTarget) {
	id, action, ok := sessionAction(r.URL.Path)
	if !ok || action != "events" {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	record, err := sessions.Get(r.Context(), id)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	if record.State == session.Closed {
		writeError(w, http.StatusConflict, "Session is closed")
		return
	}

	subscriberID, subscriber := c.subscribe(id, r.Context().Done())
	defer c.unsubscribe(id, subscriberID)
	record, err = sessions.Get(r.Context(), id)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	if record.State == session.Closed {
		writeError(w, http.StatusConflict, "Session is closed")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "event streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = io.WriteString(w, ": connected; events are live-only and are not replayed\n\n")
	flusher.Flush()

	for {
		select {
		case event := <-subscriber.events:
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, event.data); err != nil {
				return
			}
			flusher.Flush()
			if event.terminal {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (c *Channel) subscribe(sessionID string, done <-chan struct{}) (uint64, *eventSubscriber) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	c.nextSubscriber++
	id := c.nextSubscriber
	subscriber := &eventSubscriber{events: make(chan sseEvent, 32), done: done}
	if c.streams[sessionID] == nil {
		c.streams[sessionID] = make(map[uint64]*eventSubscriber)
	}
	c.streams[sessionID][id] = subscriber
	return id, subscriber
}

func (c *Channel) unsubscribe(sessionID string, subscriberID uint64) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	subscribers := c.streams[sessionID]
	delete(subscribers, subscriberID)
	if len(subscribers) == 0 {
		delete(c.streams, sessionID)
	}
}

func (c *Channel) publish(ctx context.Context, sessionID string, event sseEvent) error {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	for id, subscriber := range c.streams[sessionID] {
		select {
		case subscriber.events <- event:
		case <-subscriber.done:
			delete(c.streams[sessionID], id)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if len(c.streams[sessionID]) == 0 {
		delete(c.streams, sessionID)
	}
	return nil
}

func (c *Channel) health(w http.ResponseWriter, r *http.Request, sessions sessionTarget) {
	if _, err := sessions.List(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}{Status: "not_ready", Error: "Session control is unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ready"})
}

func (c *Channel) promptSession(w http.ResponseWriter, r *http.Request, sessions sessionTarget) {
	id, action, ok := sessionAction(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	if action == "cancel" {
		c.cancelPrompt(w, r, sessions, id)
		return
	}
	if action != "prompt" {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	var input struct {
		Prompt string `json:"prompt"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(input.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	stop, err := sessions.Prompt(r.Context(), id, input.Prompt)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		StopReason agent.StopReason `json:"stop_reason"`
	}{StopReason: stop})
}

func (c *Channel) cancelPrompt(w http.ResponseWriter, r *http.Request, sessions sessionTarget, id string) {
	if err := sessions.Cancel(r.Context(), id); err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Message string `json:"message"`
	}{Message: "Prompt cancelled"})
}

func sessionAction(path string) (id, action string, ok bool) {
	remainder := strings.TrimPrefix(path, "/sessions/")
	id, action, ok = strings.Cut(remainder, "/")
	return id, action, ok && id != "" && action != "" && !strings.Contains(action, "/")
}

func (c *Channel) getSession(w http.ResponseWriter, r *http.Request, sessions sessionTarget) {
	id := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	record, err := sessions.Get(r.Context(), id)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionJSON(record))
}

func (c *Channel) createSession(w http.ResponseWriter, r *http.Request, sessions sessionTarget) {
	var input struct {
		Agent     string `json:"agent"`
		Workspace string `json:"workspace"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(input.Agent) == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}
	if !filepath.IsAbs(input.Workspace) {
		writeError(w, http.StatusBadRequest, "workspace must be an absolute path")
		return
	}
	record, err := sessions.Create(r.Context(), session.Create{
		Agent:     input.Agent,
		Workspace: input.Workspace,
		Owner: session.Owner{
			Channel: "rest",
			ID:      c.settings.Identity,
		},
	})
	if err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sessionJSON(record))
}

func (c *Channel) listSessions(w http.ResponseWriter, r *http.Request, sessions sessionTarget) {
	records, err := sessions.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	response := make([]sessionResponse, 0, len(records))
	for _, record := range records {
		response = append(response, sessionJSON(record))
	}
	writeJSON(w, http.StatusOK, struct {
		Sessions []sessionResponse `json:"sessions"`
	}{Sessions: response})
}

func (c *Channel) authenticated(r *http.Request) bool {
	scheme, token, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	token = strings.TrimSpace(token)
	want := []byte(c.settings.BearerToken)
	got := []byte(token)
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid JSON body: multiple values")
	}
	return nil
}

type ownerResponse struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
}

type sessionResponse struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Agent          string        `json:"agent"`
	Workspace      string        `json:"workspace"`
	Owner          ownerResponse `json:"owner"`
	State          session.State `json:"state"`
	CreatedAt      time.Time     `json:"created_at"`
	LastActivityAt time.Time     `json:"last_activity_at"`
}

func sessionJSON(record session.Record) sessionResponse {
	return sessionResponse{
		ID:             record.ID,
		Name:           record.Name,
		Agent:          record.Agent,
		Workspace:      record.Workspace,
		Owner:          ownerResponse{Channel: record.Owner.Channel, ID: record.Owner.ID},
		State:          record.State,
		CreatedAt:      record.CreatedAt,
		LastActivityAt: record.LastActivityAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: message})
}

func writeSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "Session not found")
	case errors.Is(err, context.Canceled), errors.Is(err, session.ErrNoPrompt), errors.Is(err, session.ErrSessionClosed), errors.Is(err, session.ErrInvalidTransition):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// Send implements channel.Channel by publishing Agent events to clients that
// are currently observing the event's Session.
func (c *Channel) Send(ctx context.Context, event channel.Event) error {
	outbound, ok, err := agentServerEvent(event.AgentEvent)
	if err != nil {
		return fmt.Errorf("encode REST event for Session %q: %w", event.SessionID, err)
	}
	if !ok {
		return nil
	}
	return c.publish(ctx, event.SessionID, outbound)
}

// SendLifecycle implements channel.LifecycleChannel for the REST Channel.
func (c *Channel) SendLifecycle(ctx context.Context, event channel.LifecycleEvent) error {
	outbound, err := lifecycleServerEvent(event.SessionEvent)
	if err != nil {
		return fmt.Errorf("encode REST lifecycle event for Session %q: %w", event.SessionID, err)
	}
	return c.publish(ctx, event.SessionID, outbound)
}

func lifecycleServerEvent(event channel.SessionEvent) (sseEvent, error) {
	var name string
	var payload any
	terminal := false
	switch one := event.(type) {
	case channel.PromptStarted:
		name = "prompt_started"
		payload = struct {
			Prompt string `json:"prompt"`
		}{Prompt: one.Prompt}
	case channel.PromptFinished:
		name = "prompt_finished"
		payload = struct {
			StopReason agent.StopReason `json:"stop_reason,omitempty"`
			Error      string           `json:"error,omitempty"`
		}{StopReason: one.StopReason, Error: one.Error}
	case channel.SessionStateChanged:
		name = "session_state_changed"
		terminal = one.State == sessionstate.Closed
		payload = struct {
			State sessionstate.State `json:"state"`
		}{State: one.State}
	default:
		return sseEvent{}, fmt.Errorf("unsupported lifecycle event %T", event)
	}
	data, err := json.Marshal(payload)
	return sseEvent{name: name, data: data, terminal: terminal}, err
}

func agentServerEvent(event agent.Event) (sseEvent, bool, error) {
	var name string
	var payload any
	switch one := event.(type) {
	case agent.Thought:
		name = "thought"
		payload = struct {
			Text string `json:"text"`
		}{Text: one.Text}
	case agent.Message:
		name = "message"
		payload = struct {
			Text string `json:"text"`
		}{Text: one.Text}
	case agent.ToolCallBegan:
		name = "tool_call_began"
		payload = struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Kind   string `json:"kind,omitempty"`
			Status string `json:"status,omitempty"`
		}{ID: one.ID, Title: one.Title, Kind: one.Kind, Status: one.Status}
	case agent.ToolCallProgressed:
		name = "tool_call_progressed"
		payload = struct {
			ID     string `json:"id"`
			Title  string `json:"title,omitempty"`
			Status string `json:"status,omitempty"`
		}{ID: one.ID, Title: one.Title, Status: one.Status}
	case agent.PermissionRequested:
		name = "permission_requested"
		options := make([]permissionOptionEvent, 0, len(one.Options))
		for _, option := range one.Options {
			options = append(options, permissionOptionEvent{
				ID: option.ID, Name: option.Name, Kind: option.Kind,
			})
		}
		payload = struct {
			ID         string                  `json:"id"`
			ToolCallID string                  `json:"tool_call_id"`
			Title      string                  `json:"title"`
			Kind       string                  `json:"kind,omitempty"`
			Input      any                     `json:"input"`
			Options    []permissionOptionEvent `json:"options"`
		}{
			ID: one.ID, ToolCallID: one.ToolCallID, Title: one.Title,
			Kind: one.Kind, Input: one.Input, Options: options,
		}
	case agent.PermissionResolved:
		name = "permission_resolved"
		payload = struct {
			ID        string `json:"id"`
			OptionID  string `json:"option_id,omitempty"`
			TimedOut  bool   `json:"timed_out,omitempty"`
			Cancelled bool   `json:"cancelled,omitempty"`
		}{
			ID: one.ID, OptionID: one.OptionID,
			TimedOut: one.TimedOut, Cancelled: one.Cancelled,
		}
	case agent.Crashed:
		name = "crashed"
		payload = struct {
			Error string `json:"error"`
		}{Error: one.Error}
	default:
		return sseEvent{}, false, nil
	}
	data, err := json.Marshal(payload)
	return sseEvent{name: name, data: data}, true, err
}
