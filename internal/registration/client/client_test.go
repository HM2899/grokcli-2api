package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestStartUsesVersionedInternalContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/registration/v1/jobs" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Idempotency-Key") != "idem" {
			t.Fatalf("headers=%v", r.Header)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "batch_id": "b"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "secret", HTTP: server.Client()}
	result, err := client.Start(context.Background(), map[string]any{"count": 2}, "idem")
	if err != nil || result["batch_id"] != "b" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestStartUsesLongHTTPClient(t *testing.T) {
	shortClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("POST registration request used the short polling client")
		return nil, nil
	})}
	longClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Fatalf("method=%s", request.Method)
		}
		return jsonResponse(`{"ok":true,"batch_id":"b"}`), nil
	})}

	client := &Client{
		BaseURL:  "http://registration.test",
		HTTP:     shortClient,
		HTTPLong: longClient,
	}
	result, err := client.Start(context.Background(), map[string]any{"count": 2}, "")
	if err != nil || result["batch_id"] != "b" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestSessionsUsesShortHTTPClient(t *testing.T) {
	shortClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("method=%s", request.Method)
		}
		return jsonResponse(`{"sessions":[]}`), nil
	})}
	longClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("GET registration request used the long-running client")
		return nil, nil
	})}

	client := &Client{
		BaseURL:  "http://registration.test",
		HTTP:     shortClient,
		HTTPLong: longClient,
	}
	result, err := client.Sessions(context.Background())
	if err != nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestSSOImportUsesAbsoluteSSOPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/sso/v1/import" {
			t.Fatalf("method=%s path=%s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "job_id": "sso_1", "async": true})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTP: server.Client()}
	result, err := client.StartSSOImport(context.Background(), map[string]any{"sso_cookies": []string{"sso=abc"}})
	if err != nil || result["job_id"] != "sso_1" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestSSOImportJobPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/sso/v1/jobs/sso_1" {
			t.Fatalf("method=%s path=%s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "running"})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTP: server.Client()}
	result, err := client.SSOImportJob(context.Background(), "sso_1")
	if err != nil || result["status"] != "running" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
