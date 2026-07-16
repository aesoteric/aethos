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
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/session"
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
}

// Channel translates authenticated HTTP requests into Session operations.
type Channel struct {
	settings Settings
	logger   *slog.Logger
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
	return &Channel{settings: settings, logger: logger}, nil
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
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/sessions/"):
			c.promptSession(w, r, sessions)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sessions/"):
			c.getSession(w, r, sessions)
		default:
			writeError(w, http.StatusNotFound, "endpoint not found")
		}
	})
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

// Send implements channel.Channel. Event streaming is added by the SSE
// Channel work; core REST control does not block Agent delivery.
func (c *Channel) Send(context.Context, channel.Event) error {
	return nil
}
