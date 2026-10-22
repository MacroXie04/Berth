package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads migrations/NNNN_name.sql in version order and rejects
// gaps or duplicates, which would make PRAGMA user_version ambiguous.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("store: migration %q must be named NNNN_description.sql", e.Name())
		}
		body, err := fs.ReadFile(fsys, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migration versions must be contiguous from 1; got %s at position %d", m.name, i+1)
		}
	}
	return out, nil
}

// migrate applies every migration newer than PRAGMA user_version, each in its
// own transaction together with the version bump. Applied migrations are never
// edited; schema changes are always a new file.
func migrate(ctx context.Context, db *sql.DB) error {
	migs, err := loadMigrations(migrationsFS)
	if err != nil {
		return err
	}
	// A database written by a newer binary (say, after rolling back to an older
	// image) would otherwise open silently and fail per request in confusing ways.
	var current int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if current > len(migs) {
		return fmt.Errorf("store: database schema version %d is newer than this binary supports (%d); "+
			"upgrade berth or restore a matching database", current, len(migs))
	}
	for _, m := range migs {
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil) // immediate: version check and apply are atomic
	if err != nil {
		return fmt.Errorf("store: migration %s: begin: %w", m.name, err)
	}
	defer tx.Rollback()

	var current int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("store: migration %s: read version: %w", m.name, err)
	}
	if current >= m.version {
		return nil
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	// PRAGMA does not accept bound parameters; version is an int we parsed.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
		return fmt.Errorf("store: migration %s: set version: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration %s: commit: %w", m.name, err)
	}
	return nil
}
