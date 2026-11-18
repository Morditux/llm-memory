package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeNamespace(t *testing.T) {
	ok := map[string]string{
		"global":                "global",
		"  Mon Projet  ":        "mon-projet",
		"a\tb  c":               "a-b-c",
		"api_v2":                "api_v2",
		"9lives":                "9lives",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
	}
	for in, want := range ok {
		got, err := NormalizeNamespace(in)
		if err != nil || got != want {
			t.Errorf("NormalizeNamespace(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "-x", "_x", "a/b", "café", "a.b", strings.Repeat("a", 65)} {
		if got, err := NormalizeNamespace(in); err == nil {
			t.Errorf("NormalizeNamespace(%q) = %q, want error", in, got)
		}
	}
}

func TestNormalizeTags(t *testing.T) {
	got := NormalizeTags([]string{" Go, SQL ", "#go", "", "fts5\tsqlite", ",,"})
	if strings.Join(got, " ") != "go sql fts5 sqlite" {
		t.Errorf("NormalizeTags = %v", got)
	}
	if got := NormalizeTags(nil); got == nil || len(got) != 0 {
		t.Errorf("NormalizeTags(nil) = %#v, want empty non-nil", got)
	}
}

func TestNamespaces(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.RequireNamespace(ctx, "Global"); err != nil {
		t.Errorf("global should exist: %v", err)
	}
	var nf *NamespaceNotFoundError
	_, err := s.RequireNamespace(ctx, "demo")
	if !errors.As(err, &nf) || nf.Name != "demo" || strings.Join(nf.Existing, ",") != "global" {
		t.Fatalf("RequireNamespace(demo) = %v", err)
	}
	if !strings.Contains(err.Error(), "global") {
		t.Errorf("error should list existing namespaces: %v", err)
	}

	name, err := s.EnsureNamespace(ctx, "Demo", "projet de démo")
	if err != nil || name != "demo" {
		t.Fatalf("EnsureNamespace = %q, %v", name, err)
	}
	// Idempotent; empty description keeps the previous one.
	if _, err := s.EnsureNamespace(ctx, "demo", ""); err != nil {
		t.Fatal(err)
	}

	a, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindFact, Content: "a"})
	_, _ = s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindFact, Content: "b"})
	_, _ = s.Archive(ctx, a.ID, "x", "")

	list, err := s.ListNamespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListNamespaces = %+v", list)
	}
	demo, global := list[0], list[1]
	if demo.Name != "demo" || demo.Description != "projet de démo" || demo.ActiveCount != 1 || demo.LastActivity == nil {
		t.Errorf("demo = %+v", demo)
	}
	if global.Name != "global" || global.ActiveCount != 0 || global.LastActivity != nil || global.Description == "" {
		t.Errorf("global = %+v", global)
	}
	archived, _ := s.Get(ctx, a.ID)
	if !demo.LastActivity.Equal(archived.UpdatedAt) {
		t.Errorf("last activity %v, want archive time %v", demo.LastActivity, archived.UpdatedAt)
	}
}

func TestNamespaceRenameMergeArchive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Cannot rename or merge global.
	if _, err := s.RenameNamespace(ctx, "global", "other", "cli"); err == nil {
		t.Error("expected error renaming global")
	}
	if _, err := s.MergeNamespace(ctx, "global", "other", "cli"); err == nil {
		t.Error("expected error merging global")
	}

	// Create two namespaces with memories and handoffs.
	m1, err := s.Create(ctx, NewMemory{Namespace: "alpha", Kind: KindFact, Content: "fact alpha", Source: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	h1, err := s.Create(ctx, NewMemory{Namespace: "alpha", Kind: KindHandoff, Content: "handoff alpha", Source: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	tick()
	h2, err := s.Create(ctx, NewMemory{Namespace: "beta", Kind: KindHandoff, Content: "handoff beta", Source: "dave"})
	if err != nil {
		t.Fatal(err)
	}

	// Rename alpha -> gamma records an update history row without altering updated_at/updated_by.
	tick()
	got, err := s.RenameNamespace(ctx, "alpha", "gamma", "alice")
	if err != nil || got != "gamma" {
		t.Fatalf("RenameNamespace = %q, %v", got, err)
	}
	m1After, err := s.Get(ctx, m1.ID)
	if err != nil || m1After.Namespace != "gamma" || !m1After.UpdatedAt.Equal(m1.UpdatedAt) || m1After.UpdatedBy != "carol" {
		t.Fatalf("m1 = %+v (want namespace=gamma, updated_at=%v, updated_by=carol), %v", m1After, m1.UpdatedAt, err)
	}
	hist1, err := s.History(ctx, m1.ID)
	if err != nil || len(hist1) != 2 || hist1[1].Action != ActionUpdate || hist1[1].ChangedBy != "alice" {
		t.Fatalf("m1 history after rename = %+v, %v", hist1, err)
	}
	snap1, reason1, err := hist1[1].Decode()
	if err != nil || snap1.Namespace != "gamma" || reason1 != `namespace renamed "alpha" -> "gamma"` {
		t.Fatalf("m1 rename snapshot = %+v, %q, %v", snap1, reason1, err)
	}
	// Cannot rename onto existing beta.
	if _, err := s.RenameNamespace(ctx, "gamma", "beta", "cli"); err == nil {
		t.Error("expected error renaming onto existing namespace")
	}

	// Merge gamma into beta records update history on moved rows, keeps the newer
	// destination handoff h2 active, and archives the older source handoff h1.
	tick()
	moved, err := s.MergeNamespace(ctx, "gamma", "beta", "bob")
	if err != nil || moved != 2 {
		t.Fatalf("MergeNamespace = %d, %v", moved, err)
	}
	if _, err := s.RequireNamespace(ctx, "gamma"); err == nil {
		t.Error("gamma should have been deleted after merge")
	}
	m1AfterMerge, err := s.Get(ctx, m1.ID)
	if err != nil || !m1AfterMerge.UpdatedAt.Equal(m1.UpdatedAt) || m1AfterMerge.UpdatedBy != "carol" {
		t.Fatalf("m1 after merge = %+v (want updated_at=%v, updated_by=carol), %v", m1AfterMerge, m1.UpdatedAt, err)
	}
	hist1, err = s.History(ctx, m1.ID)
	if err != nil || len(hist1) != 3 || hist1[2].Action != ActionUpdate || hist1[2].ChangedBy != "bob" {
		t.Fatalf("m1 history after merge = %+v, %v", hist1, err)
	}
	snap2, reason2, err := hist1[2].Decode()
	if err != nil || snap2.Namespace != "beta" || reason2 != `namespace merged "gamma" -> "beta"` {
		t.Fatalf("m1 merge snapshot = %+v, %q, %v", snap2, reason2, err)
	}
	handoffs, err := s.List(ctx, ListFilter{Namespace: "beta", Kinds: []Kind{KindHandoff}})
	if err != nil || len(handoffs) != 1 || handoffs[0].ID != h2.ID || handoffs[0].UpdatedBy != "dave" {
		t.Fatalf("active handoffs in beta = %+v (want #%d by dave), %v", handoffs, h2.ID, err)
	}
	h1After, err := s.Get(ctx, h1.ID)
	if err != nil || !h1After.Archived() {
		t.Fatalf("older handoff h1 should be archived after merge: %+v, %v", h1After, err)
	}
	h1Hist, err := s.History(ctx, h1.ID)
	if err != nil || len(h1Hist) != 4 {
		t.Fatalf("h1 history = %+v, %v", h1Hist, err)
	}

	// Archive all active memories in beta.
	archivedCount, err := s.ArchiveNamespace(ctx, "beta", "cli", "")
	if err != nil || archivedCount != 2 {
		t.Fatalf("ArchiveNamespace = %d, %v", archivedCount, err)
	}
	active, err := s.List(ctx, ListFilter{Namespace: "beta"})
	if err != nil || len(active) != 0 {
		t.Fatalf("active in beta after archive = %d, %v", len(active), err)
	}
}

func TestNamespaceMovesPreserveRecencyAndNewerSourceHandoff(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// f1 (created first) is updated after f2 so f1 is more recent despite lower id.
	f1, err := s.Create(ctx, NewMemory{Namespace: "src", Kind: KindFact, Content: "fact 1"})
	if err != nil {
		t.Fatal(err)
	}
	tick()
	f2, err := s.Create(ctx, NewMemory{Namespace: "src", Kind: KindFact, Content: "fact 2"})
	if err != nil {
		t.Fatal(err)
	}
	tick()
	updatedContent := "fact 1 edited"
	if _, err := s.Update(ctx, f1.ID, Patch{Content: &updatedContent}, "editor"); err != nil {
		t.Fatal(err)
	}

	// Rename must preserve recency order (f1 before f2) instead of collapsing updated_at.
	tick()
	if _, err := s.RenameNamespace(ctx, "src", "renamed", "cli"); err != nil {
		t.Fatal(err)
	}
	facts, err := s.List(ctx, ListFilter{Namespace: "renamed", Kinds: []Kind{KindFact}})
	if err != nil || len(facts) != 2 || facts[0].ID != f1.ID || facts[1].ID != f2.ID {
		t.Fatalf("facts order after rename = %+v (want #%d then #%d), %v", facts, f1.ID, f2.ID, err)
	}

	// Destination fact and older handoff, followed by a newer source handoff.
	tick()
	dstFact, err := s.Create(ctx, NewMemory{Namespace: "dst", Kind: KindFact, Content: "dst fact"})
	if err != nil {
		t.Fatal(err)
	}
	tick()
	dstHandoff, err := s.Create(ctx, NewMemory{Namespace: "dst", Kind: KindHandoff, Content: "dst handoff", Source: "dst-agent"})
	if err != nil {
		t.Fatal(err)
	}
	tick()
	srcHandoff, err := s.Create(ctx, NewMemory{Namespace: "renamed", Kind: KindHandoff, Content: "src handoff", Source: "src-agent"})
	if err != nil {
		t.Fatal(err)
	}

	tick()
	if _, err := s.MergeNamespace(ctx, "renamed", "dst", "cli"); err != nil {
		t.Fatal(err)
	}
	// Newer source handoff stays active with its original author; older destination handoff is archived.
	handoffs, err := s.List(ctx, ListFilter{Namespace: "dst", Kinds: []Kind{KindHandoff}})
	if err != nil || len(handoffs) != 1 || handoffs[0].ID != srcHandoff.ID || handoffs[0].UpdatedBy != "src-agent" {
		t.Fatalf("active handoff after merge = %+v (want #%d by src-agent), %v", handoffs, srcHandoff.ID, err)
	}
	dstHandoffAfter, err := s.Get(ctx, dstHandoff.ID)
	if err != nil || !dstHandoffAfter.Archived() {
		t.Fatalf("older destination handoff should be archived: %+v, %v", dstHandoffAfter, err)
	}
	// Destination fact remains ranked ahead of older moved facts.
	allFacts, err := s.List(ctx, ListFilter{Namespace: "dst", Kinds: []Kind{KindFact}})
	if err != nil || len(allFacts) != 3 || allFacts[0].ID != dstFact.ID || allFacts[1].ID != f1.ID || allFacts[2].ID != f2.ID {
		t.Fatalf("facts order after merge = %+v (want #%d, #%d, #%d), %v", allFacts, dstFact.ID, f1.ID, f2.ID, err)
	}
}
