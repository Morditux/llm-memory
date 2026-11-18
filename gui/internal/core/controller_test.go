package core

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Morditux/llm-memory/internal/store"
)

func TestStatsLine(t *testing.T) {
	got := StatsLine(store.Stats{Active: 12, Archived: 3, SchemaVersion: 7})
	if want := "12 active · 3 archived · schema v7"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRefreshNotifiesObserver(t *testing.T) {
	fb := &FakeBackend{
		StatsValue:      store.Stats{Active: 2, SchemaVersion: 5},
		NamespacesValue: []store.Namespace{{Name: "global", ActiveCount: 2}},
	}
	var stats store.Stats
	var ns []store.Namespace
	c := NewController(fb, Observer{
		Stats:      func(s store.Stats) { stats = s },
		Namespaces: func(n []store.Namespace) { ns = n },
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats.Active != 2 || len(ns) != 1 || ns[0].Name != "global" {
		t.Fatalf("stats = %+v, ns = %+v", stats, ns)
	}
}

func TestErrorReported(t *testing.T) {
	boom := errors.New("boom")
	var got error
	called := false
	c := NewController(&FakeBackend{Err: boom}, Observer{
		Stats: func(store.Stats) { called = true },
		Error: func(err error) { got = err },
	})
	if err := c.LoadStats(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(got, boom) || called {
		t.Fatalf("observer error = %v, stats called = %v", got, called)
	}
}

func TestStaleResponseDropped(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	n := 0
	fb := &FakeBackend{StatsValue: store.Stats{Active: 1}}
	fb.Hook = func(string) {
		n++
		if n == 1 {
			close(entered)
			<-release
		}
	}
	var seen []int
	c := NewController(fb, Observer{Stats: func(s store.Stats) { seen = append(seen, s.Active) }})

	done := make(chan struct{})
	go func() { _ = c.LoadStats(context.Background()); close(done) }()
	<-entered
	fb.StatsValue = store.Stats{Active: 2}
	_ = c.LoadStats(context.Background())
	close(release)
	<-done
	if !slices.Equal(seen, []int{2}) {
		t.Fatalf("seen = %v, want [2]", seen)
	}
}

func TestFilters(t *testing.T) {
	c := NewController(&FakeBackend{}, Observer{})
	c.SetOffset(100)
	c.SelectNamespace("demo")
	if f := c.Filters(); f.Namespace != "demo" || f.Offset != 0 {
		t.Fatalf("filters = %+v", f)
	}
	c.SetOffset(50)
	c.SetFilters("q", []string{"a"}, store.KindFact, store.ArchivedOnly)
	f := c.Filters()
	if f.Query != "q" || !slices.Equal(f.Tags, []string{"a"}) || f.Kind != store.KindFact || f.Archived != store.ArchivedOnly || f.Offset != 0 {
		t.Fatalf("filters = %+v", f)
	}
	f.Tags[0] = "mutated"
	if c.Filters().Tags[0] != "a" {
		t.Fatal("Filters must return a copy")
	}
	c.SetOffset(-5)
	if c.Filters().Offset != 0 {
		t.Fatal("negative offset not clamped")
	}
	c.Select(42)
	if c.SelectedID() != 42 {
		t.Fatal("selected id lost")
	}
}
