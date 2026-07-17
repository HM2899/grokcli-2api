package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

type fakeConnector struct{ session *fakeSession }

func (c fakeConnector) Acquire(context.Context) (Session, error) { return c.session, nil }

type fakeSession struct {
	execSQL []string
	rows    map[int64]Applied
	closed  bool
}

func (s *fakeSession) Exec(_ context.Context, sql string, args ...any) error {
	s.execSQL = append(s.execSQL, sql)
	if len(args) == 3 && len(sql) >= 6 && sql[:6] == "INSERT" {
		version := args[0].(int64)
		s.rows[version] = Applied{Version: version, Name: args[1].(string), Checksum: args[2].(string)}
	}
	return nil
}

func (s *fakeSession) QueryRow(_ context.Context, _ string, args ...any) Row {
	version := args[0].(int64)
	row, ok := s.rows[version]
	return fakeRow{applied: row, ok: ok}
}

func (s *fakeSession) Close(context.Context) error { s.closed = true; return nil }

type fakeRow struct {
	applied Applied
	ok      bool
}

func (r fakeRow) Scan(dest ...any) error {
	if !r.ok {
		return ErrNoRows
	}
	switch len(dest) {
	case 1:
		*(dest[0].(*string)) = r.applied.Checksum
	case 2:
		*(dest[0].(*string)) = r.applied.Name
		*(dest[1].(*string)) = r.applied.Checksum
	case 3:
		*(dest[0].(*int64)) = r.applied.Version
		*(dest[1].(*string)) = r.applied.Name
		*(dest[2].(*string)) = r.applied.Checksum
	default:
		return fmt.Errorf("unexpected scan width %d", len(dest))
	}
	return nil
}

func TestLoadAndUp(t *testing.T) {
	files := fstest.MapFS{
		"0002_second.sql": {Data: []byte("SELECT 2")},
		"0001_first.sql":  {Data: []byte("SELECT 1")},
		"README.md":       {Data: []byte("ignored")},
	}
	migrations, err := Load(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 2 || migrations[0].Version != 1 || migrations[1].Version != 2 {
		t.Fatalf("unexpected migrations %#v", migrations)
	}

	session := &fakeSession{rows: make(map[int64]Applied)}
	applied, err := Up(context.Background(), fakeConnector{session}, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || len(session.rows) != 2 || !session.closed {
		t.Fatalf("unexpected applied state %#v closed=%v", applied, session.closed)
	}
}

func TestLoadRejectsDuplicateVersion(t *testing.T) {
	_, err := Load(fstest.MapFS{
		"0001_first.sql": {Data: []byte("SELECT 1")},
		"0001_again.sql": {Data: []byte("SELECT 1")},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate migration version") {
		t.Fatalf("expected duplicate version error, got %v", err)
	}
}

func TestVerifyRejectsChangedMigration(t *testing.T) {
	migration := Migration{Version: 1, Name: "0001_first", Checksum: "new"}
	session := &fakeSession{rows: map[int64]Applied{
		1: {Version: 1, Name: "0001_first", Checksum: "old"},
	}}
	err := Verify(context.Background(), fakeConnector{session}, []Migration{migration})
	if err == nil || errors.Is(err, ErrNoRows) {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}

func TestVerifyAppliedRequiresEveryMigrationWithoutMetadataWrite(t *testing.T) {
	session := &fakeSession{rows: map[int64]Applied{
		1: {Version: 1, Name: "0001_first", Checksum: "sum1"},
	}}
	err := VerifyApplied(context.Background(), fakeConnector{session}, []Migration{
		{Version: 1, Name: "0001_first", Checksum: "sum1"},
		{Version: 2, Name: "0002_second", Checksum: "sum2"},
	})
	if err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("expected pending migration error, got %v", err)
	}
	if len(session.execSQL) != 0 {
		t.Fatalf("VerifyApplied must not mutate database, execs=%#v", session.execSQL)
	}
	if !session.closed {
		t.Fatal("session was not closed")
	}
}

func TestVerifyAppliedPassesWhenAllMigrationsMatch(t *testing.T) {
	session := &fakeSession{rows: map[int64]Applied{
		1: {Version: 1, Name: "0001_first", Checksum: "sum1"},
		2: {Version: 2, Name: "0002_second", Checksum: "sum2"},
	}}
	err := VerifyApplied(context.Background(), fakeConnector{session}, []Migration{
		{Version: 1, Name: "0001_first", Checksum: "sum1"},
		{Version: 2, Name: "0002_second", Checksum: "sum2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(session.execSQL) != 0 {
		t.Fatalf("VerifyApplied must not mutate database, execs=%#v", session.execSQL)
	}
}
