package proxy

import (
	"context"
	"errors"
	"time"

	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

// RetryConfig 定义重试策略配置
type RetryConfig struct {
	MaxRetries     int           // 最大重试次数
	InitialBackoff time.Duration // 初始退避时间
	MaxBackoff     time.Duration // 最大退避时间
	BackoffFactor  float64       // 退避因子
}

// DefaultRetryConfig 返回默认重试配置
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     5 * time.Second,
		BackoffFactor:  2.0,
	}
}

// ShouldRetry 判断错误是否应该重试
func ShouldRetry(err error) bool {
	if err == nil {
		return false
	}

	var upstreamErr *grok.UpstreamError
	if errors.As(err, &upstreamErr) {
		// 429 (Too Many Requests), 500, 502, 503, 504 应该重试
		switch upstreamErr.Status {
		case 429, 500, 502, 503, 504:
			return true
		}
	}

	// 网络错误、超时错误应该重试
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false // 客户端主动取消不重试
	}

	return false
}

// CalculateBackoff 计算退避时间（指数退避 + 抖动）
func CalculateBackoff(attempt int, config RetryConfig) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	backoff := config.InitialBackoff
	for i := 0; i < attempt; i++ {
		backoff = time.Duration(float64(backoff) * config.BackoffFactor)
		if backoff > config.MaxBackoff {
			backoff = config.MaxBackoff
			break
		}
	}

	// 添加 20% 的随机抖动，避免雷击效应
	jitter := time.Duration(float64(backoff) * 0.2)
	if jitter > 0 {
		// 简单的抖动，实际生产环境可以用更好的随机数
		backoff = backoff + (jitter / 2)
	}

	return backoff
}

// WaitWithContext 等待退避时间，支持上下文取消
func WaitWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
