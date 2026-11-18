package view

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/Morditux/llm-memory/internal/store"
)

// nsPanel is the management panel of the selected namespace, under the
// namespace list. Its widgets other than name, desc and save are nil for the
// global namespace, which can only change its description.
type nsPanel struct {
	box      *fyne.Container
	name     *widget.Label
	desc     *widget.Entry
	save     *widget.Button
	renameTo *widget.Entry
	rename   *widget.Button
	mergeTo  *widget.SelectEntry
	// mergeOptions are the suggestions of mergeTo: the other namespaces.
	mergeOptions []string
	merge        *widget.Button
	archive      *widget.Button
}

// updateNamespacePanel rebuilds the panel for the selected namespace. It is
// empty and hidden for "All namespaces" and for a namespace that is not in the
// loaded list.
func (w *MainWindow) updateNamespacePanel() {
	w.nsPanel = nil
	w.nsPanelBox.Objects = nil
	defer w.nsPanelBox.Refresh()

	cur := w.ctrl.Filters().Namespace
	var ns *store.Namespace
	for i := range w.nsItems {
		if w.nsItems[i].Name == cur {
			ns = &w.nsItems[i]
		}
	}
	if cur == "" || ns == nil {
		w.nsPanelBox.Hide()
		return
	}
	p := &nsPanel{name: heading(ns.Name), desc: widget.NewEntry()}
	p.name.Truncation = fyne.TextTruncateEllipsis
	p.desc.SetPlaceHolder("Description")
	p.desc.SetText(ns.Description)
	p.save = widget.NewButton("Save description", func() {
		name, desc := ns.Name, p.desc.Text
		go func() { _ = w.ctrl.SetNamespaceDescription(w.ctx, name, desc) }()
	})
	p.save.Importance = widget.HighImportance
	objs := []fyne.CanvasObject{widget.NewSeparator(), p.name, p.desc, p.save}

	if ns.Name != store.DefaultNamespace {
		name := ns.Name
		p.renameTo = widget.NewEntry()
		p.renameTo.SetPlaceHolder("New name")
		p.rename = widget.NewButton("Rename", func() {
			to := p.renameTo.Text
			go func() { _ = w.ctrl.RenameNamespace(w.ctx, name, to) }()
		})

		for _, n := range w.nsItems {
			if n.Name != name {
				p.mergeOptions = append(p.mergeOptions, n.Name)
			}
		}
		p.mergeTo = widget.NewSelectEntry(p.mergeOptions)
		p.mergeTo.SetPlaceHolder("Merge into")
		p.merge = widget.NewButton("Merge", func() { w.mergeNamespace(name, p.mergeTo.Text) })

		p.archive = widget.NewButton("Archive all in "+name, func() {
			w.confirm("Archive namespace", fmt.Sprintf("Archive every active memory in %q?", name), func() {
				go func() { _ = w.ctrl.ArchiveNamespace(w.ctx, name) }()
			})
		})
		p.archive.Importance = widget.DangerImportance
		objs = append(objs,
			container.NewBorder(nil, nil, nil, p.rename, p.renameTo),
			container.NewBorder(nil, nil, nil, p.merge, p.mergeTo),
			p.archive)
	}
	w.nsPanel = p
	w.nsPanelBox.Objects = objs
	w.nsPanelBox.Show()
}

// mergeNamespace merges name into the target after a confirmation. A blank
// target is sent as is so that the backend error reaches the banner.
func (w *MainWindow) mergeNamespace(name, target string) {
	run := func() { go func() { _ = w.ctrl.MergeNamespace(w.ctx, name, target) }() }
	target = strings.TrimSpace(target)
	if target == "" {
		run()
		return
	}
	w.confirm("Merge namespace",
		fmt.Sprintf("Move every memory of %q into %q and remove %q?", name, target, name), run)
}
