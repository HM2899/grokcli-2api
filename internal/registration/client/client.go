package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const APIVersion = "v1"

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
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
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
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
