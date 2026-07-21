package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func jsonResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("{}")),
	}
}

func TestSessionsUsesBulkClientWithoutSlowingSingleSessionPolls(t *testing.T) {
	shortCalls := 0
	bulkCalls := 0
	shortClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		shortCalls++
		return jsonResponse(), nil
	})}
	bulkClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		bulkCalls++
		return jsonResponse(), nil
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
