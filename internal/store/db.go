// Package store holds the SQLite persistence layer. It never depends on the
// MCP SDK: it only speaks database/sql and plain Go types.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	// Pure-Go SQLite driver (registered as "sqlite"), FTS5 included.
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// Store is the persistence layer. It is safe for concurrent use; all access
// is serialised on a single SQLite connection.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) the database at path and applies pending
// migrations. The parent directory is created with mode 0700.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	return open(ctx, dbPath, embeddedMigrations)
}

func open(ctx context.Context, dbPath string, migrations fs.FS) (*Store, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path %q: %w", dbPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	_, statErr := os.Stat(abs)
	isNew := errors.Is(statErr, fs.ErrNotExist)

	db, err := sql.Open("sqlite", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", abs, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: abs}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database %s: %w", abs, err)
	}
	if isNew {
		// Memories may contain personal information: keep the file private.
		_ = os.Chmod(abs, 0o600)
	}
	if err := s.migrate(ctx, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds a SQLite URI. Percent-encoding the path keeps characters such as
// '?', '#' or '%' from being interpreted as URI syntax.
func dsn(abs string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") { // Windows drive letter: file:///C:/...
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	// BEGIN IMMEDIATE: a write transaction takes the write lock up front, so
	// two processes never deadlock on a read→write lock upgrade.
	q.Set("_txlock", "immediate")
	return u.String() + "?" + q.Encode()
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the absolute path of the database file.
func (s *Store) Path() string { return s.path }

// SchemaVersion returns the schema version stored in the database file.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

// LatestSchemaVersion is the schema version this binary migrates to.
func LatestSchemaVersion() int {
	ms, err := loadMigrations(embeddedMigrations)
	if err != nil || len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].version
}

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads migrations/NNNN_*.sql, sorted and checked to be
// numbered contiguously from 1.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	ms := make([]migration, 0, len(entries))
	for _, e := range entries {
		base := path.Base(e)
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("invalid migration file name %q (expected NNNN_name.sql)", base)
		}
		b, err := fs.ReadFile(fsys, e)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", base, err)
		}
		ms = append(ms, migration{version: v, name: base, sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered contiguously from 1: found %s at position %d", m.name, i+1)
		}
	}
	return ms, nil
}

func (s *Store) migrate(ctx context.Context, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	latest := 0
	if len(ms) > 0 {
		latest = ms[len(ms)-1].version
	}
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if current > latest {
		return fmt.Errorf("database %s has schema version %d, newer than the %d supported by this binary: upgrade llm-memory", s.path, current, latest)
	}
	for _, m := range ms[current:] {
		err := s.withTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("apply migration %s: %w", m.name, err)
			}
			// PRAGMA does not accept bound parameters; m.version is an int.
			if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.version)); err != nil {
				return fmt.Errorf("set schema version %d: %w", m.version, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// withTx runs fn in a transaction, committing on success.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
