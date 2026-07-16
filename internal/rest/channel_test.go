package rest_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aesoteric/aethos/internal/agent"
	"github.com/aesoteric/aethos/internal/channel"
	"github.com/aesoteric/aethos/internal/rest"
	"github.com/aesoteric/aethos/internal/session"
)

func TestClientStreamsAgentOutputAsItHappens(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{Turns: []agent.Turn{{
		WantPrompt: "inspect the config",
		Events: []agent.Event{
			agent.Thought{Text: "Considering the config."},
			agent.ToolCallBegan{ID: "tool-1", Title: "Read config", Kind: "read", Status: "in_progress"},
			agent.ToolCallProgressed{ID: "tool-1", Title: "Read config", Status: "completed"},
			agent.Message{Text: "The config is valid."},
		},
		Stop: agent.StopEndTurn,
	}}})
	handler := api.Handler(manager)
	sessionID := createSession(t, handler, t.TempDir())
	server := httptest.NewServer(handler)
	defer server.Close()
	defer server.CloseClientConnections()

	streamCtx, cancelStream := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelStream()
	stream := openEventStream(t, streamCtx, server.URL+"/sessions/"+sessionID+"/events")
	defer stream.Body.Close()

	prompted := make(chan *http.Response, 1)
	go func() {
		prompted <- serverRequest(t, server.Client(), http.MethodPost, server.URL+"/sessions/"+sessionID+"/prompt", map[string]string{
			"prompt": "inspect the config",
		})
	}()

	reader := bufio.NewReader(stream.Body)
	want := []struct {
		name string
		data string
	}{
		{name: "prompt_started", data: `{"prompt":"inspect the config"}`},
		{name: "thought", data: `{"text":"Considering the config."}`},
		{name: "tool_call_began", data: `{"id":"tool-1","title":"Read config","kind":"read","status":"in_progress"}`},
		{name: "tool_call_progressed", data: `{"id":"tool-1","title":"Read config","status":"completed"}`},
		{name: "message", data: `{"text":"The config is valid."}`},
		{name: "prompt_finished", data: `{"stop_reason":"end_turn"}`},
	}
	for _, expected := range want {
		got := readEvent(t, reader)
		if got.name != expected.name {
			t.Fatalf("SSE event name = %q, want %q", got.name, expected.name)
		}
		if string(got.data) != expected.data {
			t.Errorf("%s SSE data = %s, want %s", got.name, got.data, expected.data)
		}
	}

	response := <-prompted
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST Prompt status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestClientStreamsPromptLifecycle(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{Turns: []agent.Turn{{
		WantPrompt: "ship it",
		Stop:       agent.StopEndTurn,
	}}})
	handler := api.Handler(manager)
	sessionID := createSession(t, handler, t.TempDir())
	server := httptest.NewServer(handler)
	defer server.Close()
	defer server.CloseClientConnections()

	streamCtx, cancelStream := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelStream()
	stream := openEventStream(t, streamCtx, server.URL+"/sessions/"+sessionID+"/events")
	defer stream.Body.Close()

	prompted := make(chan *http.Response, 1)
	go func() {
		prompted <- serverRequest(t, server.Client(), http.MethodPost, server.URL+"/sessions/"+sessionID+"/prompt", map[string]string{
			"prompt": "ship it",
		})
	}()

	reader := bufio.NewReader(stream.Body)
	started := readEvent(t, reader)
	if started.name != "prompt_started" || string(started.data) != `{"prompt":"ship it"}` {
		t.Errorf("started Prompt event = %q %s, want prompt_started with Prompt text", started.name, started.data)
	}
	finished := readEvent(t, reader)
	if finished.name != "prompt_finished" || string(finished.data) != `{"stop_reason":"end_turn"}` {
		t.Errorf("finished Prompt event = %q %s, want prompt_finished with stop reason", finished.name, finished.data)
	}

	response := <-prompted
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST Prompt status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestClientAnswersStreamedPermissionAndAgentProceeds(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{Turns: []agent.Turn{{
		WantPrompt: "update the config",
		Permissions: []agent.ScriptedPermission{{
			Request: agent.PermissionRequest{
				ToolCallID: "tool-1",
				Title:      "Edit config",
				Kind:       "edit",
				Input:      map[string]any{"path": "config.toml"},
				Options: []agent.PermissionOption{
					{ID: "allow-once", Name: "Allow once", Kind: agent.PermissionAllowOnce},
					{ID: "reject-once", Name: "Reject", Kind: agent.PermissionRejectOnce},
				},
			},
			WantOptionID: "allow-once",
		}},
		Events: []agent.Event{agent.Message{Text: "Config updated."}},
		Stop:   agent.StopEndTurn,
	}}})
	handler := api.Handler(manager)
	sessionID := createSession(t, handler, t.TempDir())
	server := httptest.NewServer(handler)
	defer server.Close()
	defer server.CloseClientConnections()

	streamCtx, cancelStream := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelStream()
	stream := openEventStream(t, streamCtx, server.URL+"/sessions/"+sessionID+"/events")
	defer stream.Body.Close()

	prompted := make(chan *http.Response, 1)
	go func() {
		prompted <- serverRequest(t, server.Client(), http.MethodPost, server.URL+"/sessions/"+sessionID+"/prompt", map[string]string{
			"prompt": "update the config",
		})
	}()

	reader := bufio.NewReader(stream.Body)
	if started := readEvent(t, reader); started.name != "prompt_started" {
		t.Fatalf("first SSE event = %q, want prompt_started", started.name)
	}
	requested := readEvent(t, reader)
	if requested.name != "permission_requested" {
		t.Fatalf("second SSE event = %q, want permission_requested", requested.name)
	}
	var permission struct {
		ID         string `json:"id"`
		ToolCallID string `json:"tool_call_id"`
		Title      string `json:"title"`
		Kind       string `json:"kind"`
		Input      struct {
			Path string `json:"path"`
		} `json:"input"`
		Options []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(requested.data, &permission); err != nil {
		t.Fatalf("decode permission request event: %v", err)
	}
	if permission.ID == "" || permission.ToolCallID != "tool-1" || permission.Title != "Edit config" ||
		permission.Kind != "edit" || permission.Input.Path != "config.toml" || len(permission.Options) != 2 ||
		permission.Options[0].ID != "allow-once" || permission.Options[0].Name != "Allow once" || permission.Options[0].Kind != "allow_once" {
		t.Fatalf("permission request = %#v, want Agent request identity, input, and options", permission)
	}
	rejected := serverRequest(t, server.Client(), http.MethodPost, server.URL+"/permissions/"+permission.ID, map[string]string{
		"option_id": "not-offered",
	})
	rejected.Body.Close()
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("unoffered permission option status = %d, want %d", rejected.StatusCode, http.StatusBadRequest)
	}

	for attempt := 0; attempt < 2; attempt++ {
		answered := serverRequest(t, server.Client(), http.MethodPost, server.URL+"/permissions/"+permission.ID, map[string]string{
			"option_id": "allow-once",
		})
		answered.Body.Close()
		if answered.StatusCode != http.StatusOK {
			t.Fatalf("permission answer %d status = %d, want %d", attempt+1, answered.StatusCode, http.StatusOK)
		}
	}

	resolved := readEvent(t, reader)
	wantResolved := `{"id":"` + permission.ID + `","option_id":"allow-once"}`
	if resolved.name != "permission_resolved" || string(resolved.data) != wantResolved {
		t.Errorf("resolved permission event = %q %s, want permission_resolved %s", resolved.name, resolved.data, wantResolved)
	}
	message := readEvent(t, reader)
	if message.name != "message" || string(message.data) != `{"text":"Config updated."}` {
		t.Errorf("post-permission event = %q %s, want Agent message", message.name, message.data)
	}
	if finished := readEvent(t, reader); finished.name != "prompt_finished" {
		t.Errorf("final SSE event = %q, want prompt_finished", finished.name)
	}

	response := <-prompted
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST Prompt status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestEventStreamEndsWhenSessionCloses(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{})
	handler := api.Handler(manager)
	sessionID := createSession(t, handler, t.TempDir())
	server := httptest.NewServer(handler)
	defer server.Close()
	defer server.CloseClientConnections()

	streamCtx, cancelStream := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelStream()
	stream := openEventStream(t, streamCtx, server.URL+"/sessions/"+sessionID+"/events")
	defer stream.Body.Close()

	if _, err := manager.CloseSession(t.Context(), sessionID); err != nil {
		t.Fatalf("close Session: %v", err)
	}
	reader := bufio.NewReader(stream.Body)
	closed := readEvent(t, reader)
	if closed.name != "session_state_changed" || string(closed.data) != `{"state":"closed"}` {
		t.Errorf("closed Session event = %q %s, want terminal closed state", closed.name, closed.data)
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("read after closed Session event = %v, want EOF", err)
	}
	reopened := request(t, handler, http.MethodGet, "/sessions/"+sessionID+"/events", "test-token", nil)
	defer reopened.Body.Close()
	if reopened.StatusCode != http.StatusConflict {
		t.Errorf("closed Session event stream status = %d, want %d", reopened.StatusCode, http.StatusConflict)
	}
}

func TestClientStreamsDormantAndLiveStateChanges(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{Turns: []agent.Turn{
		{WantPrompt: "crash", Crash: true},
		{WantPrompt: "resume", Events: []agent.Event{agent.Message{Text: "Back online."}}, Stop: agent.StopEndTurn},
	}})
	handler := api.Handler(manager)
	sessionID := createSession(t, handler, t.TempDir())
	server := httptest.NewServer(handler)
	defer server.Close()
	defer server.CloseClientConnections()

	streamCtx, cancelStream := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelStream()
	stream := openEventStream(t, streamCtx, server.URL+"/sessions/"+sessionID+"/events")
	defer stream.Body.Close()
	reader := bufio.NewReader(stream.Body)

	crashedPrompt := make(chan *http.Response, 1)
	go func() {
		crashedPrompt <- serverRequest(t, server.Client(), http.MethodPost, server.URL+"/sessions/"+sessionID+"/prompt", map[string]string{"prompt": "crash"})
	}()
	for index, name := range []string{"prompt_started", "session_state_changed", "crashed", "prompt_finished"} {
		event := readEvent(t, reader)
		if event.name != name {
			t.Fatalf("crash SSE event %d = %q, want %q", index+1, event.name, name)
		}
		if name == "session_state_changed" && string(event.data) != `{"state":"dormant"}` {
			t.Errorf("crash state event = %s, want dormant", event.data)
		}
	}
	crashedResponse := <-crashedPrompt
	crashedResponse.Body.Close()
	if crashedResponse.StatusCode != http.StatusInternalServerError {
		t.Fatalf("crashed Prompt status = %d, want %d", crashedResponse.StatusCode, http.StatusInternalServerError)
	}

	resumedPrompt := make(chan *http.Response, 1)
	go func() {
		resumedPrompt <- serverRequest(t, server.Client(), http.MethodPost, server.URL+"/sessions/"+sessionID+"/prompt", map[string]string{"prompt": "resume"})
	}()
	for index, expected := range []struct {
		name string
		data string
	}{
		{name: "prompt_started", data: `{"prompt":"resume"}`},
		{name: "session_state_changed", data: `{"state":"live"}`},
		{name: "message", data: `{"text":"Back online."}`},
		{name: "prompt_finished", data: `{"stop_reason":"end_turn"}`},
	} {
		event := readEvent(t, reader)
		if event.name != expected.name || string(event.data) != expected.data {
			t.Fatalf("resume SSE event %d = %q %s, want %q %s", index+1, event.name, event.data, expected.name, expected.data)
		}
	}
	resumedResponse := <-resumedPrompt
	defer resumedResponse.Body.Close()
	if resumedResponse.StatusCode != http.StatusOK {
		t.Fatalf("resumed Prompt status = %d, want %d", resumedResponse.StatusCode, http.StatusOK)
	}
}

func TestReconnectingClientReceivesOnlySubsequentEvents(t *testing.T) {
	api, manager := openRESTFlow(t, &agent.Script{Turns: []agent.Turn{
		{WantPrompt: "first", Events: []agent.Event{agent.Message{Text: "First result."}}, Stop: agent.StopEndTurn},
		{WantPrompt: "second", Events: []agent.Event{agent.Message{Text: "Second result."}}, Stop: agent.StopEndTurn},
	}})
	handler := api.Handler(manager)
	sessionID := createSession(t, handler, t.TempDir())
	server := httptest.NewServer(handler)
	defer server.Close()
	defer server.CloseClientConnections()

	firstCtx, cancelFirst := context.WithCancel(t.Context())
	firstStream := openEventStream(t, firstCtx, server.URL+"/sessions/"+sessionID+"/events")
	firstPrompt := make(chan *http.Response, 1)
	go func() {
		firstPrompt <- serverRequest(t, server.Client(), http.MethodPost, server.URL+"/sessions/"+sessionID+"/prompt", map[string]string{"prompt": "first"})
	}()
	firstReader := bufio.NewReader(firstStream.Body)
	for _, name := range []string{"prompt_started", "message", "prompt_finished"} {
		if event := readEvent(t, firstReader); event.name != name {
			t.Fatalf("first connection event = %q, want %q", event.name, name)
		}
	}
	firstResponse := <-firstPrompt
	firstResponse.Body.Close()
	cancelFirst()
	firstStream.Body.Close()

	secondCtx, cancelSecond := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelSecond()
	secondStream := openEventStream(t, secondCtx, server.URL+"/sessions/"+sessionID+"/events")
	defer secondStream.Body.Close()
	secondPrompt := make(chan *http.Response, 1)
	go func() {
		secondPrompt <- serverRequest(t, server.Client(), http.MethodPost, server.URL+"/sessions/"+sessionID+"/prompt", map[string]string{"prompt": "second"})
	}()

	secondReader := bufio.NewReader(secondStream.Body)
	started := readEvent(t, secondReader)
	if started.name != "prompt_started" || string(started.data) != `{"prompt":"second"}` {
		t.Fatalf("first reconnected event = %q %s, want subsequent Prompt only", started.name, started.data)
	}
	message := readEvent(t, secondReader)
	if message.name != "message" || string(message.data) != `{"text":"Second result."}` {
		t.Fatalf("reconnected Agent event = %q %s, want second result", message.name, message.data)
	}
	if finished := readEvent(t, secondReader); finished.name != "prompt_finished" {
		t.Fatalf("last reconnected event = %q, want prompt_finished", finished.name)
	}
	secondResponse := <-secondPrompt
	defer secondResponse.Body.Close()
	if secondResponse.StatusCode != http.StatusOK {
		t.Fatalf("second Prompt status = %d, want %d", secondResponse.StatusCode, http.StatusOK)
	}
}

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
		{method: http.MethodGet, path: "/sessions/id/events"},
		{method: http.MethodPost, path: "/sessions/id/prompt"},
		{method: http.MethodPost, path: "/sessions/id/cancel"},
		{method: http.MethodPost, path: "/permissions/id"},
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
			name:   "unknown Session event stream",
			method: http.MethodGet,
			path:   "/sessions/unknown/events",
			want:   http.StatusNotFound,
		},
		{
			name:   "unknown permission request",
			method: http.MethodPost,
			path:   "/permissions/unknown",
			body:   map[string]string{"option_id": "allow-once"},
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
	var manager *session.Manager
	events, err := channel.NewRouter(
		func(ctx context.Context, sessionID string) (string, error) {
			record, lookupErr := manager.Get(ctx, sessionID)
			return record.Owner.Channel, lookupErr
		},
		map[string]channel.Channel{"rest": api},
	)
	if err != nil {
		t.Fatalf("open Channel router: %v", err)
	}
	manager, err = session.Open(t.Context(), t.TempDir()+"/aethos.db", connect, events)
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

func serverRequest(t *testing.T, client *http.Client, method, url string, body any) *http.Response {
	t.Helper()
	var encoded io.Reader
	if body != nil {
		var buffer bytes.Buffer
		if err := json.NewEncoder(&buffer).Encode(body); err != nil {
			t.Fatalf("encode server request body: %v", err)
		}
		encoded = &buffer
	}
	req, err := http.NewRequest(method, url, encoded)
	if err != nil {
		t.Fatalf("build server request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("send server request: %v", err)
	}
	return response
}

func openEventStream(t *testing.T, ctx context.Context, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build SSE request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET event stream status = %d, want %d: %s", response.StatusCode, http.StatusOK, body)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		response.Body.Close()
		t.Fatalf("event stream Content-Type = %q, want text/event-stream", got)
	}
	return response
}

type receivedEvent struct {
	name string
	data json.RawMessage
}

func readEvent(t *testing.T, reader *bufio.Reader) receivedEvent {
	t.Helper()
	var event receivedEvent
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE event: %v", err)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case line == "" && event.name != "":
			return event
		case strings.HasPrefix(line, "event: "):
			event.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			event.data = append(event.data, strings.TrimPrefix(line, "data: ")...)
		}
	}
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
