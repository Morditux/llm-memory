package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func historyCount(t *testing.T, s *Store, id int64) int {
	t.Helper()
	h, err := s.History(context.Background(), id)
	if err != nil {
		t.Fatalf("History(#%d): %v", id, err)
	}
	return len(h)
}

func ptr[T any](v T) *T { return &v }

func TestCreateDefaultsAndNormalisation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	m, err := s.Create(ctx, NewMemory{
		Namespace: "  Mon Projet ",
		Kind:      KindDecision,
		Title:     "  PostgreSQL\nplutôt   que MySQL ",
		Content:   "  Choix de PostgreSQL pour JSONB.  ",
		Tags:      []string{"DB, #postgres", "db"},
		Source:    "claude-desktop",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.Namespace != "mon-projet" {
		t.Errorf("namespace = %q", m.Namespace)
	}
	if m.Title != "PostgreSQL plutôt que MySQL" {
		t.Errorf("title = %q", m.Title)
	}
	if m.Content != "Choix de PostgreSQL pour JSONB." {
		t.Errorf("content = %q", m.Content)
	}
	if strings.Join(m.Tags, " ") != "db postgres" {
		t.Errorf("tags = %v", m.Tags)
	}
	if m.Importance != 2 || m.Source != "claude-desktop" || m.UpdatedBy != "claude-desktop" {
		t.Errorf("defaults: %+v", m)
	}
	if m.Archived() || m.CreatedAt.IsZero() || !m.UpdatedAt.Equal(m.CreatedAt) {
		t.Errorf("timestamps/archive: %+v", m)
	}

	// Implicit namespace creation.
	if _, err := s.RequireNamespace(ctx, "mon-projet"); err != nil {
		t.Errorf("namespace not created: %v", err)
	}

	// Default namespace and source.
	m2, err := s.Create(ctx, NewMemory{Kind: KindNote, Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Namespace != "global" || m2.Source != "unknown" || len(m2.Tags) != 0 {
		t.Errorf("defaults: %+v", m2)
	}
}

func TestCreateValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cases := map[string]NewMemory{
		"empty content":      {Kind: KindFact, Content: "   "},
		"too long":           {Kind: KindFact, Content: strings.Repeat("é", MaxContentRunes+1)},
		"bad kind":           {Kind: "opinion", Content: "x"},
		"bad importance":     {Kind: KindFact, Content: "x", Importance: 4},
		"bad namespace":      {Kind: KindFact, Content: "x", Namespace: "-oops"},
		"accented ns":        {Kind: KindFact, Content: "x", Namespace: "café"},
		"title too long":     {Kind: KindFact, Content: "x", Title: strings.Repeat("a", MaxTitleRunes+1)},
		"unknown supersedes": {Kind: KindFact, Content: "x", Supersedes: []int64{999}},
	}
	for name, in := range cases {
		if _, err := s.Create(ctx, in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// Exactly at the limit is accepted (runes, not bytes).
	if _, err := s.Create(ctx, NewMemory{Kind: KindFact, Content: strings.Repeat("é", MaxContentRunes)}); err != nil {
		t.Errorf("content at limit rejected: %v", err)
	}
	// Failed supersedes must not leave the new memory behind.
	all, _ := s.List(ctx, ListFilter{Archived: IncludeArchived})
	if len(all) != 1 {
		t.Errorf("got %d memories, want 1 (failed creations must roll back)", len(all))
	}
	var ve *ValidationError
	_, err := s.Create(ctx, NewMemory{Kind: "x", Content: "x"})
	if !errors.As(err, &ve) || ve.Field != "kind" || !strings.Contains(ve.Msg, "decision") {
		t.Errorf("kind error should list valid kinds: %v", err)
	}
}

func TestHistoryOneRowPerWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	m, err := s.Create(ctx, NewMemory{Kind: KindFact, Content: "v1", Source: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if n := historyCount(t, s, m.ID); n != 1 {
		t.Fatalf("after create: %d history rows", n)
	}
	if _, err := s.Update(ctx, m.ID, Patch{Content: ptr("v2")}, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Archive(ctx, m.ID, "b", "obsolete"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, m.ID, "cli"); err != nil {
		t.Fatal(err)
	}
	h, err := s.History(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		action  Action
		by      string
		content string
	}{
		{ActionCreate, "a", "v1"},
		{ActionUpdate, "b", "v2"},
		{ActionArchive, "b", "v2"},
		{ActionRestore, "cli", "v2"},
	}
	if len(h) != len(want) {
		t.Fatalf("history has %d rows, want %d", len(h), len(want))
	}
	for i, w := range want {
		var snap snapshot
		if err := json.Unmarshal([]byte(h[i].Snapshot), &snap); err != nil {
			t.Fatalf("row %d snapshot: %v", i, err)
		}
		if h[i].Action != w.action || h[i].ChangedBy != w.by || snap.Content != w.content {
			t.Errorf("row %d = %s by %s content %q; want %s by %s content %q",
				i, h[i].Action, h[i].ChangedBy, snap.Content, w.action, w.by, w.content)
		}
	}
	var archSnap snapshot
	_ = json.Unmarshal([]byte(h[2].Snapshot), &archSnap)
	if archSnap.Reason != "obsolete" || archSnap.ArchivedAt == nil {
		t.Errorf("archive snapshot = %+v", archSnap)
	}
}

func TestUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	m, _ := s.Create(ctx, NewMemory{Kind: KindFact, Content: "orig", Title: "t", Tags: []string{"a"}, Source: "a"})

	got, err := s.Update(ctx, m.ID, Patch{
		Title:      ptr("new title"),
		Tags:       ptr([]string{"B", "c"}),
		Importance: ptr(3),
		Kind:       ptr(KindDecision),
	}, "b")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Content != "orig" || got.Title != "new title" || strings.Join(got.Tags, ",") != "b,c" ||
		got.Importance != 3 || got.Kind != KindDecision || got.UpdatedBy != "b" || got.Source != "a" {
		t.Errorf("after update: %+v", got)
	}
	if got.UpdatedAt.Before(m.UpdatedAt) {
		t.Errorf("updated_at went backwards")
	}

	// Errors.
	if _, err := s.Update(ctx, m.ID, Patch{}, "b"); err == nil {
		t.Error("empty patch accepted")
	}
	if _, err := s.Update(ctx, 999, Patch{Title: ptr("x")}, "b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	var ve *ValidationError
	if _, err = s.Update(ctx, m.ID, Patch{Namespace: ptr("-bad")}, "b"); !errors.As(err, &ve) {
		t.Errorf("invalid namespace: %v", err)
	}
	if _, err := s.Update(ctx, m.ID, Patch{Content: ptr("")}, "b"); err == nil {
		t.Error("empty content accepted")
	}
	if n := historyCount(t, s, m.ID); n != 2 {
		t.Errorf("failed updates wrote history: %d rows, want 2", n)
	}

	// Move to a new namespace (created implicitly) and an existing namespace.
	got, err = s.Update(ctx, m.ID, Patch{Namespace: ptr("brand-new")}, "b")
	if err != nil || got.Namespace != "brand-new" {
		t.Errorf("move to new namespace: %+v, %v", got, err)
	}
	if _, err := s.EnsureNamespace(ctx, "Other", "desc"); err != nil {
		t.Fatal(err)
	}
	got, err = s.Update(ctx, m.ID, Patch{Namespace: ptr("other")}, "b")
	if err != nil || got.Namespace != "other" {
		t.Errorf("move namespace: %+v, %v", got, err)
	}

	// Archived memories cannot be updated.
	if _, err := s.Archive(ctx, m.ID, "b", ""); err != nil {
		t.Fatal(err)
	}
	var se *StateError
	if _, err := s.Update(ctx, m.ID, Patch{Title: ptr("x")}, "b"); !errors.As(err, &se) {
		t.Errorf("update archived: %v", err)
	}
}

func TestArchiveRestore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	m, _ := s.Create(ctx, NewMemory{Kind: KindFact, Content: "x"})

	a, err := s.Archive(ctx, m.ID, "b", "")
	if err != nil || !a.Archived() || a.UpdatedBy != "b" {
		t.Fatalf("Archive: %+v, %v", a, err)
	}
	var se *StateError
	if _, err := s.Archive(ctx, m.ID, "b", ""); !errors.As(err, &se) {
		t.Errorf("double archive: %v", err)
	}
	r, err := s.Restore(ctx, m.ID, "cli")
	if err != nil || r.Archived() {
		t.Fatalf("Restore: %+v, %v", r, err)
	}
	if _, err := s.Restore(ctx, m.ID, "cli"); !errors.As(err, &se) {
		t.Errorf("restore active: %v", err)
	}
	if _, err := s.Archive(ctx, 42, "b", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("archive unknown: %v", err)
	}
	if _, err := s.Restore(ctx, 42, "b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("restore unknown: %v", err)
	}
	if _, err := s.History(ctx, 42); !errors.Is(err, ErrNotFound) {
		t.Errorf("history unknown: %v", err)
	}
}

func TestSupersedes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	old1, _ := s.Create(ctx, NewMemory{Kind: KindDecision, Content: "MySQL"})
	old2, _ := s.Create(ctx, NewMemory{Kind: KindDecision, Content: "MariaDB"})
	_, _ = s.Archive(ctx, old2.ID, "x", "")

	n, err := s.Create(ctx, NewMemory{Kind: KindDecision, Content: "PostgreSQL", Source: "a", Supersedes: []int64{old1.ID, old2.ID}})
	if err != nil {
		t.Fatalf("Create with supersedes: %v", err)
	}
	g1, _ := s.Get(ctx, old1.ID)
	if !g1.Archived() {
		t.Error("superseded memory not archived")
	}
	h, _ := s.History(ctx, old1.ID)
	var snap snapshot
	_ = json.Unmarshal([]byte(h[len(h)-1].Snapshot), &snap)
	if h[len(h)-1].Action != ActionArchive || !strings.Contains(snap.Reason, "#") || h[len(h)-1].ChangedBy != "a" {
		t.Errorf("supersede history: %+v reason %q", h[len(h)-1], snap.Reason)
	}
	// Already archived: left as is (no extra history row).
	if c := historyCount(t, s, old2.ID); c != 2 {
		t.Errorf("already-archived superseded memory got %d history rows, want 2", c)
	}
	if n.Archived() {
		t.Error("new memory archived")
	}
}

func TestList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, _ := s.Create(ctx, NewMemory{Namespace: "p", Kind: KindFact, Content: "a"})
	b, _ := s.Create(ctx, NewMemory{Namespace: "p", Kind: KindDecision, Content: "b"})
	c, _ := s.Create(ctx, NewMemory{Namespace: "global", Kind: KindFact, Content: "c"})
	_, _ = s.Archive(ctx, a.ID, "x", "")

	ids := func(ms []Memory) []int64 {
		out := []int64{}
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	eq := func(got []int64, want ...int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	all, _ := s.List(ctx, ListFilter{})
	if !eq(ids(all), c.ID, b.ID) {
		t.Errorf("active list = %v", ids(all))
	}
	withArch, _ := s.List(ctx, ListFilter{Archived: IncludeArchived})
	if len(withArch) != 3 { // order between a and c depends on millisecond timing
		t.Errorf("include archived = %v", ids(withArch))
	}
	onlyArch, _ := s.List(ctx, ListFilter{Archived: ArchivedOnly})
	if !eq(ids(onlyArch), a.ID) {
		t.Errorf("archived only = %v", ids(onlyArch))
	}
	ns, _ := s.List(ctx, ListFilter{Namespace: "P", Archived: IncludeArchived})
	if len(ns) != 2 {
		t.Errorf("namespace p = %v", ids(ns))
	}
	facts, _ := s.List(ctx, ListFilter{Kinds: []Kind{KindFact}})
	if !eq(ids(facts), c.ID) {
		t.Errorf("facts = %v", ids(facts))
	}
	page, _ := s.List(ctx, ListFilter{Archived: IncludeArchived, Limit: 1, Offset: 1})
	if len(page) != 1 || page[0].ID != withArch[1].ID {
		t.Errorf("page = %v", ids(page))
	}
	if _, err := s.List(ctx, ListFilter{Namespace: "missing"}); err == nil {
		t.Error("unknown namespace accepted")
	}
	if _, err := s.List(ctx, ListFilter{Kinds: []Kind{"bogus"}}); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestOneActiveHandoffPerNamespace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	h1, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindHandoff, Content: "first", Source: "a"})
	other, _ := s.Create(ctx, NewMemory{Namespace: "api", Kind: KindHandoff, Content: "other ns", Source: "a"})
	h2, archived, err := s.CreateDetailed(ctx, NewMemory{Namespace: "demo", Kind: KindHandoff, Content: "second", Source: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0] != h1.ID {
		t.Fatalf("archived = %v, want [%d]", archived, h1.ID)
	}
	if g, _ := s.Get(ctx, h1.ID); !g.Archived() {
		t.Error("previous handoff still active")
	}
	if g, _ := s.Get(ctx, other.ID); g.Archived() {
		t.Error("handoff of another namespace archived")
	}
	h, _ := s.History(ctx, h1.ID)
	m, reason, err := h[len(h)-1].Decode()
	if err != nil || !m.Archived() || reason != fmt.Sprintf("replaced by handoff #%d", h2.ID) || h[len(h)-1].ChangedBy != "b" {
		t.Errorf("history: %+v reason=%q err=%v", h[len(h)-1], reason, err)
	}
	// Supersedes ids are reported only when actually archived.
	n, _ := s.Create(ctx, NewMemory{Kind: KindFact, Content: "x"})
	_, archived, _ = s.CreateDetailed(ctx, NewMemory{Kind: KindFact, Content: "y", Supersedes: []int64{n.ID, h1.ID}})
	if len(archived) != 1 || archived[0] != n.ID {
		t.Errorf("archived = %v, want [%d]", archived, n.ID)
	}

	// Restoring an archived handoff archives the currently active one.
	if _, err := s.Restore(ctx, h1.ID, "cli"); err != nil {
		t.Fatalf("Restore handoff: %v", err)
	}
	if g, _ := s.Get(ctx, h1.ID); g.Archived() {
		t.Error("restored handoff should be active")
	}
	if g, _ := s.Get(ctx, h2.ID); !g.Archived() {
		t.Error("previously active handoff should be archived on restore")
	}

	// Updating a non-handoff to KindHandoff or moving a handoff archives the target namespace's active handoff.
	note, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindNote, Content: "converted"})
	if _, err := s.Update(ctx, note.ID, Patch{Kind: ptr(KindHandoff)}, "b"); err != nil {
		t.Fatalf("Update to handoff: %v", err)
	}
	if g, _ := s.Get(ctx, h1.ID); !g.Archived() {
		t.Error("h1 should be archived after Update to KindHandoff")
	}
	if _, err := s.Update(ctx, note.ID, Patch{Namespace: ptr("api")}, "b"); err != nil {
		t.Fatalf("Move handoff: %v", err)
	}
	if g, _ := s.Get(ctx, other.ID); !g.Archived() {
		t.Error("other namespace active handoff should be archived when handoff moves in")
	}
}

func TestListTagsAndNamespaceDescription(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	m1, err := s.Create(ctx, NewMemory{
		Namespace:            "demo",
		NamespaceDescription: "Demo project",
		Kind:                 KindFact,
		Content:              "Uses Go",
		Tags:                 []string{"go", "backend"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindFact, Content: "Uses Go modules", Tags: []string{"mygo"}})

	nss, err := s.ListNamespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var desc string
	for _, ns := range nss {
		if ns.Name == "demo" {
			desc = ns.Description
		}
	}
	if desc != "Demo project" {
		t.Errorf("namespace description = %q, want %q", desc, "Demo project")
	}

	got, err := s.List(ctx, ListFilter{Tags: []string{"#GO"}})
	if err != nil || len(got) != 1 || got[0].ID != m1.ID {
		t.Errorf("List with tag filter = %+v, %v", got, err)
	}
	got, err = s.List(ctx, ListFilter{Tags: []string{"go", "backend"}})
	if err != nil || len(got) != 1 || got[0].ID != m1.ID {
		t.Errorf("List with multiple tags = %+v, %v", got, err)
	}
}
