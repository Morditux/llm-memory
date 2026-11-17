package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

// tick separates two writes by more than the millisecond resolution of
// updated_at, so "which edit is newer" is not left to chance.
func tick() { time.Sleep(4 * time.Millisecond) }

// state is a fingerprint of everything a merge must make equal: namespaces,
// memories (with identity and timestamps) and links.
func state(t *testing.T, s *Store) string {
	t.Helper()
	d, err := s.Export(context.Background(), ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, n := range d.Namespaces {
		lines = append(lines, "ns|"+n.Name)
	}
	for _, m := range d.Memories {
		arch := ""
		if m.ArchivedAt != nil {
			arch = FormatTime(*m.ArchivedAt)
		}
		lines = append(lines, strings.Join([]string{"mem", m.UUID, m.Namespace, string(m.Kind), m.Title, m.Content,
			strings.Join(m.Tags, ","), string(rune('0' + m.Importance)), m.Source, m.UpdatedBy,
			FormatTime(m.CreatedAt), FormatTime(m.UpdatedAt), arch}, "|"))
	}
	for _, l := range d.Links {
		lines = append(lines, strings.Join([]string{"link", l.Src, l.Dst, string(l.Kind), FormatTime(l.CreatedAt)}, "|"))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// ship exports src (with history) and merges it into dst, through JSON as the
// CLI does.
func ship(t *testing.T, src, dst *Store) MergeReport {
	t.Helper()
	ctx := context.Background()
	d, err := src.Export(ctx, ExportOptions{WithHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	var back Dump
	if err := json.Unmarshal([]byte(asJSON(t, d)), &back); err != nil {
		t.Fatal(err)
	}
	rep, err := dst.Merge(ctx, back, MergeOptions{}, false, "test")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return rep
}

// converge ships both ways and fails unless both databases end identical.
func converge(t *testing.T, a, b *Store) {
	t.Helper()
	ship(t, a, b)
	ship(t, b, a)
	ship(t, a, b) // b now holds what a took from b
	if sa, sb := state(t, a), state(t, b); sa != sb {
		t.Fatalf("databases differ after merging both ways\n--- a ---\n%s\n--- b ---\n%s", sa, sb)
	}
}

func idOf(t *testing.T, s *Store, content string) int64 {
	t.Helper()
	ms, err := s.List(context.Background(), ListFilter{Archived: IncludeArchived})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Content == content {
			return m.ID
		}
	}
	t.Fatalf("no memory with content %q", content)
	return 0
}

func TestMergeIntoEmptyReproducesSourceAndIsIdempotent(t *testing.T) {
	src, dst := populated(t), newTestStore(t)
	rep := ship(t, src, dst)
	if rep.Inserted != 5 || rep.Updated != 0 || rep.LinksAdded != 1 {
		t.Fatalf("first merge: %+v", rep)
	}
	if a, b := state(t, src), state(t, dst); a != b {
		t.Fatalf("destination differs from source\n--- src ---\n%s\n--- dst ---\n%s", a, b)
	}
	rep = ship(t, src, dst)
	if rep.Inserted != 0 || rep.Updated != 0 || rep.KeptLocal != 0 || rep.Unchanged != 5 || rep.LinksAdded != 0 || rep.HistoryAdded != 0 {
		t.Errorf("second merge is not a no-op: %+v", rep)
	}
	// Histories match line for line (same count, ordered by time).
	for _, c := range []string{"MySQL", "Réponses courtes, en français"} {
		hs, _ := src.History(context.Background(), idOf(t, src, c))
		hd, _ := dst.History(context.Background(), idOf(t, dst, c))
		if len(hs) != len(hd) {
			t.Errorf("history of %q: %d lines in source, %d in destination", c, len(hs), len(hd))
		}
	}
}

func TestMergeDoesNotBumpUpdatedAt(t *testing.T) {
	ctx := context.Background()
	src, dst := populated(t), newTestStore(t)
	ship(t, src, dst)
	a, _ := src.Get(ctx, idOf(t, src, "PostgreSQL pour JSONB"))
	b, _ := dst.Get(ctx, idOf(t, dst, "PostgreSQL pour JSONB"))
	if !a.UpdatedAt.Equal(b.UpdatedAt) || a.UUID != b.UUID || a.UpdatedBy != b.UpdatedBy {
		t.Errorf("merge altered the row: %+v vs %+v", a, b)
	}
}

func TestMergeDivergedDatabasesConverge(t *testing.T) {
	ctx := context.Background()
	a := populated(t)
	b := newTestStore(t)
	ship(t, a, b) // common ancestor

	// a edits, adds, links; b archives, adds, links, and creates a namespace.
	tick()
	if _, err := a.Update(ctx, idOf(t, a, "Réponses courtes, en français"), Patch{Content: ptr("Réponses brèves, en français")}, "a-cli"); err != nil {
		t.Fatal(err)
	}
	w, _ := a.Create(ctx, NewMemory{Namespace: "demo", Kind: KindFact, Content: "only on a", Source: "a-cli"})
	tick()
	if _, err := b.Archive(ctx, idOf(t, b, "GraphQL ?"), "b-cli", "done"); err != nil {
		t.Fatal(err)
	}
	z, _ := b.Create(ctx, NewMemory{Namespace: "newns", Kind: KindNote, Content: "only on b", Source: "b-cli"})
	tick()
	if err := a.AddLink(ctx, w.ID, idOf(t, a, "PostgreSQL pour JSONB"), LinkRelatesTo, "a-cli"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddLink(ctx, z.ID, idOf(t, b, "## Résumé\nEn cours"), LinkDependsOn, "b-cli"); err != nil {
		t.Fatal(err)
	}

	converge(t, a, b)

	got, err := a.Get(ctx, idOf(t, a, "Réponses brèves, en français"))
	if err != nil || got.UpdatedBy != "a-cli" {
		t.Errorf("a's edit lost: %+v, %v", got, err)
	}
	if m, _ := b.Get(ctx, idOf(t, b, "GraphQL ?")); !m.Archived() {
		t.Error("b's archive not carried to b itself")
	}
	if m, _ := a.Get(ctx, idOf(t, a, "GraphQL ?")); !m.Archived() {
		t.Error("b's archive did not reach a")
	}
	if _, err := a.RequireNamespace(ctx, "newns"); err != nil {
		t.Errorf("namespace newns did not reach a: %v", err)
	}
	if ls, _ := a.Links(ctx, w.ID); len(ls) != 1 {
		t.Errorf("a's link: %+v", ls)
	}
}

func TestMergeConflictNewerWins(t *testing.T) {
	ctx := context.Background()
	a, b := newTestStore(t), newTestStore(t)
	m, _ := a.Create(ctx, NewMemory{Kind: KindDecision, Title: "DB", Content: "MySQL", Source: "a-cli"})
	ship(t, a, b)
	tick()
	if _, err := a.Update(ctx, m.ID, Patch{Content: ptr("Postgres")}, "a-cli"); err != nil {
		t.Fatal(err)
	}
	tick()
	bid := idOf(t, b, "MySQL")
	if _, err := b.Update(ctx, bid, Patch{Content: ptr("SQLite")}, "b-cli"); err != nil { // b is the later edit
		t.Fatal(err)
	}

	// b's version is newer: merging b into a replaces a's, merging a into b keeps b's.
	rep := ship(t, b, a)
	if rep.Updated != 1 || rep.KeptLocal != 0 || len(rep.Changes) != 1 || rep.Changes[0].Result != "updated" {
		t.Fatalf("b into a: %+v", rep)
	}
	rep = ship(t, a, b)
	if rep.Updated != 0 || rep.Unchanged != 1 {
		t.Fatalf("a into b after convergence: %+v", rep)
	}
	got, _ := a.Get(ctx, m.ID)
	if got.Content != "SQLite" || got.UpdatedBy != "b-cli" {
		t.Errorf("a after merge: %+v", got)
	}
	// a's replaced version is still in its history, followed by the merge line.
	hs, _ := a.History(ctx, m.ID)
	var sawOld, sawMerge bool
	for _, h := range hs {
		mm, reason, _ := h.Decode()
		sawOld = sawOld || mm.Content == "Postgres"
		sawMerge = sawMerge || strings.HasPrefix(reason, "merged: newer version from")
	}
	if !sawOld || !sawMerge {
		t.Errorf("history lacks the replaced version (%v) or the merge line (%v): %+v", sawOld, sawMerge, hs)
	}
	if state(t, a) != state(t, b) {
		t.Error("not converged")
	}

	// Reverse direction on a fresh pair: the older side is kept out.
	c, d := newTestStore(t), newTestStore(t)
	n, _ := c.Create(ctx, NewMemory{Kind: KindFact, Content: "v1", Source: "t"})
	ship(t, c, d)
	tick()
	_, _ = c.Update(ctx, n.ID, Patch{Content: ptr("v2")}, "c")
	if rep := ship(t, d, c); rep.KeptLocal != 1 || rep.Updated != 0 || rep.Changes[0].Result != "kept_local" {
		t.Errorf("older incoming: %+v", rep)
	}
	if g, _ := c.Get(ctx, n.ID); g.Content != "v2" {
		t.Errorf("older version overwrote the newer: %+v", g)
	}
}

func TestMergeTieBreakDoesNotDependOnDirection(t *testing.T) {
	ctx := context.Background()
	a, b := newTestStore(t), newTestStore(t)
	m, _ := a.Create(ctx, NewMemory{Kind: KindFact, Content: "base", Source: "t"})
	ship(t, a, b)
	_, _ = a.Update(ctx, m.ID, Patch{Content: ptr("from a")}, "t")
	_, _ = b.Update(ctx, idOf(t, b, "base"), Patch{Content: ptr("from b")}, "t")
	// Force the same timestamp on both sides.
	const ts = "2030-01-01T00:00:00.000Z"
	for _, s := range []*Store{a, b} {
		if _, err := s.db.ExecContext(ctx, "UPDATE memories SET updated_at = ?", ts); err != nil {
			t.Fatal(err)
		}
	}
	converge(t, a, b)
	got, _ := a.Get(ctx, idOf(t, a, "from b"))
	if got.Content != "from b" { // "from b" > "from a" in the canonical order
		t.Errorf("tie-break picked %q", got.Content)
	}
}

func TestMergePairsIndependentTwinsOnSmallerUUID(t *testing.T) {
	ctx := context.Background()
	a, b := newTestStore(t), newTestStore(t)
	ma, _ := a.Create(ctx, NewMemory{Kind: KindFact, Content: "same fact", Source: "a"})
	mb, _ := b.Create(ctx, NewMemory{Kind: KindFact, Content: "same fact", Source: "b"})
	if ma.UUID == mb.UUID {
		t.Fatal("setup: same uuid")
	}
	rep := ship(t, b, a)
	if rep.TwinsMatched != 1 || rep.Inserted != 0 {
		t.Fatalf("b into a: %+v", rep)
	}
	converge(t, a, b)
	want := ma.UUID
	if mb.UUID < want {
		want = mb.UUID
	}
	for name, s := range map[string]*Store{"a": a, "b": b} {
		ms, _ := s.List(ctx, ListFilter{})
		if len(ms) != 1 || ms[0].UUID != want {
			t.Errorf("%s: %+v, want one memory with uuid %s", name, ms, want)
		}
	}
}

func TestMergeNoContentMatchKeepsBoth(t *testing.T) {
	ctx := context.Background()
	a, b := newTestStore(t), newTestStore(t)
	_, _ = a.Create(ctx, NewMemory{Kind: KindFact, Content: "same fact", Source: "a"})
	_, _ = b.Create(ctx, NewMemory{Kind: KindFact, Content: "same fact", Source: "b"})
	d, _ := b.Export(ctx, ExportOptions{})
	rep, err := a.Merge(ctx, d, MergeOptions{NoContentMatch: true}, false, "t")
	if err != nil || rep.Inserted != 1 || rep.TwinsMatched != 0 {
		t.Fatalf("%+v, %v", rep, err)
	}
	if ms, _ := a.List(ctx, ListFilter{}); len(ms) != 2 {
		t.Errorf("want both memories, got %d", len(ms))
	}
}

func TestMergeTwinIsNotARowTheDumpAlreadyDescribes(t *testing.T) {
	ctx := context.Background()
	const u1, u2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	s := newTestStore(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mk := func(u string) DumpMemory {
		return DumpMemory{Memory: Memory{UUID: u, Namespace: "global", Kind: KindFact, Content: "a", CreatedAt: at, UpdatedAt: at}}
	}
	d := Dump{FormatVersion: 2, Memories: []DumpMemory{mk(u1), mk(u2)}}
	if rep, err := s.Merge(ctx, d, MergeOptions{}, false, "t"); err != nil || rep.Inserted != 2 || rep.TwinsMatched != 0 {
		t.Fatalf("empty target: %+v, %v", rep, err)
	}
	// Again: both found by uuid, nothing paired by content.
	if rep, err := s.Merge(ctx, d, MergeOptions{}, false, "t"); err != nil || rep.Unchanged != 2 || rep.TwinsMatched != 0 || rep.Inserted != 0 {
		t.Fatalf("second merge: %+v, %v", rep, err)
	}
}

func TestMergeValidationAndDryRun(t *testing.T) {
	ctx := context.Background()
	src := populated(t)
	var ve *ValidationError

	v1, _ := src.Export(ctx, ExportOptions{FormatVersion: 1})
	if _, err := newTestStore(t).Merge(ctx, v1, MergeOptions{}, false, "t"); !errorsAs(err, &ve) {
		t.Errorf("v1 dump: %v", err)
	}
	d, _ := src.Export(ctx, ExportOptions{})
	d.Memories[2].UUID = ""
	if _, err := newTestStore(t).Merge(ctx, d, MergeOptions{}, false, "t"); !errorsAs(err, &ve) {
		t.Errorf("missing uuid: %v", err)
	}

	d, _ = src.Export(ctx, ExportOptions{})
	dst := newTestStore(t)
	rep, err := dst.Merge(ctx, d, MergeOptions{}, true, "t")
	if err != nil || !rep.DryRun || rep.Inserted != 5 || rep.LinksAdded != 1 {
		t.Fatalf("dry run: %+v, %v", rep, err)
	}
	if all, _ := dst.List(ctx, ListFilter{Archived: IncludeArchived}); len(all) != 0 {
		t.Errorf("dry run wrote %d memories", len(all))
	}
	// A bad memory in the middle rolls everything back.
	d.Memories[3].Kind = "bogus"
	if _, err := dst.Merge(ctx, d, MergeOptions{}, false, "t"); err == nil {
		t.Error("bad memory accepted")
	}
	if all, _ := dst.List(ctx, ListFilter{Archived: IncludeArchived}); len(all) != 0 {
		t.Errorf("failed merge left %d memories", len(all))
	}
}

func TestMergeLinkToUnknownEndIsSkippedAndKnownLocalEndResolves(t *testing.T) {
	ctx := context.Background()
	a, b := newTestStore(t), newTestStore(t)
	ma, _ := a.Create(ctx, NewMemory{Kind: KindFact, Content: "one", Source: "t"})
	mb, _ := a.Create(ctx, NewMemory{Kind: KindFact, Content: "two", Source: "t"})
	ship(t, a, b)
	// A partial dump: only "one", plus a link to "two" (already in b) and one to nowhere.
	d, _ := a.Export(ctx, ExportOptions{})
	d.Memories = d.Memories[:1]
	const ghost = "33333333-3333-4333-8333-333333333333"
	d.Links = []DumpLink{
		{Src: ma.UUID, Dst: mb.UUID, Kind: LinkDependsOn},
		{Src: ma.UUID, Dst: ghost, Kind: LinkRelatesTo},
	}
	rep, err := b.Merge(ctx, d, MergeOptions{}, false, "t")
	if err != nil || rep.LinksAdded != 1 || rep.LinksSkipped != 1 {
		t.Errorf("%+v, %v", rep, err)
	}
}

func TestMergeHistoryUnionDoesNotDuplicate(t *testing.T) {
	ctx := context.Background()
	a, b := newTestStore(t), newTestStore(t)
	m, _ := a.Create(ctx, NewMemory{Kind: KindFact, Content: "h", Source: "t"})
	tick()
	_, _ = a.Update(ctx, m.ID, Patch{Content: ptr("h2")}, "t")
	ship(t, a, b)
	ship(t, b, a)
	ship(t, a, b)
	ha, _ := a.History(ctx, m.ID)
	hb, _ := b.History(ctx, idOf(t, b, "h2"))
	if len(ha) != 2 || len(hb) != 2 {
		t.Errorf("histories: %d in a, %d in b, want 2 and 2", len(ha), len(hb))
	}
	if ha[0].Action != ActionCreate || hb[0].Action != ActionCreate {
		t.Errorf("history order: %v / %v", ha[0].Action, hb[0].Action)
	}
}

func errorsAs(err error, target **ValidationError) bool { return errors.As(err, target) }
