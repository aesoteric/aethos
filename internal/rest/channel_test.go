package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/rest"
	"github.com/aesoteric/aethos/internal/session"
)

func TestAuthenticatedClientCreatesSession(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{})
	workspace := t.TempDir()

	response := request(t, api.Handler(manager), http.MethodPost, "/sessions", "test-token", map[string]string{
		"agent":     "codex-acp",
		"workspace": workspace,
	})
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		t.Fatalf("POST /sessions status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	var body struct {
		ID        string `json:"id"`
		Agent     string `json:"agent"`
		Workspace string `json:"workspace"`
		State     string `json:"state"`
		Owner     struct {
			Channel string `json:"channel"`
			ID      string `json:"id"`
		} `json:"owner"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode created Session: %v", err)
	}
	if body.ID == "" || body.Agent != "codex-acp" || body.Workspace != workspace || body.State != "live" {
		t.Errorf("created Session = %#v, want requested live Session", body)
	}
	if body.Owner.Channel != "rest" || body.Owner.ID != "api" {
		t.Errorf("created Session owner = %#v, want REST API identity", body.Owner)
	}
}

func TestHealthReportsReadinessWithoutAuthentication(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{})

	response := request(t, api.Handler(manager), http.MethodGet, "/health", "", nil)
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if body.Status != "ready" {
		t.Errorf("health status = %q, want ready", body.Status)
	}
}

func TestHealthReportsWhenSessionControlIsNotReady(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{})
	if err := manager.Close(); err != nil {
		t.Fatalf("close Session manager: %v", err)
	}

	response := request(t, api.Handler(manager), http.MethodGet, "/health", "", nil)
	defer response.Body.Close()

	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /health status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode unready health response: %v", err)
	}
	if body.Status != "not_ready" || body.Error == "" {
		t.Errorf("unready health response = %#v, want status and human-readable error", body)
	}
}

func TestEverySessionRouteRequiresBearerToken(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{})
	handler := api.Handler(manager)

	routes := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/sessions"},
		{method: http.MethodGet, path: "/sessions"},
		{method: http.MethodGet, path: "/sessions/id"},
		{method: http.MethodPost, path: "/sessions/id/prompt"},
		{method: http.MethodPost, path: "/sessions/id/cancel"},
	}
	for _, route := range routes {
		for _, token := range []string{"", "wrong-token"} {
			response := request(t, handler, route.method, route.path, token, nil)
			if response.StatusCode != http.StatusUnauthorized {
				response.Body.Close()
				t.Errorf("%s %s with token %q status = %d, want %d", route.method, route.path, token, response.StatusCode, http.StatusUnauthorized)
				continue
			}
			if got := response.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("unauthorized Content-Type = %q, want application/json", got)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Errorf("decode unauthorized response: %v", err)
			}
			response.Body.Close()
			if body.Error == "" {
				t.Error("unauthorized response has no human-readable error")
			}
		}
	}
}

func TestAuthenticatedClientListsSessions(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{})
	handler := api.Handler(manager)
	created := request(t, handler, http.MethodPost, "/sessions", "test-token", map[string]string{
		"agent":     "codex-acp",
		"workspace": t.TempDir(),
	})
	created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create fixture Session status = %d, want %d", created.StatusCode, http.StatusCreated)
	}

	response := request(t, handler, http.MethodGet, "/sessions", "test-token", nil)
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /sessions status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var body struct {
		Sessions []struct {
			ID    string `json:"id"`
			Agent string `json:"agent"`
			State string `json:"state"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode Session list: %v", err)
	}
	if len(body.Sessions) != 1 || body.Sessions[0].ID == "" || body.Sessions[0].Agent != "codex-acp" || body.Sessions[0].State != "live" {
		t.Errorf("Session list = %#v, want created live Session", body.Sessions)
	}
}

func TestAuthenticatedClientInspectsSessionStatus(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{})
	handler := api.Handler(manager)
	created := request(t, handler, http.MethodPost, "/sessions", "test-token", map[string]string{
		"agent":     "codex-acp",
		"workspace": t.TempDir(),
	})
	var fixture struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(created.Body).Decode(&fixture); err != nil {
		created.Body.Close()
		t.Fatalf("decode fixture Session: %v", err)
	}
	created.Body.Close()

	response := request(t, handler, http.MethodGet, "/sessions/"+fixture.ID, "test-token", nil)
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /sessions/{id} status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var body struct {
		ID    string `json:"id"`
		Agent string `json:"agent"`
		State string `json:"state"`
		Owner struct {
			Channel string `json:"channel"`
			ID      string `json:"id"`
		} `json:"owner"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode Session status: %v", err)
	}
	if body.ID != fixture.ID || body.Agent != "codex-acp" || body.State != "live" || body.Owner.Channel != "rest" || body.Owner.ID != "api" {
		t.Errorf("Session status = %#v, want persisted REST Session", body)
	}
}

func TestAuthenticatedClientSendsPrompt(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{Turns: []agent.Turn{{
		WantPrompt: "ship it",
		Stop:       agent.StopEndTurn,
	}}})
	handler := api.Handler(manager)
	sessionID := createSession(t, handler, t.TempDir())

	response := request(t, handler, http.MethodPost, "/sessions/"+sessionID+"/prompt", "test-token", map[string]string{
		"prompt": "ship it",
	})
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST /sessions/{id}/prompt status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var body struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode Prompt result: %v", err)
	}
	if body.StopReason != "end_turn" {
		t.Errorf("Prompt stop reason = %q, want end_turn", body.StopReason)
	}
}

func TestAuthenticatedClientCancelsRunningPrompt(t *testing.T) {
	started := make(chan struct{}, 1)
	block := make(chan struct{})
	api, manager := openRESTFlow(t, &agent.Script{Turns: []agent.Turn{{
		WantPrompt: "keep working",
		Started:    started,
		Continue:   block,
	}}})
	handler := api.Handler(manager)
	sessionID := createSession(t, handler, t.TempDir())
	promptResult := make(chan *http.Response, 1)
	go func() {
		promptResult <- request(t, handler, http.MethodPost, "/sessions/"+sessionID+"/prompt", "test-token", map[string]string{
			"prompt": "keep working",
		})
	}()
	<-started

	cancelled := request(t, handler, http.MethodPost, "/sessions/"+sessionID+"/cancel", "test-token", nil)
	defer cancelled.Body.Close()
	if cancelled.StatusCode != http.StatusOK {
		t.Fatalf("POST /sessions/{id}/cancel status = %d, want %d", cancelled.StatusCode, http.StatusOK)
	}
	var cancelBody struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(cancelled.Body).Decode(&cancelBody); err != nil {
		t.Fatalf("decode cancel result: %v", err)
	}
	if cancelBody.Message != "Prompt cancelled" {
		t.Errorf("cancel result = %q, want human-readable confirmation", cancelBody.Message)
	}

	prompted := <-promptResult
	defer prompted.Body.Close()
	if prompted.StatusCode != http.StatusConflict {
		t.Errorf("cancelled Prompt status = %d, want %d", prompted.StatusCode, http.StatusConflict)
	}
}

func TestInternalFailuresReturn500JSON(t *testing.T) {
	api, err := rest.New(rest.Settings{
		ListenAddress: "127.0.0.1:0",
		BearerToken:   "test-token",
		Identity:      "api",
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open REST Channel: %v", err)
	}
	connect := func(context.Context, string, agent.Handlers) (*agent.Conn, error) {
		return nil, errors.New("Agent is unavailable")
	}
	manager, err := session.Open(t.Context(), t.TempDir()+"/aethos.db", connect, api)
	if err != nil {
		t.Fatalf("open Session manager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	response := request(t, api.Handler(manager), http.MethodPost, "/sessions", "test-token", map[string]string{
		"agent":     "codex-acp",
		"workspace": t.TempDir(),
	})
	defer response.Body.Close()

	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed POST /sessions status = %d, want %d", response.StatusCode, http.StatusInternalServerError)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("internal failure Content-Type = %q, want application/json", got)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode internal failure: %v", err)
	}
	if body.Error == "" {
		t.Error("internal failure has no human-readable error")
	}
}

func TestFailuresUseSpecificStatusesAndHumanReadableJSON(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{})
	handler := api.Handler(manager)
	sessionID := createSession(t, handler, t.TempDir())

	tests := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{
			name:   "validation",
			method: http.MethodPost,
			path:   "/sessions",
			body:   map[string]string{"workspace": t.TempDir()},
			want:   http.StatusBadRequest,
		},
		{
			name:   "unknown Session",
			method: http.MethodGet,
			path:   "/sessions/unknown",
			want:   http.StatusNotFound,
		},
		{
			name:   "conflicting cancel",
			method: http.MethodPost,
			path:   "/sessions/" + sessionID + "/cancel",
			want:   http.StatusConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := request(t, handler, tt.method, tt.path, "test-token", tt.body)
			defer response.Body.Close()
			if response.StatusCode != tt.want {
				t.Fatalf("%s %s status = %d, want %d", tt.method, tt.path, response.StatusCode, tt.want)
			}
			if got := response.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("failure Content-Type = %q, want application/json", got)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode failure: %v", err)
			}
			if body.Error == "" {
				t.Error("failure has no human-readable error")
			}
		})
	}
}

func TestCreateRejectsInvalidJSONBodies(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{})
	handler := api.Handler(manager)

	for _, body := range []string{
		`{"agent":`,
		`{"agent":"codex-acp","workspace":"/workspace"} {"agent":"second","workspace":"/other"}`,
		`{"agent":"codex-acp","workspace":"/workspace","unexpected":true}`,
	} {
		response := rawRequest(handler, http.MethodPost, "/sessions", "test-token", body)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /sessions body %q status = %d, want %d", body, response.StatusCode, http.StatusBadRequest)
		}
	}
}

func openRESTFlow(t *testing.T, script *agent.Script) (*rest.Channel, *session.Manager) {
	t.Helper()
	api, err := rest.New(rest.Settings{
		ListenAddress: "127.0.0.1:0",
		BearerToken:   "test-token",
		Identity:      "api",
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open REST Channel: %v", err)
	}
	connect := func(ctx context.Context, _ string, handlers agent.Handlers) (*agent.Conn, error) {
		return agent.ConnectScript(ctx, slog.New(slog.DiscardHandler), handlers, script)
	}
	manager, err := session.Open(t.Context(), t.TempDir()+"/aethos.db", connect, api)
	if err != nil {
		t.Fatalf("open Session manager: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close Session manager: %v", err)
		}
	})
	return api, manager
}

func request(t *testing.T, handler http.Handler, method, path, token string, body any) *http.Response {
	t.Helper()
	var encoded bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	return rawRequest(handler, method, path, token, encoded.String())
}

func rawRequest(handler http.Handler, method, path, token, body string) *http.Response {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder.Result()
}

func createSession(t *testing.T, handler http.Handler, workspace string) string {
	t.Helper()
	response := request(t, handler, http.MethodPost, "/sessions", "test-token", map[string]string{
		"agent":     "codex-acp",
		"workspace": workspace,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("POST /sessions fixture status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode fixture Session: %v", err)
	}
	return body.ID
}
