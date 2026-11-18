package view

import (
	"context"
	"fmt"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/Morditux/llm-memory/gui/internal/core"
	"github.com/Morditux/llm-memory/internal/store"
)

// WindowSize is the initial size of the main window.
var WindowSize = fyne.NewSize(1400, 850)

// AllNamespaces is the label of the entry that clears the namespace filter.
const AllNamespaces = "All namespaces"

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

	detail       *fyne.Container
	detailScroll *container.Scroll
	shownID      int64
	title        *widget.Label
	tagButtons   []*widget.Button
	linkButtons  []*widget.Button
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
	// uiMu serialises updates: the real driver runs them on one thread, but
	// the test driver runs fyne.Do inline in the calling goroutine.
	var uiMu sync.Mutex
	ui := func(f func()) {
		fyne.Do(func() {
			uiMu.Lock()
			defer uiMu.Unlock()
			f()
			if w.updated != nil {
				w.updated()
			}
		})
	}
	w.ctrl = core.NewController(b, core.Observer{
		Stats:      func(s store.Stats) { ui(func() { w.stats.SetText(core.StatsLine(s)) }) },
		Namespaces: func(ns []store.Namespace) { ui(func() { w.setNamespaces(ns) }) },
		List:       func(r core.ListResult) { ui(func() { w.setList(r) }) },
		Detail:     func(v core.DetailView) { ui(func() { w.setDetail(v) }) },
		Error:      func(err error) { ui(func() { w.showError(err) }) },
	})

	w.newBtn = widget.NewButton("+ New", nil)
	w.newBtn.Importance = widget.HighImportance
	w.newBtn.Disable()
	w.gcBtn = widget.NewButton("Run GC", nil)
	w.gcBtn.Disable()
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

func (w *MainWindow) showError(err error) {
	w.banner.SetText(err.Error())
	w.banner.Show()
}

func (w *MainWindow) clearError() {
	w.banner.SetText("")
	w.banner.Hide()
}
