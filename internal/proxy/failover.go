package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

var ErrCommitted = errors.New("response already committed")

type CommitState struct {
	mu        sync.Mutex
	committed bool
}

func (s *CommitState) Commit()         { s.mu.Lock(); s.committed = true; s.mu.Unlock() }
func (s *CommitState) Committed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.committed }

type Attempt struct {
	Account grok.Account
	Body    io.ReadCloser
}

// OpenWithFailover retries only errors classified as account/upstream retryable
// and only before the caller commits model content or tool payload to the client.
func OpenWithFailover(
	ctx context.Context,
	client *grok.Client,
	accounts []grok.Account,
	model string,
	body map[string]any,
	state *CommitState,
) (Attempt, error) {
	if state == nil {
		state = &CommitState{}
	}
	var last error
	for _, account := range accounts {
		if state.Committed() {
			return Attempt{}, fmt.Errorf("%w: %v", ErrCommitted, last)
		}
		response, err := client.Open(ctx, account, model, body)
		if err == nil {
			return Attempt{Account: account, Body: response.Body}, nil
		}
		last = err
		if !grok.Retryable(err) {
			break
		}
	}
	if last == nil {
		last = errors.New("no eligible accounts")
	}
	return Attempt{}, last
}
