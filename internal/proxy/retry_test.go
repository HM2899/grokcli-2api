package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

func TestShouldRetry(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "429 Too Many Requests",
			err:  &grok.UpstreamError{Status: 429, Body: "rate limited"},
			want: true,
		},
		{
			name: "500 Internal Server Error",
			err:  &grok.UpstreamError{Status: 500, Body: "server error"},
			want: true,
		},
		{
			name: "502 Bad Gateway",
			err:  &grok.UpstreamError{Status: 502, Body: "bad gateway"},
			want: true,
		},
		{
			name: "503 Service Unavailable",
			err:  &grok.UpstreamError{Status: 503, Body: "service unavailable"},
			want: true,
		},
		{
			name: "504 Gateway Timeout",
			err:  &grok.UpstreamError{Status: 504, Body: "gateway timeout"},
			want: true,
		},
		{
			name: "401 Unauthorized",
			err:  &grok.UpstreamError{Status: 401, Body: "unauthorized"},
			want: false,
		},
		{
			name: "400 Bad Request",
			err:  &grok.UpstreamError{Status: 400, Body: "bad request"},
			want: false,
		},
		{
			name: "context canceled",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "context deadline exceeded",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "generic error",
			err:  errors.New("some error"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldRetry(tt.err); got != tt.want {
				t.Errorf("ShouldRetry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCalculateBackoff(t *testing.T) {
	config := DefaultRetryConfig()

	tests := []struct {
		name    string
		attempt int
		min     time.Duration
		max     time.Duration
	}{
		{
			name:    "first retry",
			attempt: 0,
			min:     80 * time.Millisecond,
			max:     120 * time.Millisecond,
		},
		{
			name:    "second retry",
			attempt: 1,
			min:     160 * time.Millisecond,
			max:     240 * time.Millisecond,
		},
		{
			name:    "third retry",
			attempt: 2,
			min:     320 * time.Millisecond,
			max:     480 * time.Millisecond,
		},
		{
			name:    "max backoff",
			attempt: 10,
			min:     config.MaxBackoff - (config.MaxBackoff / 5),
			max:     config.MaxBackoff + (config.MaxBackoff / 5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backoff := CalculateBackoff(tt.attempt, config)
			if backoff < tt.min || backoff > tt.max {
				t.Errorf("CalculateBackoff(%d) = %v, want between %v and %v", tt.attempt, backoff, tt.min, tt.max)
			}
		})
	}
}

func TestWaitWithContext(t *testing.T) {
	t.Run("normal wait", func(t *testing.T) {
		ctx := context.Background()
		start := time.Now()
		err := WaitWithContext(ctx, 50*time.Millisecond)
		elapsed := time.Since(start)

		if err != nil {
			t.Errorf("WaitWithContext() error = %v, want nil", err)
		}
		if elapsed < 50*time.Millisecond {
			t.Errorf("WaitWithContext() elapsed = %v, want >= 50ms", elapsed)
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := WaitWithContext(ctx, 1*time.Second)
		if err != context.Canceled {
			t.Errorf("WaitWithContext() error = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("context timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		err := WaitWithContext(ctx, 1*time.Second)
		if err != context.DeadlineExceeded {
			t.Errorf("WaitWithContext() error = %v, want %v", err, context.DeadlineExceeded)
		}
	})
}
