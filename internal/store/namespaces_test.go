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
