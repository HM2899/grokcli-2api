package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRefreshAccessTokenSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-1" {
			t.Fatalf("form=%v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "aaa.eyJzdWIiOiJ1MSIsImV4cCI6NDEwMjQ0NDgwMH0.sig",
			"refresh_token": "rt-2",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	client := &Client{TokenURL: server.URL, HTTP: server.Client()}
	data, err := client.RefreshAccessToken(context.Background(), map[string]any{
		"refresh_token": "rt-1",
		"email":         "a@x.ai",
	})
	if err != nil {
		t.Fatal(err)
	}
	aid, entry, err := EntryFromTokenResponse(data, map[string]any{"email": "a@x.ai", "sso": "cookie"})
	if err != nil {
		t.Fatal(err)
	}
	if aid == "" || entry["key"] == nil || entry["sso"] != "cookie" {
		t.Fatalf("aid=%s entry=%#v", aid, entry)
	}
}

func TestRefreshPermanentError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh_token is invalid"}`))
	}))
	defer server.Close()
	client := &Client{TokenURL: server.URL, HTTP: server.Client()}
	_, err := client.RefreshAccessToken(context.Background(), map[string]any{"refresh_token": "bad"})
	var re *RefreshError
	if err == nil || !errorsAs(err, &re) || !re.Permanent {
		t.Fatalf("err=%v", err)
	}
}

func errorsAs(err error, target **RefreshError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*RefreshError); ok {
		*target = e
		return true
	}
	return false
}
