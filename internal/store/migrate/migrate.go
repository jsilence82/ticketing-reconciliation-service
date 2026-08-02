// Package migrate applies the embedded SQL migrations.
//
// This is a ~150-line applier rather than goose or golang-migrate — see
// docs/ARCHITECTURE.md, "Migration tooling" for why.
//
// Migrations are forward-only. There are no down migrations: rollback for a
// single-table schema is a Postgres restore (see docs/BACKUP.md), and a down
// migration that has never been run is a down migration that does not work.
package migrate

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
)

//go:embed sql/*.sql
var migrationFS embed.FS

// filePattern is deliberately strict. An unparsable filename is a hard error,
// never a silent skip — a migration that does not run because it was misnamed is
// the worst possible failure mode.
var filePattern = regexp.MustCompile(`^V(\d+)__([a-z0-9_]+)\.sql$`)

// advisoryLockKey serialises concurrent appliers. Two containers starting at
// once under Compose must not race to create the same table.
const advisoryLockKey int64 = 0x7472736D6967 // "trsmig"

// Migration is one embedded SQL file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum []byte
}

// Load reads and validates every embedded migration.
//
// It is separated from Apply so the filename and ordering rules can be tested
// without a database — those are the mistakes that happen on a clean checkout,
// and they should not need Postgres to catch.
func Load() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "sql")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var out []Migration
	seen := make(map[int]string)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		m := filePattern.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf(
				"migration %q does not match V{n}__{snake_case}.sql; "+
					"rename it rather than letting it be skipped", e.Name())
		}

		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", e.Name(), err)
		}
		if version < 1 {
			return nil, fmt.Errorf("migration %q: version must be >= 1", e.Name())
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q",
				version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile("sql/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}

		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     m[2],
			SQL:      string(body),
			Checksum: sum[:],
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	// Contiguity: a gap means a migration was deleted or never committed, and
	// applying the rest would produce a schema nobody has ever tested.
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf(
				"migration versions must be contiguous from 1; expected V%d, found V%d (%s)",
				i+1, m.Version, m.Name)
		}
	}

	return out, nil
}

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    int PRIMARY KEY,
    name       text NOT NULL,
    checksum   bytea NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// Apply brings the database up to the latest embedded migration.
//
// It is safe to call on every start: already-applied versions are skipped, so a
// no-op run costs one query.
func Apply(ctx context.Context, conn *pgx.Conn) (applied []int, err error) {
	migrations, err := Load()
	if err != nil {
		return nil, err
	}

	// Serialise appliers. Released automatically when the session ends, so a
	// crashed applier cannot wedge the next start.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if _, uerr := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey); uerr != nil && err == nil {
			err = fmt.Errorf("release migration lock: %w", uerr)
		}
	}()

	if _, err := conn.Exec(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	existing, err := loadApplied(ctx, conn)
	if err != nil {
		return nil, err
	}

	for _, m := range migrations {
		if prev, ok := existing[m.Version]; ok {
			// An edited migration means the database and the repo disagree about
			// what the schema is. Refuse rather than guess.
			if !equalBytes(prev, m.Checksum) {
				return applied, fmt.Errorf(
					"migration V%d__%s was already applied with a different checksum; "+
						"migrations are immutable — add a new one instead of editing it",
					m.Version, m.Name)
			}
			continue
		}

		if err := applyOne(ctx, conn, m); err != nil {
			return applied, err
		}
		applied = append(applied, m.Version)
	}

	return applied, nil
}

// applyOne runs a single migration and records it, atomically. If the DDL fails
// the record is rolled back with it, so a partial migration can never be
// mistaken for a complete one.
func applyOne(ctx context.Context, conn *pgx.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migration V%d__%s: begin: %w", m.Version, m.Name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("migration V%d__%s: %w", m.Version, m.Name, err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1,$2,$3)`,
		m.Version, m.Name, m.Checksum,
	); err != nil {
		return fmt.Errorf("migration V%d__%s: record: %w", m.Version, m.Name, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migration V%d__%s: commit: %w", m.Version, m.Name, err)
	}
	return nil
}

func loadApplied(ctx context.Context, conn *pgx.Conn) (map[int][]byte, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := make(map[int][]byte)
	for rows.Next() {
		var v int
		var sum []byte
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[v] = sum
	}
	return out, rows.Err()
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ErrNoMigrations signals an empty embed, which almost always means the
// //go:embed pattern stopped matching.
var ErrNoMigrations = errors.New("migrate: no migrations embedded")
