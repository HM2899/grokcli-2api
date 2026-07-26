package client

import (
	"context"
	"net/http"
	"testing"
)

func TestSessionsUsesBulkClientWithoutSlowingSingleSessionPolls(t *testing.T) {
	shortCalls := 0
	bulkCalls := 0
	shortClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		shortCalls++
		return jsonResponse("{}"), nil
	})}
	bulkClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		bulkCalls++
		return jsonResponse("{}"), nil
	})}
	client := &Client{
		BaseURL:  "http://registration.invalid",
		HTTP:     shortClient,
		HTTPBulk: bulkClient,
	}

	if _, err := client.Sessions(context.Background()); err != nil {
		t.Fatalf("Sessions() error=%v", err)
	}
	if _, err := client.Session(context.Background(), "session-1", false); err != nil {
		t.Fatalf("Session() error=%v", err)
	}
	if bulkCalls != 1 || shortCalls != 1 {
		t.Fatalf("bulkCalls=%d shortCalls=%d", bulkCalls, shortCalls)
	}
}
