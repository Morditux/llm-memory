package view

import (
	"context"
	"fmt"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"

	"github.com/Morditux/llm-memory/gui/internal/core"
	"github.com/Morditux/llm-memory/internal/store"
)

// WindowSize is the initial size of the main window.
var WindowSize = fyne.NewSize(1400, 850)

// AllNamespaces is the label of the entry that clears the namespace filter.
const AllNamespaces = "All namespaces"

// BannerDelay is how long a success message stays in the status banner.
const BannerDelay = 4 * time.Second

// MainWindow is the explorer window.
type MainWindow struct {
	Window fyne.Window

	ctx  context.Context
	ctrl *core.Controller

	newBtn  *widget.Button
	gcBtn   *widget.Button
	stats   *widget.Label
	nsList  *widget.List
	nsItems []store.Namespace

	banner  *widget.Label
	search  *widget.Entry
	tags    *widget.Entry
	kind    *widget.Select
	arch    *widget.Select
	list    *widget.List
	items   []store.Memory
	empty   *widget.Label
	pager   *fyne.Container
	prevBtn *widget.Button
	nextBtn *widget.Button
	pageLbl *widget.Label

	// bannerIsErr tells whether the banner shows an error; errors stay until
	// the next successful load, successes hide after BannerDelay. bannerGen
	// invalidates a hide timer when a newer message replaces its message.
	bannerIsErr bool
	bannerGen   uint64
	bannerStop  func() bool
	bannerAfter core.AfterFunc
	// confirm asks the user to confirm an action and calls onYes if they do.
	// Tests replace it.
	confirm func(title, msg string, onYes func())
	// createOpen is set while the creation dialog is shown.
	createOpen bool
	create     *createForm

	// ui runs f on the UI thread, serialised, then calls updated.
	ui func(f func())

	detail       *fyne.Container
	detailScroll *container.Scroll
	shownID      int64
	title        *widget.Label
	tagButtons   []*widget.Button
	linkButtons  []*widget.Button
	actions      detailActions
	edit         *editForm
	simBox       *fyne.Container
	supersedeBtn []*widget.Button
	unlinkBtns   []*widget.Button
	linkAdd      linkAddRow
	diffToggle   *widget.Button
	diffGrids    []*widget.TextGrid
	rawLabels    []*widget.Label

	// syncing is set while the views are changed programmatically so that
	// their change callbacks do not act as user input. UI thread only.
	syncing bool

	// updated, when set, is called on the UI thread after each data update.
	updated func()
}

// NewMainWindow builds the explorer window on app, reading from b, and starts
// loading data in the background. Cancelling ctx abandons in-flight requests.
func NewMainWindow(ctx context.Context, app fyne.App, b core.Backend) *MainWindow {
	return newMainWindow(ctx, app, b, nil)
}

func newMainWindow(ctx context.Context, app fyne.App, b core.Backend, updated func()) *MainWindow {
	app.Settings().SetTheme(Theme{})
	w := &MainWindow{ctx: ctx, Window: app.NewWindow("llm-memory"), updated: updated}
	w.bannerAfter = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	w.confirm = w.confirmDialog
	// uiMu serialises updates: the real driver runs them on one thread, but
	// the test driver runs fyne.Do inline in the calling goroutine.
	var uiMu sync.Mutex
	w.ui = func(f func()) {
		fyne.Do(func() {
			uiMu.Lock()
			defer uiMu.Unlock()
			f()
			if w.updated != nil {
				w.updated()
			}
		})
	}
	ui := w.ui
	w.ctrl = core.NewController(b, core.Observer{
		Stats:      func(s store.Stats) { ui(func() { w.stats.SetText(core.StatsLine(s)) }) },
		Namespaces: func(ns []store.Namespace) { ui(func() { w.setNamespaces(ns) }) },
		List:       func(r core.ListResult) { ui(func() { w.setList(r) }) },
		Detail:     func(v core.DetailView) { ui(func() { w.setDetail(v) }) },
		Similar:    func(v core.SimilarView) { ui(func() { w.setSimilar(v) }) },
		Banner:     func(msg string, isErr bool) { ui(func() { w.showBanner(msg, isErr) }) },
		Error:      func(err error) { ui(func() { w.showBanner(err.Error(), true) }) },
	})

	w.newBtn = widget.NewButton("+ New", w.openCreate)
	w.newBtn.Importance = widget.HighImportance
	w.gcBtn = widget.NewButton("Run GC", w.runGC)
	w.stats = widget.NewLabel("")
	w.stats.Importance = widget.LowImportance
	title := widget.NewLabelWithStyle("llm-memory", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	toolbar := container.NewBorder(nil, nil,
		container.NewHBox(title, w.newBtn, w.gcBtn), w.stats)

	w.nsList = w.buildNamespaceList()
	left := container.NewBorder(
		widget.NewLabelWithStyle("Namespaces", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		nil, nil, nil, w.nsList)

	inner := container.NewHSplit(w.buildCenter(), w.buildDetail())
	inner.SetOffset(0.55)
	root := container.NewHSplit(left, inner)
	root.SetOffset(0.18)
	sep := canvas.NewRectangle(colorBorder)
	sep.SetMinSize(fyne.NewSize(0, 1))
	w.Window.SetContent(container.NewBorder(container.NewVBox(toolbar, sep), nil, nil, nil, root))
	w.Window.Resize(WindowSize)
	w.bindShortcuts()

	w.Reload()
	return w
}

// Reload fetches stats, namespaces and the memory list in the background.
func (w *MainWindow) Reload() {
	go func() { _ = w.ctrl.Refresh(w.ctx) }()
	w.loadList()
}

func (w *MainWindow) loadList() {
	go func() { _ = w.ctrl.LoadList(w.ctx) }()
}

func (w *MainWindow) loadDetail(id int64) {
	go func() { _ = w.ctrl.LoadDetail(w.ctx, id) }()
}

// Controller returns the window controller.
func (w *MainWindow) Controller() *core.Controller { return w.ctrl }

func (w *MainWindow) buildNamespaceList() *widget.List {
	l := widget.NewList(
		func() int { return len(w.nsItems) + 1 },
		func() fyne.CanvasObject {
			name := widget.NewLabel("namespace")
			name.Truncation = fyne.TextTruncateEllipsis
			count := widget.NewLabel("0")
			count.Importance = widget.LowImportance
			return container.NewBorder(nil, nil, nil, count, name)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			row := o.(*fyne.Container)
			name := row.Objects[0].(*widget.Label)
			count := row.Objects[1].(*widget.Label)
			if i == 0 {
				name.SetText(AllNamespaces)
				total := 0
				for _, n := range w.nsItems {
					total += n.ActiveCount
				}
				count.SetText(fmt.Sprint(total))
				return
			}
			n := w.nsItems[i-1]
			name.SetText(n.Name)
			count.SetText(fmt.Sprint(n.ActiveCount))
		},
	)
	l.OnSelected = func(i widget.ListItemID) {
		if w.syncing {
			return
		}
		if i == 0 {
			w.ctrl.SelectNamespace("")
		} else {
			w.ctrl.SelectNamespace(w.nsItems[i-1].Name)
		}
		w.loadList()
	}
	return l
}

func (w *MainWindow) setNamespaces(ns []store.Namespace) {
	w.clearError()
	w.nsItems = ns
	w.nsList.Refresh()
	sel := 0
	cur := w.ctrl.Filters().Namespace
	for i, n := range ns {
		if n.Name == cur {
			sel = i + 1
		}
	}
	w.syncing = true
	w.nsList.Select(sel)
	w.syncing = false
}

// showBanner shows a status message: errors in red until the next successful
// load, successes in green until BannerDelay has elapsed.
func (w *MainWindow) showBanner(msg string, isErr bool) {
	if w.bannerStop != nil {
		w.bannerStop()
		w.bannerStop = nil
	}
	w.bannerGen++
	gen := w.bannerGen
	w.bannerIsErr = isErr
	if isErr {
		w.banner.Importance = widget.DangerImportance
	} else {
		w.banner.Importance = widget.SuccessImportance
		w.bannerStop = w.bannerAfter(BannerDelay, func() {
			w.ui(func() {
				if w.bannerGen == gen {
					w.hideBanner()
				}
			})
		})
	}
	w.banner.SetText(msg)
	w.banner.Show()
}

// clearError hides the banner if it shows an error: a successful load makes
// the error obsolete, whereas a success message hides on its own.
func (w *MainWindow) clearError() {
	if w.bannerIsErr {
		w.hideBanner()
	}
}

func (w *MainWindow) hideBanner() {
	w.bannerIsErr = false
	w.banner.SetText("")
	w.banner.Hide()
}

func (w *MainWindow) confirmDialog(title, msg string, onYes func()) {
	dialog.NewConfirm(title, msg, func(ok bool) {
		if ok {
			onYes()
		}
	}, w.Window).Show()
}

func (w *MainWindow) runGC() {
	w.confirm("Run GC", "Archive every expired memory?", func() {
		go func() { _ = w.ctrl.GC(w.ctx) }()
	})
}

// shortcuts of the window.
var (
	shortcutNew  = &desktop.CustomShortcut{KeyName: fyne.KeyN, Modifier: fyne.KeyModifierShortcutDefault}
	shortcutSave = &desktop.CustomShortcut{KeyName: fyne.KeyS, Modifier: fyne.KeyModifierShortcutDefault}
)

func (w *MainWindow) bindShortcuts() {
	c := w.Window.Canvas()
	c.AddShortcut(shortcutNew, func(s fyne.Shortcut) { w.onShortcut(s) })
	c.AddShortcut(shortcutSave, func(s fyne.Shortcut) { w.onShortcut(s) })
	c.SetOnTypedKey(func(e *fyne.KeyEvent) {
		if e.Name == fyne.KeyEscape {
			w.cancelEdit()
		}
	})
}

// onShortcut runs the action of a window shortcut and reports whether s is one.
func (w *MainWindow) onShortcut(s fyne.Shortcut) bool {
	switch s.ShortcutName() {
	case shortcutNew.ShortcutName():
		w.openCreate()
	case shortcutSave.ShortcutName():
		w.saveEdit()
	default:
		return false
	}
	return true
}
