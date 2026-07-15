package telegram_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aesoteric/aethos/internal/telegram"
)

func TestClientValidatesBotTokenThroughTelegramProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/botvalid-token/getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"aethos"}}`)
		case "/botinvalid-token/getMe":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"ok":false,"description":"Unauthorized"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := telegram.NewClient(server.URL, server.Client())
	if err := client.ValidateToken(t.Context(), "valid-token"); err != nil {
		t.Fatalf("ValidateToken(valid) = %v, want nil", err)
	}

	err := client.ValidateToken(t.Context(), "invalid-token")
	if err == nil {
		t.Fatal("ValidateToken(invalid) succeeded, want error")
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("ValidateToken(invalid) error = %q, want Telegram's reason", err)
	}
	if strings.Contains(err.Error(), "invalid-token") {
		t.Errorf("ValidateToken(invalid) error leaked the bot token: %q", err)
	}
}

func TestClientRejectsEmptyBotTokenWithoutSendingIt(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	client := telegram.NewClient(server.URL, server.Client())
	err := client.ValidateToken(t.Context(), "   ")
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("ValidateToken(empty) = %v, want required-field error", err)
	}
	if requests != 0 {
		t.Errorf("empty token made %d requests, want 0", requests)
	}
}
