package ownership

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Owner string

const (
	OwnerPython Owner = "python"
	OwnerGo     Owner = "go"
	OwnerNone   Owner = "none"
)

var ErrStaleEpoch = errors.New("account ownership epoch changed")

type Row interface {
	Scan(...any) error
}

type Session interface {
	Exec(context.Context, string, ...any) error
	QueryRow(context.Context, string, ...any) Row
}

type Lease struct {
	AccountID string
	Owner     Owner
	Epoch     int64
	Until     *time.Time
}

// Transfer increments epoch and changes ownership under a row lock. Writers
// retain the returned epoch and include it in every later mutation.
func Transfer(ctx context.Context, session Session, accountID string, to Owner, leaseUntil *time.Time) (Lease, error) {
	if accountID == "" {
		return Lease{}, errors.New("account id is required")
	}
	if to != OwnerPython && to != OwnerGo && to != OwnerNone {
		return Lease{}, fmt.Errorf("unsupported owner %q", to)
	}

	var lease Lease
	err := session.QueryRow(ctx, `
INSERT INTO account_runtime_ownership (
  account_id, owner, epoch, lease_until, updated_at
) VALUES ($1, $2, 1, $3, now())
ON CONFLICT (account_id) DO UPDATE SET
  owner = EXCLUDED.owner,
  epoch = account_runtime_ownership.epoch + 1,
  lease_until = EXCLUDED.lease_until,
  updated_at = now()
RETURNING account_id, owner, epoch, lease_until
`, accountID, string(to), leaseUntil).Scan(
		&lease.AccountID, &lease.Owner, &lease.Epoch, &lease.Until,
	)
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// Check verifies a writer still owns the exact epoch. The check belongs in the
// same database transaction as the protected account/pool mutation.
func Check(ctx context.Context, session Session, accountID string, owner Owner, epoch int64) error {
	var allowed bool
	err := session.QueryRow(ctx, `
SELECT owner = $2
   AND epoch = $3
   AND (lease_until IS NULL OR lease_until > now())
FROM account_runtime_ownership
WHERE account_id = $1
FOR UPDATE
`, accountID, string(owner), epoch).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrStaleEpoch
	}
	return nil
}
