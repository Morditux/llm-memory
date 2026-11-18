package view

import (
	"errors"
	"reflect"
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

// A mutation announces itself in the banner, then reloads stats, namespaces,
// the list and the detail: that is five UI updates.
const (
	mutationUpdates = 5
	archivedID      = 3
)

// confirmer replaces the confirmation dialogs of a window.
type confirmer struct {
	accept bool
	msgs   []string
}

func (c *confirmer) install(w *MainWindow) {
	w.confirm = func(_, msg string, onYes func()) {
		c.msgs = append(c.msgs, msg)
		if c.accept {
			onYes()
		}
	}
}

func mutFake() *core.FakeBackend {
	fb := newFake()
	fb.DetailValues[archivedID] = explorer.Detail{
		Memory: store.Memory{ID: archivedID, Namespace: "global", Kind: store.KindFact, Content: "old", ArchivedAt: new(time.Time)},
	}
	fb.CreateValue = store.Memory{ID: 2}
	fb.GCValue = 4
	return fb
}

// ready builds a window on fb and selects memory 1.
func ready(t *testing.T, a fyne.App, fb *core.FakeBackend) (*MainWindow, func(int), *confirmer) {
	t.Helper()
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	c := &confirmer{accept: true}
	c.install(w)
	selectCard(t, w, wait, 0)
	return w, wait, c
}

func texts(o fyne.CanvasObject) []string {
	switch o := o.(type) {
	case *widget.Label:
		return []string{o.Text}
	case *fyne.Container:
		var out []string
		for _, c := range o.Objects {
			out = append(out, texts(c)...)
		}
		return out
	}
	return nil
}

func lastCall(t *testing.T, fb *core.FakeBackend) core.MutationCall {
	t.Helper()
	calls := fb.MutationCalls()
	if len(calls) == 0 {
		t.Fatal("no mutation call")
	}
	return calls[len(calls)-1]
}

func TestCreateViaDialog(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	w.nsList.Select(2) // demo
	wait(1)

	test.Tap(w.newBtn)
	f := w.create
	if f == nil || !w.createOpen {
		t.Fatal("dialog not open")
	}
	if f.namespace.Text != "demo" || f.kind.Selected != "fact" || f.imp.Selected != "2" {
		t.Fatalf("defaults: ns = %q, kind = %q, imp = %q", f.namespace.Text, f.kind.Selected, f.imp.Selected)
	}
	if !slices.Equal(f.kind.Options, []string{"fact", "decision", "preference", "task_state", "note"}) ||
		!slices.Equal(f.imp.Options, []string{"1", "2", "3"}) || f.ttl.PlaceHolder != "Expires in (e.g. 24h, 7d)" {
		t.Fatal("unexpected options or placeholders")
	}
	f.namespace.SetText(" ")
	f.kind.SetSelected("decision")
	f.imp.SetSelected("3")
	f.title.SetText(" T ")
	f.tags.SetText("a, ,b")
	f.ttl.SetText("7d")
	f.content.SetText("hello\nworld")
	test.Tap(f.create)
	wait(mutationUpdates + 1) // + closing the dialog

	got := lastCall(t, fb)
	want := explorer.CreateRequest{
		Namespace: "global", Kind: store.KindDecision, Importance: 3, Title: "T",
		Tags: []string{"a", "b"}, ExpiresIn: "7d", Content: "hello\nworld",
	}
	if got.Method != "Create" || !equalCreate(got.Create, want) {
		t.Fatalf("call = %+v", got)
	}
	if w.createOpen || w.create != nil {
		t.Fatal("dialog still open")
	}
	if w.banner.Text != "Created memory #2" || w.banner.Importance != widget.SuccessImportance || !w.banner.Visible() {
		t.Fatalf("banner = %q (%v)", w.banner.Text, w.banner.Importance)
	}
	if w.title.Text != "Memory #2" || w.ctrl.SelectedID() != 2 {
		t.Fatalf("title = %q, selected = %d", w.title.Text, w.ctrl.SelectedID())
	}
}

func equalCreate(a, b explorer.CreateRequest) bool {
	return a.Namespace == b.Namespace && a.Kind == b.Kind && a.Importance == b.Importance &&
		a.Title == b.Title && slices.Equal(a.Tags, b.Tags) && a.ExpiresIn == b.ExpiresIn && a.Content == b.Content
}

func TestCreateDefaultsToGlobal(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, mutFake())
	wait(initialUpdates)
	w.openCreate()
	if w.create.namespace.Text != "global" {
		t.Fatalf("namespace = %q", w.create.namespace.Text)
	}
	w.openCreate() // already open: no second dialog
	w.create.closeBtn.OnTapped()
	if w.createOpen {
		t.Fatal("Close did not close the dialog")
	}
}

func TestCreateErrorKeepsDialogOpen(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	fb.Errs = map[string]error{"Create": errors.New("invalid duration \"soon\"")}
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	w.openCreate()
	f := w.create
	f.ttl.SetText("soon")
	test.Tap(f.create)
	wait(2) // banner + dialog
	if !w.createOpen || !f.errLabel.Visible() || f.errLabel.Text != `invalid duration "soon"` {
		t.Fatalf("open = %v, error = %q", w.createOpen, f.errLabel.Text)
	}
	if w.banner.Text != f.errLabel.Text || w.banner.Importance != widget.DangerImportance {
		t.Fatalf("banner = %q (%v)", w.banner.Text, w.banner.Importance)
	}
	if f.create.Disabled() {
		t.Fatal("Create must be usable again")
	}
	if fb.Calls("Stats") != 1 {
		t.Fatal("reload after a failed creation")
	}
}

func TestCreateShortcut(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait := build(t, a, mutFake())
	wait(initialUpdates)
	if !w.onShortcut(shortcutNew) {
		t.Fatal("Ctrl+N not handled")
	}
	if !w.createOpen {
		t.Fatal("Ctrl+N did not open the dialog")
	}
}

func TestEditAndSave(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	w, wait, _ := ready(t, a, fb)

	if w.actions.edit == nil || w.actions.similar == nil || w.actions.archive == nil || w.actions.restore != nil {
		t.Fatal("active memory must have Edit, Find Similar and Archive")
	}
	test.Tap(w.actions.edit)
	wait(1)
	e := w.edit
	if e == nil || fb.Calls("Detail") != 1 {
		t.Fatalf("edit form = %v, detail calls = %d", e, fb.Calls("Detail"))
	}
	if e.namespace.Text != "global" || e.kind.Selected != "fact" || e.imp.Selected != "3" ||
		e.title.Text != "First" || e.tags.Text != "alpha, beta" || e.ttl.PlaceHolder != "New TTL (e.g. 24h, 7d)" ||
		e.content.Text != "alpha\nbeta  gamma" {
		t.Fatalf("form not prefilled: %+v", e.values())
	}
	if e.clear != nil {
		t.Fatal("no expiration to clear")
	}
	e.title.SetText("Renamed")
	e.tags.SetText("x")
	e.imp.SetSelected("1")
	test.Tap(e.save)
	wait(mutationUpdates)

	c := lastCall(t, fb)
	u := c.Update
	if c.Method != "Update" || c.ID != 1 || *u.Namespace != "global" || *u.Kind != store.KindFact ||
		*u.Importance != 1 || *u.Title != "Renamed" || !slices.Equal(*u.Tags, []string{"x"}) ||
		*u.Content != "alpha\nbeta  gamma" || u.ExpiresIn != "" || u.ClearExpires {
		t.Fatalf("call = %+v", c)
	}
	if w.edit != nil || w.banner.Text != "Updated memory #1" {
		t.Fatalf("edit = %v, banner = %q", w.edit, w.banner.Text)
	}
}

func TestEditClearExpiration(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	d := fb.DetailValues[1]
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	d.Memory.ExpiresAt = &exp
	fb.DetailValues[1] = d
	w, wait, _ := ready(t, a, fb)
	test.Tap(w.actions.edit)
	wait(1)
	if w.edit.clear == nil || !strings.Contains(w.edit.clear.Text, "Clear expiration (2030-01-02T03:04:05Z)") {
		t.Fatalf("clear = %+v", w.edit.clear)
	}
	w.edit.clear.SetChecked(true)
	w.edit.ttl.SetText(" 24h ")
	test.Tap(w.edit.save)
	wait(mutationUpdates)
	u := lastCall(t, fb).Update
	if !u.ClearExpires || u.ExpiresIn != "24h" {
		t.Fatalf("update = %+v", u)
	}
}

func TestEditCancelAndKeys(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	w, wait, _ := ready(t, a, fb)
	canvas := w.Window.Canvas()

	test.Tap(w.actions.edit)
	wait(1)
	test.Tap(w.edit.cancel)
	wait(1)
	if w.edit != nil || len(fb.MutationCalls()) != 0 {
		t.Fatal("Cancel must leave edit mode without saving")
	}

	test.Tap(w.actions.edit)
	wait(1)
	canvas.OnTypedKey()(&fyne.KeyEvent{Name: fyne.KeyEscape})
	wait(1)
	if w.edit != nil {
		t.Fatal("Escape did not cancel")
	}

	test.Tap(w.actions.edit)
	wait(1)
	w.edit.title.TypedKey(&fyne.KeyEvent{Name: fyne.KeyEscape})
	wait(1)
	if w.edit != nil {
		t.Fatal("Escape in an entry did not cancel")
	}

	test.Tap(w.actions.edit)
	wait(1)
	w.edit.title.TypedShortcut(shortcutSave)
	wait(mutationUpdates)
	if lastCall(t, fb).Method != "Update" || w.edit != nil {
		t.Fatal("Ctrl+S in an entry did not save")
	}

	test.Tap(w.actions.edit)
	wait(1)
	w.onShortcut(shortcutSave)
	wait(mutationUpdates)
	if n := len(fb.MutationCalls()); n != 2 {
		t.Fatalf("mutation calls = %d", n)
	}
	// Outside edit mode Ctrl+S and Escape do nothing.
	w.onShortcut(shortcutSave)
	canvas.OnTypedKey()(&fyne.KeyEvent{Name: fyne.KeyEscape})
	if n := len(fb.MutationCalls()); n != 2 {
		t.Fatalf("mutation calls = %d", n)
	}
}

func TestArchiveAsksConfirmation(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	w, wait, c := ready(t, a, fb)

	c.accept = false
	test.Tap(w.actions.archive)
	if len(c.msgs) != 1 || !strings.Contains(c.msgs[0], "#1") || len(fb.MutationCalls()) != 0 {
		t.Fatalf("declined: msgs = %v, calls = %v", c.msgs, fb.MutationCalls())
	}

	c.accept = true
	test.Tap(w.actions.archive)
	wait(mutationUpdates)
	call := lastCall(t, fb)
	if call.Method != "Archive" || call.ID != 1 || call.Reason != "archived from desktop GUI" {
		t.Fatalf("call = %+v", call)
	}
	if w.banner.Text != "Archived memory #1" {
		t.Fatalf("banner = %q", w.banner.Text)
	}
}

func TestRestore(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	w, wait, _ := ready(t, a, fb)
	w.loadDetail(archivedID)
	wait(1)
	if w.actions.restore == nil || w.actions.edit != nil || w.actions.archive != nil || w.actions.similar != nil {
		t.Fatal("archived memory must only have Restore")
	}
	test.Tap(w.actions.restore)
	wait(mutationUpdates)
	if c := lastCall(t, fb); c.Method != "Restore" || c.ID != archivedID {
		t.Fatalf("call = %+v", c)
	}
	if w.banner.Text != "Restored memory #3" {
		t.Fatalf("banner = %q", w.banner.Text)
	}
}

func TestRunGC(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	w, wait := build(t, a, fb)
	wait(initialUpdates)
	c := &confirmer{}
	c.install(w)

	test.Tap(w.gcBtn)
	if len(c.msgs) != 1 || fb.Calls("GC") != 0 {
		t.Fatalf("declined: msgs = %v", c.msgs)
	}
	c.accept = true
	test.Tap(w.gcBtn)
	wait(4) // banner, stats, namespaces, list: nothing selected
	if fb.Calls("GC") != 1 || fb.Calls("Detail") != 0 {
		t.Fatalf("GC = %d, detail = %d", fb.Calls("GC"), fb.Calls("Detail"))
	}
	if w.banner.Text != "GC archived 4 expired memory(ies)" {
		t.Fatalf("banner = %q", w.banner.Text)
	}
}

func TestFindSimilarThenSupersede(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	fb.SimilarValue = []store.Similar{
		{ID: 2, Kind: store.KindNote, Title: "Two", Snippet: "second body", Similarity: 0.834},
		{ID: 9, Kind: store.KindFact, Snippet: "other", Similarity: 0.5},
	}
	entered, release := make(chan struct{}), make(chan struct{})
	fb.Hook = func(m string) {
		if m == "Similar" {
			close(entered)
			<-release
		}
	}
	w, wait, c := ready(t, a, fb)

	test.Tap(w.actions.similar)
	<-entered
	wait(1)
	if got := texts(w.simBox); !slices.Equal(got, []string{"Searching similar memories…"}) {
		t.Fatalf("searching: %q", got)
	}
	close(release)
	wait(1)
	got := texts(w.simBox)
	want := []string{"Similar in global (2)", "#2 · note · 83% match", "Two", "second body", "#9 · fact · 50% match", "other"}
	if !slices.Equal(got, want) {
		t.Fatalf("similar = %q, want %q", got, want)
	}
	if len(w.supersedeBtn) != 2 || w.supersedeBtn[0].Text != "Supersede #2" {
		t.Fatalf("supersede buttons = %d", len(w.supersedeBtn))
	}

	c.accept = false
	test.Tap(w.supersedeBtn[0])
	if len(c.msgs) != 1 || len(fb.MutationCalls()) != 0 {
		t.Fatalf("declined: msgs = %v", c.msgs)
	}
	c.accept = true
	test.Tap(w.supersedeBtn[0])
	wait(mutationUpdates)
	if cl := lastCall(t, fb); cl.Method != "Supersede" || cl.ID != 1 || cl.TargetID != 2 {
		t.Fatalf("call = %+v", cl)
	}
	if w.banner.Text != "Archived #2 (superseded by #1)" {
		t.Fatalf("banner = %q", w.banner.Text)
	}
	if len(w.simBox.Objects) != 0 {
		t.Fatal("similar section must be cleared by the reload")
	}
}

func TestFindSimilarEmpty(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait, _ := ready(t, a, mutFake())
	test.Tap(w.actions.similar)
	wait(2)
	if got := texts(w.simBox); !slices.Equal(got, []string{"No similar memories found in global."}) {
		t.Fatalf("similar = %q", got)
	}
}

func TestFindSimilarError(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	fb.Errs = map[string]error{"Similar": errors.New("no engine")}
	w, wait, _ := ready(t, a, fb)
	test.Tap(w.actions.similar)
	wait(3) // searching, cleared, banner
	if len(w.simBox.Objects) != 0 || w.banner.Text != "no engine" || w.banner.Importance != widget.DangerImportance {
		t.Fatalf("similar = %q, banner = %q", texts(w.simBox), w.banner.Text)
	}
}

func TestRemoveAndAddLink(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	d := fb.DetailValues[1]
	d.Links = append(d.Links, store.LinkView{Direction: "outgoing", Kind: store.LinkSupersedes, ID: 8, MemoryKind: store.KindFact})
	fb.DetailValues[1] = d
	w, wait, _ := ready(t, a, fb)

	// Only the outgoing relates_to link can be removed: not the incoming
	// depends_on one, nor the outgoing supersedes one.
	if len(w.unlinkBtns) != 1 || w.unlinkBtns[0].Text != "×" {
		t.Fatalf("unlink buttons = %d", len(w.unlinkBtns))
	}
	test.Tap(w.unlinkBtns[0])
	wait(1)
	want := core.MutationCall{Method: "RemoveLink", ID: 1, TargetID: 2, Kind: store.LinkRelatesTo}
	if c := lastCall(t, fb); !reflect.DeepEqual(c, want) {
		t.Fatalf("call = %+v", c)
	}
	if fb.Calls("Detail") != 2 || fb.Calls("List") != 1 || fb.Calls("Stats") != 1 {
		t.Fatalf("detail = %d, list = %d, stats = %d", fb.Calls("Detail"), fb.Calls("List"), fb.Calls("Stats"))
	}

	l := w.linkAdd
	if !slices.Equal(l.kind.Options, []string{"relates_to", "depends_on", "supersedes"}) || l.kind.Selected != "relates_to" ||
		l.target.PlaceHolder != "Target #id" || l.add.Text != "+ Link" {
		t.Fatal("unexpected link row")
	}
	for _, bad := range []string{"", "abc", "0", "-2"} {
		l.target.SetText(bad)
		test.Tap(l.add)
	}
	if n := len(fb.MutationCalls()); n != 1 {
		t.Fatalf("invalid targets triggered %d calls", n-1)
	}
	l.kind.SetSelected("depends_on")
	l.target.SetText(" 5 ")
	test.Tap(l.add)
	wait(1)
	want = core.MutationCall{Method: "AddLink", ID: 1, TargetID: 5, Kind: store.LinkDependsOn}
	if c := lastCall(t, fb); !reflect.DeepEqual(c, want) {
		t.Fatalf("call = %+v", c)
	}
	if fb.Calls("Detail") != 3 || fb.Calls("List") != 1 {
		t.Fatalf("detail = %d, list = %d", fb.Calls("Detail"), fb.Calls("List"))
	}
}

func TestMutationErrorShownAndPersistent(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	fb := mutFake()
	fb.Errs = map[string]error{"Archive": errors.New("database is locked")}
	w, wait, _ := ready(t, a, fb)
	scheduled := 0
	w.bannerAfter = func(time.Duration, func()) func() bool { scheduled++; return func() bool { return true } }

	test.Tap(w.actions.archive)
	wait(1)
	if w.banner.Text != "database is locked" || w.banner.Importance != widget.DangerImportance || !w.banner.Visible() {
		t.Fatalf("banner = %q", w.banner.Text)
	}
	if scheduled != 0 {
		t.Fatal("errors must not auto-hide")
	}
	if fb.Calls("Stats") != 1 {
		t.Fatal("no reload expected after an error")
	}
}

func TestSuccessBannerAutoHides(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait, _ := ready(t, a, mutFake())
	var delays []time.Duration
	var fire []func()
	stopped := 0
	w.bannerAfter = func(d time.Duration, f func()) func() bool {
		delays = append(delays, d)
		fire = append(fire, f)
		return func() bool { stopped++; return true }
	}

	test.Tap(w.actions.archive)
	wait(mutationUpdates)
	if !w.banner.Visible() || w.banner.Importance != widget.SuccessImportance || !slices.Equal(delays, []time.Duration{4 * time.Second}) {
		t.Fatalf("visible = %v, delays = %v", w.banner.Visible(), delays)
	}

	// A newer message cancels the pending hide and is not hidden by it.
	w.ui(func() { w.showBanner("second", false) })
	wait(1)
	if stopped != 1 || len(fire) != 2 {
		t.Fatalf("stopped = %d, timers = %d", stopped, len(fire))
	}
	fire[0]()
	wait(1)
	if !w.banner.Visible() || w.banner.Text != "second" {
		t.Fatal("stale timer hid the banner")
	}
	fire[1]()
	wait(1)
	if w.banner.Visible() || w.banner.Text != "" {
		t.Fatal("banner not hidden")
	}
}

func TestSuccessBannerSurvivesReloads(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w, wait, _ := ready(t, a, mutFake())
	test.Tap(w.actions.archive)
	wait(mutationUpdates)
	if !w.banner.Visible() {
		t.Fatal("success hidden by the reload")
	}
	// An error is cleared by the next successful load, unlike a success.
	w.ui(func() { w.showBanner("oops", true) })
	wait(1)
	w.loadList()
	wait(1)
	if w.banner.Visible() {
		t.Fatal("error not cleared by a successful load")
	}
}
