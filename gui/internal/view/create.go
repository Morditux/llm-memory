package view

import (
	"strconv"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"github.com/Morditux/llm-memory/gui/internal/core"
)

// memoryKinds are the kinds a memory can be created or edited with.
var memoryKinds = []string{"fact", "decision", "preference", "task_state", "note"}

// importances are the choices of the importance selectors.
var importances = []string{"1", "2", "3"}

const defaultImportance = "2"

// createForm is the content of the "New memory" dialog.
type createForm struct {
	namespace *widget.Entry
	kind      *widget.Select
	imp       *widget.Select
	title     *widget.Entry
	tags      *widget.Entry
	ttl       *widget.Entry
	content   *widget.Entry
	errLabel  *widget.Label
	create    *widget.Button
	closeBtn  *widget.Button
	dlg       dialog.Dialog
}

// values reads the form fields.
func (f *createForm) values() core.CreateForm {
	imp, _ := strconv.Atoi(f.imp.Selected)
	return core.CreateForm{
		Namespace:  f.namespace.Text,
		Kind:       f.kind.Selected,
		Importance: imp,
		Title:      f.title.Text,
		Tags:       f.tags.Text,
		ExpiresIn:  f.ttl.Text,
		Content:    f.content.Text,
	}
}

// openCreate shows the creation dialog, pre-filled with the current namespace
// or "global". It does nothing if the dialog is already open.
func (w *MainWindow) openCreate() {
	if w.createOpen {
		return
	}
	ns := w.ctrl.Filters().Namespace
	if ns == "" {
		ns = core.DefaultNamespace
	}
	f := &createForm{
		namespace: widget.NewEntry(),
		kind:      widget.NewSelect(memoryKinds, nil),
		imp:       widget.NewSelect(importances, nil),
		title:     widget.NewEntry(),
		tags:      widget.NewEntry(),
		ttl:       widget.NewEntry(),
		content:   widget.NewMultiLineEntry(),
		errLabel:  widget.NewLabel(""),
	}
	f.namespace.SetText(ns)
	f.kind.SetSelected(memoryKinds[0])
	f.imp.SetSelected(defaultImportance)
	f.title.SetPlaceHolder("Title (optional)")
	f.tags.SetPlaceHolder("Tags comma-separated (optional)")
	f.ttl.SetPlaceHolder("Expires in (e.g. 24h, 7d)")
	f.content.SetPlaceHolder("Memory content…")
	f.content.Wrapping = fyne.TextWrapWord
	f.content.SetMinRowsVisible(8)
	f.errLabel.Importance = widget.DangerImportance
	f.errLabel.Wrapping = fyne.TextWrapWord
	f.errLabel.Hide()

	f.create = widget.NewButton("Create", func() { w.submitCreate(f) })
	f.create.Importance = widget.HighImportance
	f.closeBtn = widget.NewButton("Close", func() { f.dlg.Hide() })

	form := widget.NewForm(
		widget.NewFormItem("Namespace", f.namespace),
		widget.NewFormItem("Kind", f.kind),
		widget.NewFormItem("Importance", f.imp),
		widget.NewFormItem("Title", f.title),
		widget.NewFormItem("Tags", f.tags),
		widget.NewFormItem("Expires in", f.ttl),
		widget.NewFormItem("Content", f.content),
	)
	body := container.NewVBox(form, f.errLabel,
		container.NewHBox(layout.NewSpacer(), f.closeBtn, f.create))
	d := dialog.NewCustomWithoutButtons("New memory", body, w.Window)
	d.SetOnClosed(func() { w.createOpen, w.create = false, nil })
	f.dlg = d
	w.createOpen, w.create = true, f
	d.Resize(fyne.NewSize(560, d.MinSize().Height))
	d.Show()
}

// submitCreate creates the memory. The dialog stays open, with the error
// displayed in it, when the creation fails.
func (w *MainWindow) submitCreate(f *createForm) {
	req := f.values().Request()
	f.create.Disable()
	go func() {
		_, err := w.ctrl.Create(w.ctx, req)
		w.ui(func() {
			f.create.Enable()
			if err != nil {
				f.errLabel.SetText(err.Error())
				f.errLabel.Show()
				return
			}
			f.dlg.Hide()
		})
	}()
}
