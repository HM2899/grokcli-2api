package grok

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenForcesStreamAndCompatibilityHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, want := range map[string]string{
			"Authorization": "Bearer token", "X-Xai-Token-Auth": "xai-grok-cli",
			"X-Grok-Model-Override": "grok-4.5", "X-Grok-Client-Version": "0.2.93",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s=%q want %q", name, got, want)
			}
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != true {
			t.Fatalf("stream not forced: %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL + "/v1", HTTP: server.Client()}
	response, err := client.Open(context.Background(), Account{ID: "a", Token: "token"}, "grok-4.5", map[string]any{"stream": false})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestReadSSE(t *testing.T) {
	var events []Event
	err := ReadSSE(strings.NewReader("data: {\"x\":1}\n\ndata: [DONE]\n\n"), func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil || len(events) != 2 || events[0].Done || !events[1].Done {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}
