package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/migrate"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [up|status|verify] [flags]\n", os.Args[0])
		flag.PrintDefaults()
	}
	migrationDir := flag.String("dir", "migrations", "directory containing versioned SQL migrations")
	flag.Parse()
	action := "status"
	if flag.NArg() > 0 {
		action = flag.Arg(0)
	}

	cfg, err := config.Load()
	if err != nil {
		fatal("invalid configuration", err)
	}
	ctx := context.Background()
	connector, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal("connect PostgreSQL", err)
	}
	defer connector.Close()

	directory := os.DirFS(filepath.Clean(*migrationDir))
	migrations, err := migrate.Load(directory)
	if err != nil {
		fatal("load migrations", err)
	}
	switch action {
	case "up":
		applied, err := migrate.Up(ctx, connector, migrations)
		if err != nil {
			fatal("apply migrations", err)
		}
		for _, migration := range applied {
			fmt.Printf("applied %04d %s %s\n", migration.Version, migration.Name, migration.Checksum)
		}
		fmt.Printf("ok: %d migration(s) applied\n", len(applied))
	case "verify":
		if err := migrate.Verify(ctx, connector, migrations); err != nil {
			fatal("verify migrations", err)
		}
		fmt.Printf("ok: %d migration file(s) verified\n", len(migrations))
	case "status":
		applied, err := migrate.Status(ctx, connector, migrations)
		if err != nil {
			fatal("migration status", err)
		}
		byVersion := make(map[int64]migrate.Applied, len(applied))
		for _, item := range applied {
			byVersion[item.Version] = item
		}
		for _, migration := range migrations {
			state := "pending"
			if _, ok := byVersion[migration.Version]; ok {
				state = "applied"
			}
			fmt.Printf("%04d %-8s %s\n", migration.Version, state, migration.Name)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func fatal(message string, err error) {
	slog.Error(message, "error", err)
	os.Exit(1)
}
