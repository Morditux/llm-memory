package view

import (
	"context"
	"fmt"

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
	nsErr   *widget.Label

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
	ui := func(f func()) {
		fyne.Do(func() {
			f()
			if w.updated != nil {
				w.updated()
			}
		})
	}
	w.ctrl = core.NewController(b, core.Observer{
		Stats:      func(s store.Stats) { ui(func() { w.stats.SetText(core.StatsLine(s)) }) },
		Namespaces: func(ns []store.Namespace) { ui(func() { w.setNamespaces(ns) }) },
		Error:      func(err error) { ui(func() { w.nsErr.SetText(err.Error()) }) },
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

	w.nsErr = widget.NewLabel("")
	w.nsErr.Importance = widget.DangerImportance
	w.nsErr.Wrapping = fyne.TextWrapWord
	w.nsList = w.buildNamespaceList()
	left := container.NewBorder(
		widget.NewLabelWithStyle("Namespaces", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		w.nsErr, nil, nil, w.nsList)

	center := placeholder("Memories")
	right := placeholder("Select a memory")

	inner := container.NewHSplit(center, right)
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

// Reload fetches stats and namespaces in the background.
func (w *MainWindow) Reload() {
	go func() { _ = w.ctrl.Refresh(w.ctx) }()
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
		if i == 0 {
			w.ctrl.SelectNamespace("")
			return
		}
		w.ctrl.SelectNamespace(w.nsItems[i-1].Name)
	}
	return l
}

func (w *MainWindow) setNamespaces(ns []store.Namespace) {
	w.nsErr.SetText("")
	w.nsItems = ns
	w.nsList.Refresh()
}

func placeholder(text string) fyne.CanvasObject {
	l := widget.NewLabel(text)
	l.Importance = widget.LowImportance
	return container.NewCenter(l)
}
