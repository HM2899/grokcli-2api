package ownership

import (
	"context"
	"errors"
	"testing"
	"time"
)

type row struct {
	values []any
	err    error
}

func (r row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, value := range r.values {
		switch out := dest[i].(type) {
		case *string:
			*out = value.(string)
		case *Owner:
			*out = value.(Owner)
		case *int64:
			*out = value.(int64)
		case **time.Time:
			*out = value.(*time.Time)
		case *bool:
			*out = value.(bool)
		}
	}
	return nil
}

type session struct{ rows []row }

func (s *session) Exec(context.Context, string, ...any) error { return nil }
func (s *session) QueryRow(context.Context, string, ...any) Row {
	r := s.rows[0]
	s.rows = s.rows[1:]
	return r
}

func TestCheckRejectsStaleEpoch(t *testing.T) {
	s := &session{rows: []row{{values: []any{false}}}}
	err := Check(context.Background(), s, "a", OwnerGo, 4)
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("expected stale epoch, got %v", err)
	}
}
