package redis

import (
	"context"
	"time"
)

type PickObserver struct {
	Client *Client
}

func NewPickObserver(client *Client) PickObserver {
	return PickObserver{Client: client}
}

func (o PickObserver) LoadPenalty(ctx context.Context, accountID string) int64 {
	if o.Client == nil {
		return 0
	}
	inflight, err := o.Client.GetInflight(ctx, accountID)
	if err != nil {
		return 0
	}
	return inflight * 1000
}

// LoadPenalties batches inflight lookups for a candidate window (hot path).
func (o PickObserver) LoadPenalties(ctx context.Context, accountIDs []string) map[string]int64 {
	out := map[string]int64{}
	if o.Client == nil || len(accountIDs) == 0 {
		return out
	}
	inflight := o.Client.GetInflightMany(ctx, accountIDs)
	for id, n := range inflight {
		if n > 0 {
			out[id] = n * 1000
		}
	}
	return out
}

func (o PickObserver) MarkPick(ctx context.Context, accountID string) {
	if o.Client == nil {
		return
	}
	_, _ = o.Client.MarkInflight(ctx, accountID, InflightTTLSeconds)
	_, _ = o.Client.MarkSoftUsed(ctx, accountID, SoftUsedTTLSeconds, time.Now())
}

func (o PickObserver) ReleasePick(ctx context.Context, accountID string) {
	if o.Client == nil {
		return
	}
	_ = o.Client.ReleaseInflight(ctx, accountID)
}
