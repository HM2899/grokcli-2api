package grok

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL          string
	CLIversion       string
	ClientSurface    string
	ClientIdentifier string
	HTTP             *http.Client
}

type Account struct {
	ID    string
	Token string
}

type Event struct {
	Data []byte
	Done bool
}

type UpstreamError struct {
	Status     int
	Body       string
	RetryAfter string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.Status, e.Body)
}

// defaultHTTPClient returns a properly configured HTTP client with connection pooling
func defaultHTTPClient() *http.Client {
	// No overall Client.Timeout: streaming responses can run for minutes.
	// Bound connect + response headers only via Transport.
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:        200,               // 增加全局空闲连接数
			MaxIdleConnsPerHost: 100,               // 增加每个 host 的空闲连接数，支持高并发
			MaxConnsPerHost:     200,               // 增加每个 host 的最大连接数
			IdleConnTimeout:     120 * time.Second, // 延长空闲连接保持时间
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,  // fail-fast dial for failover TTFT
				KeepAlive: 60 * time.Second, // 延长 TCP keepalive
			}).DialContext,
			ForceAttemptHTTP2:     true,
			DisableCompression:    false,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second, // fail-fast first-byte; long streams keep body open after headers
			DisableKeepAlives:     false,
			WriteBufferSize:       32 * 1024, // 增加写缓冲，提高大请求性能
			ReadBufferSize:        32 * 1024, // 增加读缓冲，提高大响应性能
		},
	}
}

func (c *Client) Open(ctx context.Context, account Account, model string, body map[string]any) (*http.Response, error) {
	if c.HTTP == nil {
		c.HTTP = defaultHTTPClient()
	}
	payload := cloneMap(body)
	payload["model"] = model
	payload["stream"] = true
	options, _ := payload["stream_options"].(map[string]any)
	options = cloneMap(options)
	options["include_usage"] = true
	payload["stream_options"] = options
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	for name, value := range c.Headers(account.Token, model) {
		request.Header.Set(name, value)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return nil, &UpstreamError{
			Status: response.StatusCode, Body: string(body),
			RetryAfter: response.Header.Get("Retry-After"),
		}
	}
	return response, nil
}

func (c *Client) Headers(token, model string) map[string]string {
	version := c.CLIversion
	if version == "" {
		version = "0.2.93"
	}
	surface := c.ClientSurface
	if surface == "" {
		surface = "grok-cli"
	}
	identifier := c.ClientIdentifier
	if identifier == "" {
		identifier = "grokcli-2api"
	}
	return map[string]string{
		"Content-Type":             "application/json",
		"Authorization":            "Bearer " + token,
		"X-XAI-Token-Auth":         "xai-grok-cli",
		"x-grok-model-override":    model,
		"x-grok-client-version":    version,
		"x-grok-client-surface":    surface,
		"x-grok-client-identifier": identifier,
		"User-Agent":               "grok-cli/" + version,
		"Accept":                   "text/event-stream, application/json",
	}
}

func ReadSSE(reader io.Reader, emit func(Event) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		joined := strings.Join(data, "\n")
		data = data[:0]
		if joined == "[DONE]" {
			return emit(Event{Done: true})
		}
		return emit(Event{Data: []byte(joined)})
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// ReadSSEWithIdle is like ReadSSE but invokes onIdle whenever no complete SSE
// frame arrives for idle duration. Used for Anthropic/OpenAI stream keepalives.
func ReadSSEWithIdle(reader io.Reader, idle time.Duration, emit func(Event) error, onIdle func() error) error {
	if idle <= 0 || onIdle == nil {
		return ReadSSE(reader, emit)
	}
	type result struct {
		event Event
		err   error
		done  bool
	}
	ch := make(chan result, 1)
	go func() {
		err := ReadSSE(reader, func(event Event) error {
			ch <- result{event: event}
			return nil
		})
		ch <- result{err: err, done: true}
	}()
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case item := <-ch:
			if item.done {
				return item.err
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
			if err := emit(item.event); err != nil {
				return err
			}
		case <-timer.C:
			if err := onIdle(); err != nil {
				return err
			}
			timer.Reset(idle)
		}
	}
}
func Retryable(err error) bool {
	var upstream *UpstreamError
	if !errors.As(err, &upstream) {
		return false
	}
	switch upstream.Status {
	case 401, 403, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input)+2)
	for key, value := range input {
		out[key] = value
	}
	return out
}
