package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesDirAndMigrates(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "nested", "dir with space")
	name := "m#e?m%.db" // URI-special characters
	if runtime.GOOS == "windows" {
		name = "m#e%m.db" // '?' is invalid in Windows filenames
	}
	p := filepath.Join(dir, name)
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := os.Stat(p); err != nil {
		t.Fatalf("database file not created at the exact path: %v", err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %o, want 700", st.Mode().Perm())
		}
		fst, _ := os.Stat(p)
		if fst.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %o, want 600", fst.Mode().Perm())
		}
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil || v != LatestSchemaVersion() || v < 1 {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, LatestSchemaVersion())
	}

	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	var fk int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v; want 1", fk, err)
	}
	var global int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM namespaces WHERE name='global'").Scan(&global); err != nil || global != 1 {
		t.Errorf("global namespace missing: %d, %v", global, err)
	}
}

func TestReopenDoesNotRemigrate(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "mem.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Create(ctx, NewMemory{Kind: KindFact, Content: "persisted"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Re-running 0001 would fail with "table namespaces already exists".
	s2, err := Open(ctx, p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	got, err := s2.Get(ctx, m.ID)
	if err != nil || got.Content != "persisted" {
		t.Fatalf("Get after reopen = %+v, %v", got, err)
	}
}

func TestRefuseNewerSchema(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "mem.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	_, err = Open(ctx, p)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Open newer schema: err = %v, want refusal", err)
	}
}

func TestIncrementalMigration(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "mem.db")
	v1 := fstest.MapFS{"migrations/0001_a.sql": {Data: []byte("CREATE TABLE a(x);")}}
	s, err := open(ctx, p, v1)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	v2 := fstest.MapFS{
		"migrations/0001_a.sql": {Data: []byte("CREATE TABLE a(x);")},
		"migrations/0002_b.sql": {Data: []byte("CREATE TABLE b(y); INSERT INTO a VALUES (1);")},
	}
	s, err = open(ctx, p, v2)
	if err != nil {
		t.Fatalf("migrate to v2: %v", err)
	}
	defer func() { _ = s.Close() }()
	if v, _ := s.SchemaVersion(ctx); v != 2 {
		t.Errorf("version = %d, want 2", v)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM a").Scan(&n); err != nil || n != 1 {
		t.Errorf("0002 applied %d times (%v), want once", n, err)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "mem.db")
	bad := fstest.MapFS{"migrations/0001_bad.sql": {Data: []byte("CREATE TABLE ok(x); THIS IS NOT SQL;")}}
	if _, err := open(ctx, p, bad); err == nil {
		t.Fatal("expected migration error")
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v, n int
	_ = db.QueryRow("PRAGMA user_version").Scan(&v)
	_ = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='ok'").Scan(&n)
	if v != 0 || n != 0 {
		t.Errorf("after failed migration: user_version=%d, table ok present=%d; want 0, 0", v, n)
	}
}

func TestLoadMigrationsValidation(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"bad name": {"migrations/abc.sql": {Data: []byte("")}},
		"gap": {
			"migrations/0001_a.sql": {Data: []byte("")},
			"migrations/0003_c.sql": {Data: []byte("")},
		},
	}
	for name, fsys := range cases {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
