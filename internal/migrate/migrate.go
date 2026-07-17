package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const advisoryLockID int64 = 0x4732414d494752 // "G2AMIGR"

var migrationName = regexp.MustCompile(`^([0-9]+)_([a-z0-9_]+)\.sql$`)

// Connector returns one pinned PostgreSQL session. The session-scoped advisory
// lock and transaction must never hop between pooled connections.
type Connector interface {
	Acquire(context.Context) (Session, error)
}

type Session interface {
	Exec(context.Context, string, ...any) error
	QueryRow(context.Context, string, ...any) Row
	Close(context.Context) error
}

type Row interface {
	Scan(...any) error
}

type Migration struct {
	Version  int64
	Name     string
	SQL      string
	Checksum string
}

type Applied struct {
	Version  int64
	Name     string
	Checksum string
}

func Load(files fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	migrations := make([]Migration, 0, len(entries))
	seen := make(map[int64]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := migrationName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		if previous, ok := seen[version]; ok {
			return nil, fmt.Errorf("duplicate migration version %d: %s and %s", version, previous, entry.Name())
		}
		body, err := fs.ReadFile(files, entry.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
		seen[version] = entry.Name()
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

func Status(ctx context.Context, connector Connector, migrations []Migration) ([]Applied, error) {
	session, err := connector.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer session.Close(context.WithoutCancel(ctx))
	if err := ensureMetadata(ctx, session); err != nil {
		return nil, err
	}
	applied := make([]Applied, 0, len(migrations))
	for _, migration := range migrations {
		var got Applied
		err := session.QueryRow(ctx,
			"SELECT version, name, checksum FROM schema_migrations WHERE version = $1",
			migration.Version,
		).Scan(&got.Version, &got.Name, &got.Checksum)
		if err != nil {
			if isNoRows(err) {
				continue
			}
			return nil, err
		}
		applied = append(applied, got)
	}
	return applied, nil
}

func Verify(ctx context.Context, connector Connector, migrations []Migration) error {
	session, err := connector.Acquire(ctx)
	if err != nil {
		return err
	}
	defer session.Close(context.WithoutCancel(ctx))
	if err := ensureMetadata(ctx, session); err != nil {
		return err
	}
	return verifySession(ctx, session, migrations)
}

// VerifyApplied is the application-startup check: every migration file must be
// present in schema_migrations with the same name and checksum. Unlike Status,
// Verify, and Up, it does not create metadata, so a fresh or un-migrated
// database remains untouched and the runtime can fail closed.
func VerifyApplied(ctx context.Context, connector Connector, migrations []Migration) error {
	session, err := connector.Acquire(ctx)
	if err != nil {
		return err
	}
	defer session.Close(context.WithoutCancel(ctx))
	for _, migration := range migrations {
		var name, checksum string
		err := session.QueryRow(ctx,
			"SELECT name, checksum FROM schema_migrations WHERE version = $1",
			migration.Version,
		).Scan(&name, &checksum)
		if err != nil {
			if isNoRows(err) {
				return fmt.Errorf("migration %d (%s) is pending", migration.Version, migration.Name)
			}
			return err
		}
		if name != migration.Name || checksum != migration.Checksum {
			return fmt.Errorf("migration %d checksum/name mismatch: database=%s/%s files=%s/%s",
				migration.Version, name, checksum, migration.Name, migration.Checksum)
		}
	}
	return nil
}

func Up(ctx context.Context, connector Connector, migrations []Migration) (applied []Migration, err error) {
	session, err := connector.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeErr := session.Close(context.WithoutCancel(ctx))
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	if err := ensureMetadata(ctx, session); err != nil {
		return nil, err
	}
	if err := session.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockID); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockErr := session.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", advisoryLockID)
		if err == nil && unlockErr != nil {
			err = fmt.Errorf("release migration lock: %w", unlockErr)
		}
	}()

	if err := verifySession(ctx, session, migrations); err != nil {
		return nil, err
	}
	for _, migration := range migrations {
		var checksum string
		scanErr := session.QueryRow(ctx,
			"SELECT checksum FROM schema_migrations WHERE version = $1",
			migration.Version,
		).Scan(&checksum)
		if scanErr == nil {
			continue
		}
		if !isNoRows(scanErr) {
			return applied, scanErr
		}
		if err := session.Exec(ctx, "BEGIN"); err != nil {
			return applied, err
		}
		if err := session.Exec(ctx, migration.SQL); err != nil {
			_ = session.Exec(context.WithoutCancel(ctx), "ROLLBACK")
			return applied, fmt.Errorf("apply migration %d (%s): %w", migration.Version, migration.Name, err)
		}
		if err := session.Exec(ctx,
			"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
			migration.Version, migration.Name, migration.Checksum,
		); err != nil {
			_ = session.Exec(context.WithoutCancel(ctx), "ROLLBACK")
			return applied, fmt.Errorf("record migration %d: %w", migration.Version, err)
		}
		if err := session.Exec(ctx, "COMMIT"); err != nil {
			_ = session.Exec(context.WithoutCancel(ctx), "ROLLBACK")
			return applied, fmt.Errorf("commit migration %d: %w", migration.Version, err)
		}
		applied = append(applied, migration)
	}
	return applied, nil
}

func verifySession(ctx context.Context, session Session, migrations []Migration) error {
	for _, migration := range migrations {
		var name, checksum string
		err := session.QueryRow(ctx,
			"SELECT name, checksum FROM schema_migrations WHERE version = $1",
			migration.Version,
		).Scan(&name, &checksum)
		if err != nil {
			if isNoRows(err) {
				continue
			}
			return err
		}
		if name != migration.Name || checksum != migration.Checksum {
			return fmt.Errorf("migration %d checksum/name mismatch: database=%s/%s files=%s/%s",
				migration.Version, name, checksum, migration.Name, migration.Checksum)
		}
	}
	return nil
}

func ensureMetadata(ctx context.Context, session Session) error {
	return session.Exec(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version BIGINT PRIMARY KEY,
  name TEXT NOT NULL,
  checksum TEXT NOT NULL,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`)
}

func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrNoRows) || strings.Contains(strings.ToLower(err.Error()), "no rows")
}

// ErrNoRows lets driver adapters and tests expose a common sentinel.
var ErrNoRows = errors.New("no rows")
