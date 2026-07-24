package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	APIVersion                 = "v1"
	maxResponseBodyBytes int64 = 32 << 20
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// HTTPLong is used for long-running POST operations (job creation,
	// device login, SSO import) that may take ~30s+ due to turnstile
	// solving and email roundtrips. If nil, falls back to HTTP.
	HTTPLong *http.Client
	// HTTPBulk is used only for the full registration sessions list, whose
	// JSON can take seconds to serialize under large durable batches.
	HTTPBulk *http.Client
}

type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("registration service %d: %s", e.Status, e.Detail) }

func (c *Client) Availability(ctx context.Context) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/availability", nil, nil)
}

func (c *Client) Start(ctx context.Context, request map[string]any, idempotencyKey string) (map[string]any, error) {
	headers := make(http.Header)
	if idempotencyKey != "" {
		headers.Set("Idempotency-Key", idempotencyKey)
	}
	return c.do(ctx, http.MethodPost, "/jobs", request, headers)
}

func (c *Client) Sessions(ctx context.Context) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/sessions", nil, nil)
}

func (c *Client) Session(ctx context.Context, id string, includeAuth bool) (map[string]any, error) {
	query := ""
	if includeAuth {
		query = "?include_auth_json=1"
	}
	return c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(id)+query, nil, nil)
}

func (c *Client) StopSession(ctx context.Context, id string) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/stop", map[string]any{}, nil)
}

func (c *Client) Batch(ctx context.Context, id string) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/batches/"+url.PathEscape(id), nil, nil)
}

func (c *Client) ResumeBatch(ctx context.Context, id string, force bool) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/batches/"+url.PathEscape(id)+"/resume", map[string]any{"force": force}, nil)
}

func (c *Client) StopBatch(ctx context.Context, id string) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/batches/"+url.PathEscape(id)+"/stop", map[string]any{}, nil)
}

func (c *Client) Reclaim(ctx context.Context, autoResume bool) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/reclaim", map[string]any{"auto_resume": autoResume}, nil)
}

func (c *Client) StopAll(ctx context.Context) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/stop", map[string]any{}, nil)
}

func (c *Client) StartDeviceLogin(ctx context.Context, request map[string]any) (map[string]any, error) {
	return c.doAbsolute(ctx, http.MethodPost, "/internal/device/v1/login", request, nil)
}

func (c *Client) DeviceLoginSession(ctx context.Context, sessionID string) (map[string]any, error) {
	return c.doAbsolute(ctx, http.MethodGet, "/internal/device/v1/sessions/"+url.PathEscape(sessionID), nil, nil)
}

func (c *Client) DeviceLoginSessions(ctx context.Context) (map[string]any, error) {
	return c.doAbsolute(ctx, http.MethodGet, "/internal/device/v1/sessions", nil, nil)
}

func (c *Client) StartSSOImport(ctx context.Context, request map[string]any) (map[string]any, error) {
	return c.doAbsolute(ctx, http.MethodPost, "/internal/sso/v1/import", request, nil)
}

func (c *Client) SSOImportJob(ctx context.Context, jobID string) (map[string]any, error) {
	return c.doAbsolute(ctx, http.MethodGet, "/internal/sso/v1/jobs/"+url.PathEscape(jobID), nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, headers http.Header) (map[string]any, error) {
	return c.doAbsolute(ctx, method, "/internal/registration/"+APIVersion+path, body, headers)
}

func (c *Client) doAbsolute(ctx context.Context, method, absPath string, body any, headers http.Header) (map[string]any, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		return nil, errors.New("registration service URL is not configured")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method,
		strings.TrimRight(c.BaseURL, "/")+absPath,
		reader,
	)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	httpClient := c.HTTP
	if httpClient == nil {
		// Fail-fast fallback only — server package injects a shared Transport.
		// Keep under browser REG_POLL_TIMEOUT_MS (~900ms).
		httpClient = &http.Client{
			Timeout: 750 * time.Millisecond,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext,
				MaxIdleConns:          128,
				MaxIdleConnsPerHost:   64,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 600 * time.Millisecond,
				ForceAttemptHTTP2:     true,
			},
		}
	}
	// Long-running POST operations (job creation, device login, SSO import)
	// can take ~30s+ due to turnstile solving + email roundtrips. Use a
	// dedicated long-timeout client so the default short-timeout client
	// (tuned for fast polling) doesn't prematurely abort them.
	if method == http.MethodPost && c.HTTPLong != nil {
		httpClient = c.HTTPLong
	} else if method == http.MethodGet && absPath == "/internal/registration/"+APIVersion+"/sessions" && c.HTTPBulk != nil {
		httpClient = c.HTTPBulk
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maxResponseBodyBytes {
		return nil, fmt.Errorf("registration service response exceeds %d MiB", maxResponseBodyBytes>>20)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope map[string]any
		detail := strings.TrimSpace(string(payload))
		if json.Unmarshal(payload, &envelope) == nil {
			if value, ok := envelope["detail"].(string); ok {
				detail = value
			} else if value, ok := envelope["error"].(string); ok {
				detail = value
			}
		}
		return nil, &Error{Status: response.StatusCode, Detail: detail}
	}
	var output map[string]any
	if len(payload) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(payload, &output); err != nil {
		return nil, err
	}
	return output, nil
}
