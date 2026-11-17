package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestNewUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		if !validUUID(u) {
			t.Fatalf("invalid uuid %q", u)
		}
		if seen[u] {
			t.Fatalf("duplicate uuid %q", u)
		}
		seen[u] = true
	}
	for _, bad := range []string{"", "x", strings.ToUpper("0a1b2c3d-0000-4000-8000-000000000000"), "0a1b2c3d-0000-1000-8000-000000000000"} {
		if validUUID(bad) {
			t.Errorf("validUUID(%q) = true", bad)
		}
	}
}

func TestCreateAssignsUUIDAndSnapshotCarriesIt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, err := s.Create(ctx, NewMemory{Kind: KindFact, Content: "a", Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, NewMemory{Kind: KindFact, Content: "b", Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !validUUID(a.UUID) || !validUUID(b.UUID) || a.UUID == b.UUID {
		t.Fatalf("uuids = %q, %q", a.UUID, b.UUID)
	}
	got, err := s.Get(ctx, a.ID)
	if err != nil || got.UUID != a.UUID {
		t.Fatalf("Get uuid = %q, %v; want %q", got.UUID, err, a.UUID)
	}
	// The uuid survives updates and archive/restore.
	c := "a2"
	if _, err := s.Update(ctx, a.ID, Patch{Content: &c}, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, a.ID)
	if got.UUID != a.UUID {
		t.Errorf("uuid changed by Update: %q -> %q", a.UUID, got.UUID)
	}
	h, err := s.History(ctx, a.ID)
	if err != nil || len(h) != 2 || !strings.Contains(h[1].Snapshot, a.UUID) {
		t.Errorf("history snapshot lacks the uuid: %+v, %v", h, err)
	}
}

func TestUUIDTriggerFillsRawInsert(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO memories(kind, content) VALUES ('note', 'raw')"); err != nil {
		t.Fatal(err)
	}
	var u string
	if err := s.db.QueryRowContext(ctx, "SELECT uuid FROM memories WHERE content = 'raw'").Scan(&u); err != nil {
		t.Fatal(err)
	}
	if !validUUID(u) {
		t.Errorf("trigger uuid = %q", u)
	}
	// Unique index.
	if _, err := s.db.ExecContext(ctx, "INSERT INTO memories(kind, content, uuid) VALUES ('note', 'dup', ?)", u); err == nil {
		t.Error("duplicate uuid accepted")
	}
}

func TestInstanceID(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "mem.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.InstanceID(ctx)
	if err != nil || !validUUID(id) {
		t.Fatalf("InstanceID = %q, %v", id, err)
	}
	_ = s.Close()
	s, err = Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if again, _ := s.InstanceID(ctx); again != id {
		t.Errorf("instance id changed across reopen: %q -> %q", id, again)
	}
	other := newTestStore(t)
	if oid, _ := other.InstanceID(ctx); oid == id {
		t.Error("two databases share an instance id")
	}
}

// TestMigrateFromV4BackfillsUUIDs opens a database at schema 4 with existing
// rows (as a pre-multi-machine install would have), then migrates to latest.
func TestMigrateFromV4BackfillsUUIDs(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "mem.db")

	v4 := fstest.MapFS{}
	for _, n := range []string{"0001_initial.sql", "0002_embeddings.sql", "0003_expires_at.sql", "0004_links.sql"} {
		b, err := fs.ReadFile(embeddedMigrations, "migrations/"+n)
		if err != nil {
			t.Fatal(err)
		}
		v4["migrations/"+n] = &fstest.MapFile{Data: b}
	}
	old, err := open(ctx, p, v4)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"one", "two", "three"} {
		if _, err := old.db.ExecContext(ctx, "INSERT INTO memories(kind, content) VALUES ('note', ?)", c); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := old.SchemaVersion(ctx); v != 4 {
		t.Fatalf("setup schema = %d, want 4", v)
	}
	_ = old.Close()

	s, err := Open(ctx, p)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer func() { _ = s.Close() }()
	if v, _ := s.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 5 {
		t.Fatalf("schema = %d, want %d", v, LatestSchemaVersion())
	}
	ms, err := s.List(ctx, ListFilter{Archived: IncludeArchived})
	if err != nil || len(ms) != 3 {
		t.Fatalf("List: %d rows, %v", len(ms), err)
	}
	seen := map[string]bool{}
	for _, m := range ms {
		if !validUUID(m.UUID) || seen[m.UUID] {
			t.Errorf("memory #%d: bad or duplicate uuid %q", m.ID, m.UUID)
		}
		seen[m.UUID] = true
	}
	if id, err := s.InstanceID(ctx); err != nil || !validUUID(id) {
		t.Errorf("InstanceID after migration = %q, %v", id, err)
	}
}
