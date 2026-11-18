package view

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"github.com/Morditux/llm-memory/gui/internal/core"
	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

// initialUpdates is the number of UI updates of the startup load: stats,
// namespaces and the first page of the list.
const initialUpdates = 3

func newFake() *core.FakeBackend {
	return &core.FakeBackend{
		StatsValue: store.Stats{Active: 5, Archived: 2, SchemaVersion: 9},
		NamespacesValue: []store.Namespace{
			{Name: "global", ActiveCount: 3},
			{Name: "demo", ActiveCount: 2},
		},
		ListValue: []store.Memory{
			{ID: 1, Namespace: "global", Kind: store.KindFact, Importance: 3, Title: "First", Content: "alpha\nbeta  gamma", Tags: []string{"alpha", "beta"}, UpdatedBy: "ui"},
			{ID: 2, Namespace: "demo", Kind: store.KindNote, Importance: 2, Content: "second body", UpdatedBy: "ui"},
		},
		SearchValue: []store.SearchResult{{Memory: store.Memory{ID: 2, Namespace: "demo", Kind: store.KindNote, Content: "second body"}}},
		DetailValues: map[int64]explorer.Detail{
			1: {
				Memory: store.Memory{ID: 1, Namespace: "global", Kind: store.KindFact, Importance: 3, Title: "First", Content: "alpha\nbeta  gamma", Tags: []string{"alpha", "beta"}, UpdatedBy: "ui"},
				Links: []store.LinkView{
					{Direction: "outgoing", Kind: store.LinkRelatesTo, ID: 2, MemoryKind: store.KindNote, Title: "Two"},
					{Direction: "incoming", Kind: store.LinkDependsOn, ID: 3, MemoryKind: store.KindFact, Archived: true},
				},
				History: []store.HistoryEntry{
					{Action: store.ActionCreate, Snapshot: "one\ntwo", ChangedBy: "ui"},
					{Action: store.ActionUpdate, Snapshot: "one\nthree", ChangedBy: "gui"},
				},
			},
			2: {Memory: store.Memory{ID: 2, Namespace: "demo", Kind: store.KindNote, Content: "second body", UpdatedBy: "ui"}},
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
	wait(initialUpdates)

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
		t.Fatal("toolbar buttons should be disabled until the mutation phase")
	}
	if len(w.items) != 2 || w.empty.Visible() || w.pager.Visible() {
		t.Fatalf("items = %d, empty = %v, pager = %v", len(w.items), w.empty.Visible(), w.pager.Visible())
	}
	if w.title.Text != "Details" {
		t.Fatalf("title = %q", w.title.Text)
	}
}

func TestAllNamespacesHighlightedAtStart(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, newFake())
	wait(initialUpdates)
	fired := false
	w.nsList.OnSelected = func(widget.ListItemID) { fired = true }
	w.nsList.Select(0)
	if fired {
		t.Fatal("item 0 was not already selected")
	}
}

func TestSelectNamespaceUpdatesController(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, newFake())
	wait(initialUpdates)
	w.nsList.Select(2)
	wait(1)
	if got := w.Controller().Filters().Namespace; got != "demo" {
		t.Fatalf("namespace = %q", got)
	}
	w.nsList.Select(0)
	wait(1)
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
	wait(initialUpdates)
	if w.banner.Text == "" || !w.banner.Visible() {
		t.Fatal("error not displayed")
	}
}

// typeInto sets the entry text and then runs its change callback by hand.
// Entry.SetText keeps rendering after calling OnChanged, which would race
// in the test driver with the reload that callback starts in the background.
func typeInto(e *widget.Entry, text string) {
	fn := e.OnChanged
	e.OnChanged = nil
	e.SetText(text)
	e.OnChanged = fn
	fn(text)
}

// choose is typeInto for a Select.
func choose(s *widget.Select, option string) {
	fn := s.OnChanged
	s.OnChanged = nil
	s.SetSelected(option)
	s.OnChanged = fn
	fn(option)
}

func TestSelectNamespaceReloadsList(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	w.nsList.Select(2)
	wait(1)
	calls := fb.ListCalls()
	if last := calls[len(calls)-1]; last.Namespace != "demo" || last.Offset != 0 {
		t.Fatalf("last list call = %+v", last)
	}
}

func TestCardContent(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, newFake())
	wait(initialUpdates)
	card := newCard()
	bindCard(card, w.items[0])
	box := card.(*fyne.Container)
	if got := box.Objects[0].(*widget.RichText).String(); !strings.Contains(got, "#1") || !strings.Contains(got, "imp:3") {
		t.Fatalf("meta = %q", got)
	}
	if got := box.Objects[1].(*widget.Label).Text; got != "First" {
		t.Fatalf("title = %q", got)
	}
	if got := box.Objects[2].(*widget.Label).Text; got != "alpha beta gamma" {
		t.Fatalf("preview = %q", got)
	}
}

func TestSearchUpdatesList(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	typeInto(w.search, "second")
	wait(1)
	if len(w.items) != 1 || w.items[0].ID != 2 {
		t.Fatalf("items = %+v", w.items)
	}
	sq := fb.SearchCalls()
	if len(sq) != 1 || sq[0].Query != "second" {
		t.Fatalf("search calls = %+v", sq)
	}
	if w.pager.Visible() {
		t.Fatal("pager must be hidden while searching")
	}
}

func TestEmptyListMessage(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	fb.ListValue = nil
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	if !w.empty.Visible() || w.empty.Text != "No memories match the current filter." || w.list.Visible() {
		t.Fatal("empty message not shown")
	}
}

func TestKindAndArchivedFilters(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	choose(w.kind, "fact")
	wait(1)
	choose(w.arch, "All")
	wait(1)
	last := fb.ListCalls()[2]
	if !slices.Equal(last.Kinds, []store.Kind{store.KindFact}) || last.Archived != store.IncludeArchived {
		t.Fatalf("last list call = %+v", last)
	}
}

func selectCard(t *testing.T, w *MainWindow, wait func(int), i int) {
	t.Helper()
	w.list.Select(i)
	wait(1)
}

func TestSelectCardShowsDetail(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, newFake())
	wait(initialUpdates)
	selectCard(t, w, wait, 0)
	if w.title.Text != "Memory #1" {
		t.Fatalf("title = %q", w.title.Text)
	}
	if len(w.tagButtons) != 2 || w.tagButtons[0].Text != "#alpha" {
		t.Fatalf("tag buttons = %d", len(w.tagButtons))
	}
	if len(w.linkButtons) != 2 || w.linkButtons[0].Text != "#2" {
		t.Fatalf("link buttons = %d", len(w.linkButtons))
	}
	if w.diffToggle.Text != "Show Raw" || len(w.diffGrids) != 2 {
		t.Fatalf("toggle = %q, grids = %d", w.diffToggle.Text, len(w.diffGrids))
	}
	g := w.diffGrids[1]
	var rows []string
	for i := range g.Rows {
		rows = append(rows, g.RowText(i))
	}
	if want := []string{"  one", "- two", "+ three"}; !slices.Equal(rows, want) {
		t.Fatalf("diff rows = %q, want %q", rows, want)
	}
}

func TestTagClickFiltersList(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	selectCard(t, w, wait, 0)
	test.Tap(w.tagButtons[1])
	wait(1)
	if w.tags.Text != "beta" {
		t.Fatalf("tags entry = %q", w.tags.Text)
	}
	calls := fb.ListCalls()
	if last := calls[len(calls)-1]; !slices.Equal(last.Tags, []string{"beta"}) || last.Offset != 0 {
		t.Fatalf("last list call = %+v", last)
	}
	time.Sleep(2 * core.DebounceDelay)
	if n := len(fb.ListCalls()); n != len(calls) {
		t.Fatalf("tag click triggered a debounced reload (%d calls, want %d)", n, len(calls))
	}
}

func TestLinkJump(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, newFake())
	wait(initialUpdates)
	selectCard(t, w, wait, 0)
	test.Tap(w.linkButtons[0])
	wait(1)
	if w.title.Text != "Memory #2" {
		t.Fatalf("title = %q", w.title.Text)
	}
	if len(w.linkButtons) != 0 || len(w.diffGrids) != 0 {
		t.Fatal("detail of #2 has no links nor history")
	}
}

func TestDiffToggle(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	selectCard(t, w, wait, 0)
	test.Tap(w.diffToggle)
	wait(1)
	if w.diffToggle.Text != "Show Diff" || len(w.diffGrids) != 0 || len(w.rawLabels) != 2 {
		t.Fatalf("toggle = %q, grids = %d, raw = %d", w.diffToggle.Text, len(w.diffGrids), len(w.rawLabels))
	}
	if w.rawLabels[1].Text != "one\nthree" {
		t.Fatalf("raw = %q", w.rawLabels[1].Text)
	}
	if fb.Calls("Detail") != 1 {
		t.Fatal("toggle must not refetch")
	}
	test.Tap(w.diffToggle)
	wait(1)
	if w.diffToggle.Text != "Show Raw" || len(w.diffGrids) != 2 {
		t.Fatal("diff not restored")
	}
}

func TestPager(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	fb.ListFunc = func(f store.ListFilter) ([]store.Memory, error) {
		if f.Offset == 0 {
			ms := make([]store.Memory, core.PageSize)
			for i := range ms {
				ms[i].ID = int64(i + 1)
			}
			return ms, nil
		}
		return []store.Memory{{ID: 99}}, nil
	}
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	if !w.pager.Visible() || w.pageLbl.Text != "Page 1 (50 shown)" || !w.prevBtn.Disabled() || w.nextBtn.Disabled() {
		t.Fatalf("page 1: visible = %v, label = %q", w.pager.Visible(), w.pageLbl.Text)
	}
	test.Tap(w.nextBtn)
	wait(1)
	if got := fb.ListCalls()[1].Offset; got != core.PageSize {
		t.Fatalf("offset = %d", got)
	}
	if w.pageLbl.Text != "Page 2 (1 shown)" || w.prevBtn.Disabled() || !w.nextBtn.Disabled() {
		t.Fatalf("page 2: label = %q", w.pageLbl.Text)
	}
	test.Tap(w.prevBtn)
	wait(1)
	if w.pageLbl.Text != "Page 1 (50 shown)" {
		t.Fatalf("label = %q", w.pageLbl.Text)
	}
}

func TestTagsEntryFiltersList(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	typeInto(w.tags, " Alpha, beta ")
	wait(1)
	calls := fb.ListCalls()
	if last := calls[len(calls)-1]; !slices.Equal(last.Tags, []string{"alpha", "beta"}) {
		t.Fatalf("last list call = %+v", last)
	}
}
