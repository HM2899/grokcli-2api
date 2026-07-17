package redis

import (
	"context"
	"time"
)

type ChatAffinity struct {
	Client *Client
	TTL    time.Duration
}

func NewChatAffinity(client *Client, ttl time.Duration) ChatAffinity {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return ChatAffinity{Client: client, TTL: ttl}
}

func (a ChatAffinity) GetAffinity(ctx context.Context, fingerprint string) (string, error) {
	if a.Client == nil {
		return "", nil
	}
	entry, err := a.Client.GetAffinity(ctx, fingerprint, a.TTL)
	if err != nil || entry == nil {
		return "", err
	}
	return entry.AccountID, nil
}

func (a ChatAffinity) BindAffinity(ctx context.Context, fingerprint, accountID string) error {
	if a.Client == nil {
		return nil
	}
	return a.Client.BindAffinity(ctx, fingerprint, accountID, a.TTL, "", "")
}
