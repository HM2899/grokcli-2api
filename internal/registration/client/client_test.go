package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
