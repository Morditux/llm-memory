package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// populated returns a store with namespaces, archived memories, updates and
// supersedes, so that every column and history action is exercised.
func populated(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureNamespace(ctx, "demo", "Projet de démo"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindDecision, Title: "BDD", Content: "MySQL", Tags: []string{"db"}, Source: "claude-desktop"})
	_, _ = s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindDecision, Title: "BDD", Content: "PostgreSQL pour JSONB", Tags: []string{"db", "postgres"}, Importance: 3, Source: "vscode", Supersedes: []int64{a.ID}})
	c, _ := s.Create(ctx, NewMemory{Kind: KindPreference, Content: "Réponses courtes", Source: "claude-code"})
	_, _ = s.Update(ctx, c.ID, Patch{Content: ptr("Réponses courtes, en français")}, "claude-desktop")
	d, _ := s.Create(ctx, NewMemory{Namespace: "api", Kind: KindNote, Content: "GraphQL ?", Source: "vscode"})
	_, _ = s.Archive(ctx, d.ID, "cli", "abandonné")
	_, _ = s.Restore(ctx, d.ID, "cli")
	_, _ = s.Create(ctx, NewMemory{Namespace: "api", Kind: KindHandoff, Content: "## Résumé\nEn cours", Source: "claude-desktop"})
	return s
}

// normalize clears what legitimately differs between two databases.
func normalize(d Dump) Dump {
	d.ExportedAt = time.Time{}
	for i := range d.Namespaces {
		d.Namespaces[i].CreatedAt = time.Time{} // "global" is created by each database
	}
	for i := range d.Memories {
		d.Memories[i].ID = 0
		for j := range d.Memories[i].History {
			var snap map[string]any
			_ = json.Unmarshal(d.Memories[i].History[j].Snapshot, &snap)
			delete(snap, "id")
			d.Memories[i].History[j].Snapshot, _ = json.Marshal(snap)
		}
	}
	return d
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := populated(t)
	for _, withHistory := range []bool{false, true} {
		d, err := src.Export(ctx, ExportOptions{WithHistory: withHistory})
		if err != nil {
			t.Fatal(err)
		}
		if d.FormatVersion != FormatVersion || d.SchemaVersion != LatestSchemaVersion() || len(d.Memories) != 5 || len(d.Namespaces) != 3 {
			t.Fatalf("dump: %+v", d)
		}

		// Through JSON, as the CLI does.
		var back Dump
		if err := json.Unmarshal([]byte(asJSON(t, d)), &back); err != nil {
			t.Fatal(err)
		}
		dst := newTestStore(t)
		rep, err := dst.Import(ctx, back, false, "cli")
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if rep.Imported != 5 || rep.Skipped != 0 || rep.NamespacesCreated != 2 {
			t.Errorf("report: %+v", rep)
		}
		d2, err := dst.Export(ctx, ExportOptions{WithHistory: withHistory})
		if err != nil {
			t.Fatal(err)
		}
		if !withHistory {
			// The importer records its own "create" line; compare memories only.
			if got, want := asJSON(t, normalize(d2)), asJSON(t, normalize(d)); got != want {
				t.Errorf("round trip differs\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			h, _ := dst.History(ctx, rep.ImportedIDs[0])
			if len(h) != 1 || h[0].Action != ActionCreate || h[0].ChangedBy != "cli" {
				t.Errorf("import history: %+v", h)
			}
			continue
		}
		if got, want := asJSON(t, normalize(d2)), asJSON(t, normalize(d)); got != want {
			t.Errorf("round trip with history differs\n--- got ---\n%s\n--- want ---\n%s", got, want)
		}
	}
}

func TestImportSkipsDuplicatesAndDryRun(t *testing.T) {
	ctx := context.Background()
	src := populated(t)
	d, _ := src.Export(ctx, ExportOptions{})

	dst := newTestStore(t)
	rep, err := dst.Import(ctx, d, true, "cli")
	if err != nil || !rep.DryRun || rep.Imported != 5 {
		t.Fatalf("dry run: %+v, %v", rep, err)
	}
	if all, _ := dst.List(ctx, ListFilter{Archived: IncludeArchived}); len(all) != 0 {
		t.Fatalf("dry run wrote %d memories", len(all))
	}
	if _, err := dst.RequireNamespace(ctx, "demo"); err == nil {
		t.Fatal("dry run created a namespace")
	}

	if _, err := dst.Import(ctx, d, false, "cli"); err != nil {
		t.Fatal(err)
	}
	rep, err = dst.Import(ctx, d, false, "cli")
	if err != nil || rep.Imported != 0 || rep.Skipped != 5 {
		t.Fatalf("second import: %+v, %v", rep, err)
	}
	// Importing into the source itself: everything is a duplicate.
	if rep, _ := src.Import(ctx, d, false, "cli"); rep.Skipped != 5 {
		t.Errorf("self import: %+v", rep)
	}
}

func TestImportValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	good := DumpMemory{Memory: Memory{Namespace: "x", Kind: KindFact, Content: "ok"}}

	var ve *ValidationError
	if _, err := s.Import(ctx, Dump{FormatVersion: 99}, false, "cli"); !errors.As(err, &ve) {
		t.Errorf("format version: %v", err)
	}
	bad := []DumpMemory{
		{Memory: Memory{Namespace: "x", Kind: "bogus", Content: "c"}},
		{Memory: Memory{Namespace: "x", Kind: KindFact, Content: ""}},
		{Memory: Memory{Namespace: "-x", Kind: KindFact, Content: "c"}},
		{Memory: Memory{Namespace: "x", Kind: KindFact, Content: "c", Importance: 9}},
		{Memory: Memory{Namespace: "x", Kind: KindFact, Content: "c"}, History: []DumpHistory{{Action: "delete", Snapshot: json.RawMessage(`{}`)}}},
	}
	for i, b := range bad {
		_, err := s.Import(ctx, Dump{FormatVersion: 1, Memories: []DumpMemory{good, b}}, false, "cli")
		if err == nil {
			t.Errorf("bad memory %d accepted", i)
		}
	}
	// All-or-nothing: the valid memory before the bad one was rolled back.
	if all, _ := s.List(ctx, ListFilter{Archived: IncludeArchived}); len(all) != 0 {
		t.Errorf("partial import left %d memories", len(all))
	}
	// Missing optional fields get defaults.
	rep, err := s.Import(ctx, Dump{FormatVersion: 1, Memories: []DumpMemory{good}}, false, "cli")
	if err != nil || rep.Imported != 1 {
		t.Fatalf("minimal import: %+v, %v", rep, err)
	}
	m, _ := s.Get(ctx, rep.ImportedIDs[0])
	if m.Importance != 2 || m.Source != "unknown" || m.CreatedAt.IsZero() {
		t.Errorf("defaults: %+v", m)
	}
}

func TestExportNamespace(t *testing.T) {
	ctx := context.Background()
	s := populated(t)
	d, err := s.Export(ctx, ExportOptions{Namespace: "demo"})
	if err != nil || len(d.Namespaces) != 1 || len(d.Memories) != 2 {
		t.Fatalf("namespace export: %d ns, %d memories, %v", len(d.Namespaces), len(d.Memories), err)
	}
	if _, err := s.Export(ctx, ExportOptions{Namespace: "nope"}); err == nil {
		t.Error("unknown namespace accepted")
	}
}

func TestPurgeAndStats(t *testing.T) {
	ctx := context.Background()
	s := populated(t)

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != 4 || st.Archived != 1 || st.SchemaVersion != LatestSchemaVersion() || st.FileBytes == 0 || st.Path != s.Path() {
		t.Errorf("stats: %+v", st)
	}
	if len(st.ByNamespace) != 3 || st.ByKind[0].Key != "decision" || st.ByKind[0].Active != 1 || st.ByKind[0].Archived != 1 {
		t.Errorf("stats groups: %+v / %+v", st.ByNamespace, st.ByKind)
	}

	past := time.Now().Add(-time.Hour)
	if n, _ := s.CountArchivedBefore(ctx, past); n != 0 {
		t.Errorf("archived before an hour ago: %d", n)
	}
	future := time.Now().Add(time.Hour)
	if n, _ := s.CountArchivedBefore(ctx, future); n != 1 {
		t.Errorf("archived before now+1h: %d", n)
	}
	n, err := s.Purge(ctx, future)
	if err != nil || n != 1 {
		t.Fatalf("Purge = %d, %v", n, err)
	}
	if all, _ := s.List(ctx, ListFilter{Archived: ArchivedOnly}); len(all) != 0 {
		t.Error("archived memory survived purge")
	}
	var hist, fts int
	_ = s.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_history WHERE memory_id NOT IN (SELECT id FROM memories)").Scan(&hist)
	_ = s.db.QueryRowContext(ctx, "SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'mysql'").Scan(&fts)
	if hist != 0 || fts != 0 {
		t.Errorf("orphans after purge: history=%d fts=%d", hist, fts)
	}
	if res, _ := s.Search(ctx, SearchQuery{Query: "postgresql"}); len(res) != 1 {
		t.Errorf("search after purge: %d", len(res))
	}
}

func TestImportIntoReopenedFile(t *testing.T) {
	ctx := context.Background()
	src := populated(t)
	d, _ := src.Export(ctx, ExportOptions{WithHistory: true})
	p := filepath.Join(t.TempDir(), "dst.db")
	dst, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Import(ctx, d, false, "cli"); err != nil {
		t.Fatal(err)
	}
	_ = dst.Close()
	dst, err = Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dst.Close() }()
	if res, _ := dst.Search(ctx, SearchQuery{Query: "graphql"}); len(res) != 1 {
		t.Errorf("FTS index not filled by import: %d", len(res))
	}
}

func TestExportImportPreservesExpiresAt(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	exp := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Millisecond)
	m, err := src.Create(ctx, NewMemory{
		Namespace: "ttl",
		Kind:      KindNote,
		Content:   "temporary note with TTL",
		ExpiresAt: &exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := src.Export(ctx, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dst := newTestStore(t)
	rep, err := dst.Import(ctx, d, false, "cli")
	if err != nil || rep.Imported != 1 {
		t.Fatalf("Import: %+v, %v", rep, err)
	}
	got, err := dst.Get(ctx, rep.ImportedIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(*m.ExpiresAt) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, m.ExpiresAt)
	}
}
