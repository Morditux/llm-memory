package view

import (
	"context"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"

	"github.com/Morditux/llm-memory/gui/internal/core"
	"github.com/Morditux/llm-memory/internal/store"
)

func newFake() *core.FakeBackend {
	return &core.FakeBackend{
		StatsValue: store.Stats{Active: 5, Archived: 2, SchemaVersion: 9},
		NamespacesValue: []store.Namespace{
			{Name: "global", ActiveCount: 3},
			{Name: "demo", ActiveCount: 2},
		},
	}
}

// build creates the window and returns a function waiting for n UI updates.
func build(t *testing.T, a fyne.App, b core.Backend) (*MainWindow, func(n int)) {
	t.Helper()
	ch := make(chan struct{}, 8)
	w := newMainWindow(context.Background(), a, b, func() { ch <- struct{}{} })
	return w, func(n int) {
		t.Helper()
		for range n {
			select {
			case <-ch:
			case <-time.After(3 * time.Second):
				t.Fatal("timeout waiting for UI update")
			}
		}
	}
}

func TestMainWindowPopulates(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, newFake())
	wait(2)

	if w.Window.Title() != "llm-memory" {
		t.Fatalf("title = %q", w.Window.Title())
	}
	if got, want := w.stats.Text, "5 active · 2 archived · schema v9"; got != want {
		t.Fatalf("stats = %q, want %q", got, want)
	}
	if w.nsList.Length() != 3 {
		t.Fatalf("list length = %d, want 3 (All + 2)", w.nsList.Length())
	}
	if !w.newBtn.Disabled() || !w.gcBtn.Disabled() {
		t.Fatal("toolbar buttons should be disabled in phase 1")
	}
}

func TestSelectNamespaceUpdatesController(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, newFake())
	wait(2)
	w.nsList.Select(2)
	if got := w.Controller().Filters().Namespace; got != "demo" {
		t.Fatalf("namespace = %q", got)
	}
	w.nsList.Select(0)
	if got := w.Controller().Filters().Namespace; got != "" {
		t.Fatalf("namespace = %q, want all", got)
	}
}

func TestErrorShown(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	fb.Err = context.DeadlineExceeded
	w, wait := build(t, a, fb)
	wait(2)
	if w.nsErr.Text == "" {
		t.Fatal("error not displayed")
	}
}
