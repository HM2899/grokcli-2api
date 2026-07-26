package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionsAcceptsResponseLargerThanLegacyFourMiBLimit(t *testing.T) {
	const marker = "large-session-list-ok"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/registration/v1/sessions" {
			t.Fatalf("method=%s path=%s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"marker":   marker,
			"sessions": []any{map[string]any{"log": strings.Repeat("x", (5<<20)+1)}},
		})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTP: server.Client()}
	result, err := client.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions() error=%v", err)
	}
	if result["marker"] != marker {
		t.Fatalf("marker=%v", result["marker"])
	}
}
