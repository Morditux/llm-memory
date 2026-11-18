package view

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"github.com/Morditux/llm-memory/gui/internal/core"
	"github.com/Morditux/llm-memory/internal/store"
)

// nsReady builds a window with memory 1 displayed and the namespace at list
// row (1 is "global", 2 is "demo") selected.
func nsReady(t *testing.T, a fyne.App, row int, fb *core.FakeBackend) (*MainWindow, func(int), *confirmer) {
	t.Helper()
	fb.NamespacesValue[1].Description = "Demo things"
	w, wait, c := ready(t, a, fb)
	w.nsList.Select(row)
	wait(1)
	return w, wait, c
}

func TestNamespacePanelHiddenForAll(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, newFake())
	wait(initialUpdates)
	if w.nsPanel != nil || w.nsPanelBox.Visible() {
		t.Fatal("panel shown for All namespaces")
	}
	w.nsList.Select(2)
	wait(1)
	if w.nsPanel == nil || !w.nsPanelBox.Visible() {
		t.Fatal("panel hidden for a namespace")
	}
	w.nsList.Select(0)
	wait(1)
	if w.nsPanel != nil || w.nsPanelBox.Visible() {
		t.Fatal("panel kept after going back to All namespaces")
	}
}

func TestNamespacePanelGlobalOnlyDescription(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	fb.NamespacesValue[0].Description = "Shared"
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	w.nsList.Select(1)
	wait(1)
	p := w.nsPanel
	if p == nil || p.name.Text != "global" || p.desc.Text != "Shared" || p.desc.PlaceHolder != "Description" ||
		p.save.Text != "Save description" {
		t.Fatalf("panel = %+v", p)
	}
	if p.rename != nil || p.merge != nil || p.archive != nil || p.renameTo != nil || p.mergeTo != nil {
		t.Fatal("global must only expose its description")
	}
}

func TestNamespacePanelLayout(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait, _ := ready(t, a, newFake())
	w.nsList.Select(2)
	wait(1)
	p := w.nsPanel
	if p.name.Text != "demo" || !p.name.TextStyle.Bold || p.renameTo.PlaceHolder != "New name" ||
		p.rename.Text != "Rename" || p.mergeTo.PlaceHolder != "Merge into" || p.merge.Text != "Merge" ||
		p.archive.Text != "Archive all in demo" || p.archive.Importance != widget.DangerImportance {
		t.Fatalf("panel = %+v", p)
	}
	if !slices.Equal(p.mergeOptions, []string{"global"}) {
		t.Fatalf("merge options = %v", p.mergeOptions)
	}
}

func TestSaveDescription(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	w, wait, _ := nsReady(t, a, 2, fb)
	if w.nsPanel.desc.Text != "Demo things" {
		t.Fatalf("description = %q", w.nsPanel.desc.Text)
	}
	w.nsPanel.desc.SetText("Updated")
	fb.NamespacesValue[1].Description = "Updated"
	test.Tap(w.nsPanel.save)
	wait(2) // banner + namespaces

	want := core.MutationCall{Method: "SetNamespaceDescription", Name: "demo", Description: "Updated"}
	if got := lastCall(t, fb); !reflect.DeepEqual(got, want) {
		t.Fatalf("call = %+v", got)
	}
	if w.banner.Text != `Updated description for "demo"` || w.banner.Importance != widget.SuccessImportance {
		t.Fatalf("banner = %q", w.banner.Text)
	}
	if fb.Calls("Stats") != 1 || fb.Calls("List") != 2 {
		t.Fatalf("description must only reload the namespaces: stats = %d, list = %d", fb.Calls("Stats"), fb.Calls("List"))
	}
	if w.nsPanel.desc.Text != "Updated" {
		t.Fatalf("panel description = %q", w.nsPanel.desc.Text)
	}
}

func TestRenameFollowsSelection(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	w, wait, _ := nsReady(t, a, 2, fb)
	fb.RenameValue = "zzz"
	fb.NamespacesValue = []store.Namespace{{Name: "global", ActiveCount: 3}, {Name: "zzz", ActiveCount: 2}}

	w.nsPanel.renameTo.SetText(" ZZZ ")
	test.Tap(w.nsPanel.rename)
	wait(mutationUpdates)

	want := core.MutationCall{Method: "RenameNamespace", Name: "demo", NewName: " ZZZ "}
	if got := lastCall(t, fb); !reflect.DeepEqual(got, want) {
		t.Fatalf("call = %+v", got)
	}
	if w.banner.Text != `Renamed namespace to "zzz"` {
		t.Fatalf("banner = %q", w.banner.Text)
	}
	if w.ctrl.Filters().Namespace != "zzz" || w.nsRow != 2 || w.nsItems[w.nsRow-1].Name != "zzz" {
		t.Fatalf("namespace = %q, row = %d", w.ctrl.Filters().Namespace, w.nsRow)
	}
	if w.nsPanel == nil || w.nsPanel.name.Text != "zzz" || w.nsPanel.archive.Text != "Archive all in zzz" {
		t.Fatalf("panel = %+v", w.nsPanel)
	}
	if got := fb.ListCalls(); got[len(got)-1].Namespace != "zzz" || got[len(got)-1].Offset != 0 {
		t.Fatalf("last list = %+v", got[len(got)-1])
	}
}

func TestRenameMovesHighlight(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	w, wait, _ := nsReady(t, a, 2, fb)
	// "abc" sorts before "global": the highlighted row moves up.
	fb.RenameValue = "abc"
	fb.NamespacesValue = []store.Namespace{{Name: "abc", ActiveCount: 2}, {Name: "global", ActiveCount: 3}}
	w.nsPanel.renameTo.SetText("abc")
	test.Tap(w.nsPanel.rename)
	wait(mutationUpdates)
	if w.nsRow != 1 || w.nsPanel == nil || w.nsPanel.name.Text != "abc" {
		t.Fatalf("row = %d, panel = %+v", w.nsRow, w.nsPanel)
	}
}

func TestMergeAsksConfirmation(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	fb.MergeValue = 2
	w, wait, c := nsReady(t, a, 2, fb)
	fb.NamespacesValue = []store.Namespace{{Name: "global", ActiveCount: 5}}

	w.nsPanel.mergeTo.SetText("  global ")
	c.accept = false
	test.Tap(w.nsPanel.merge)
	if len(c.msgs) != 1 || !strings.Contains(c.msgs[0], `"demo"`) || !strings.Contains(c.msgs[0], `"global"`) ||
		len(fb.MutationCalls()) != 0 {
		t.Fatalf("declined: msgs = %v, calls = %v", c.msgs, fb.MutationCalls())
	}

	c.accept = true
	test.Tap(w.nsPanel.merge)
	wait(mutationUpdates)
	want := core.MutationCall{Method: "MergeNamespace", Name: "demo", NewName: "global"}
	if got := lastCall(t, fb); !reflect.DeepEqual(got, want) {
		t.Fatalf("call = %+v", got)
	}
	if w.banner.Text != `Merged "demo" into "global" (2 moved)` {
		t.Fatalf("banner = %q", w.banner.Text)
	}
	if w.ctrl.Filters().Namespace != "global" || w.nsRow != 1 {
		t.Fatalf("namespace = %q, row = %d", w.ctrl.Filters().Namespace, w.nsRow)
	}
	if w.nsPanel == nil || w.nsPanel.rename != nil {
		t.Fatal("panel must now be the global one")
	}
}

func TestMergeBlankTargetReportsError(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	fb.Errs = map[string]error{"MergeNamespace": errors.New("namespace is required")}
	w, wait, c := nsReady(t, a, 2, fb)
	test.Tap(w.nsPanel.merge)
	wait(1)
	if len(c.msgs) != 0 {
		t.Fatalf("confirmation asked for a blank target: %v", c.msgs)
	}
	if w.banner.Text != "namespace is required" || w.banner.Importance != widget.DangerImportance {
		t.Fatalf("banner = %q", w.banner.Text)
	}
	if w.ctrl.Filters().Namespace != "demo" || w.nsRow != 2 {
		t.Fatal("selection changed after a failed merge")
	}
}

func TestArchiveNamespaceAsksConfirmation(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	fb.ArchiveNamespaceValue = 2
	w, wait, c := nsReady(t, a, 2, fb)

	c.accept = false
	test.Tap(w.nsPanel.archive)
	if len(c.msgs) != 1 || !strings.Contains(c.msgs[0], `"demo"`) || len(fb.MutationCalls()) != 0 {
		t.Fatalf("declined: msgs = %v, calls = %v", c.msgs, fb.MutationCalls())
	}

	c.accept = true
	test.Tap(w.nsPanel.archive)
	wait(mutationUpdates)
	want := core.MutationCall{Method: "ArchiveNamespace", Name: "demo", Reason: "archived from desktop GUI"}
	if got := lastCall(t, fb); !reflect.DeepEqual(got, want) {
		t.Fatalf("call = %+v", got)
	}
	if w.banner.Text != `Archived 2 memory(ies) in "demo"` || w.ctrl.Filters().Namespace != "demo" {
		t.Fatalf("banner = %q, namespace = %q", w.banner.Text, w.ctrl.Filters().Namespace)
	}
}

func TestNamespaceErrorInBanner(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := newFake()
	fb.Errs = map[string]error{"RenameNamespace": errors.New(`namespace "x" already exists (use merge instead)`)}
	w, wait, _ := nsReady(t, a, 2, fb)
	w.nsPanel.renameTo.SetText("x")
	test.Tap(w.nsPanel.rename)
	wait(1)
	if w.banner.Text != `namespace "x" already exists (use merge instead)` ||
		w.banner.Importance != widget.DangerImportance || !w.banner.Visible() {
		t.Fatalf("banner = %q", w.banner.Text)
	}
	if w.ctrl.Filters().Namespace != "demo" || w.nsRow != 2 {
		t.Fatal("selection changed after a failed rename")
	}
}
