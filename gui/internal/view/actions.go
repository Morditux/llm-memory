package view

import (
	"fmt"
	"strconv"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/Morditux/llm-memory/gui/internal/core"
	"github.com/Morditux/llm-memory/internal/store"
)

var linkKinds = []string{
	string(store.LinkRelatesTo), string(store.LinkDependsOn), string(store.LinkSupersedes),
}

// detailActions are the buttons of the detail header.
type detailActions struct {
	edit, similar, archive, restore *widget.Button
}

// linkAddRow is the row adding a link to the displayed memory.
type linkAddRow struct {
	kind   *widget.Select
	target *widget.Entry
	add    *widget.Button
}

// actionButtons builds the header buttons: Restore for an archived memory,
// Edit, Find Similar and Archive otherwise.
func (w *MainWindow) actionButtons(m store.Memory) fyne.CanvasObject {
	id := m.ID
	if m.ArchivedAt != nil {
		w.actions.restore = widget.NewButton("Restore", func() {
			go func() { _ = w.ctrl.Restore(w.ctx, id) }()
		})
		return container.NewHBox(w.actions.restore)
	}
	w.actions.edit = widget.NewButton("Edit", func() { go w.ctrl.Edit() })
	w.actions.similar = widget.NewButton("Find Similar", func() {
		go func() { _ = w.ctrl.FindSimilar(w.ctx, id) }()
	})
	w.actions.archive = widget.NewButton("Archive", func() {
		w.confirm("Archive memory", fmt.Sprintf("Archive memory #%d?", id), func() {
			go func() { _ = w.ctrl.Archive(w.ctx, id) }()
		})
	})
	w.actions.archive.Importance = widget.DangerImportance
	return container.NewHBox(w.actions.edit, w.actions.similar, w.actions.archive)
}

// readBody is the read-only title, tags and content of a memory.
func (w *MainWindow) readBody(m store.Memory) []fyne.CanvasObject {
	var objs []fyne.CanvasObject
	if m.Title != "" {
		objs = append(objs, heading(m.Title))
	}
	if len(m.Tags) > 0 {
		tags := container.NewHBox()
		for _, t := range m.Tags {
			b := widget.NewButton("#"+t, func() { w.onTag(t) })
			b.Importance = widget.LowImportance
			w.tagButtons = append(w.tagButtons, b)
			tags.Add(b)
		}
		objs = append(objs, tags)
	}
	content := widget.NewLabel(m.Content)
	content.Wrapping = fyne.TextWrapWord
	content.Selectable = true
	return append(objs, content)
}

// keyEntry is an Entry that routes the window shortcuts and Escape, which a
// focused Entry would otherwise swallow.
type keyEntry struct {
	widget.Entry
	win *MainWindow
}

func (w *MainWindow) newKeyEntry(multiLine bool) *keyEntry {
	e := &keyEntry{win: w}
	e.MultiLine = multiLine
	if multiLine {
		e.Wrapping = fyne.TextWrapWord
	}
	e.ExtendBaseWidget(e)
	return e
}

// TypedKey implements fyne.Focusable.
func (e *keyEntry) TypedKey(k *fyne.KeyEvent) {
	if k.Name == fyne.KeyEscape {
		e.win.cancelEdit()
		return
	}
	e.Entry.TypedKey(k)
}

// TypedShortcut implements fyne.Shortcutable.
func (e *keyEntry) TypedShortcut(s fyne.Shortcut) {
	if !e.win.onShortcut(s) {
		e.Entry.TypedShortcut(s)
	}
}

// editForm is the edit mode of the detail panel.
type editForm struct {
	id        int64
	namespace *keyEntry
	kind      *widget.Select
	imp       *widget.Select
	title     *keyEntry
	tags      *keyEntry
	ttl       *keyEntry
	clear     *widget.Check // nil unless the memory has an expiration
	content   *keyEntry
	save      *widget.Button
	cancel    *widget.Button
}

func (w *MainWindow) newEditForm(m store.Memory) *editForm {
	kinds := memoryKinds
	if !contains(kinds, string(m.Kind)) {
		kinds = append(append([]string(nil), kinds...), string(m.Kind))
	}
	f := &editForm{
		id:        m.ID,
		namespace: w.newKeyEntry(false),
		kind:      widget.NewSelect(kinds, nil),
		imp:       widget.NewSelect(importances, nil),
		title:     w.newKeyEntry(false),
		tags:      w.newKeyEntry(false),
		ttl:       w.newKeyEntry(false),
		content:   w.newKeyEntry(true),
	}
	f.namespace.SetPlaceHolder("Namespace")
	f.namespace.SetText(m.Namespace)
	f.kind.SetSelected(string(m.Kind))
	f.imp.SetSelected(strconv.Itoa(m.Importance))
	f.title.SetPlaceHolder("Title")
	f.title.SetText(m.Title)
	f.tags.SetPlaceHolder("Tags comma-separated")
	f.tags.SetText(joinTags(m.Tags))
	f.ttl.SetPlaceHolder("New TTL (e.g. 24h, 7d)")
	f.content.SetText(m.Content)
	f.content.SetMinRowsVisible(8)
	if m.ExpiresAt != nil {
		f.clear = widget.NewCheck("Clear expiration ("+m.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")+")", nil)
	}
	f.cancel = widget.NewButton("Cancel", w.cancelEdit)
	f.save = widget.NewButton("Save", w.saveEdit)
	f.save.Importance = widget.HighImportance
	return f
}

func joinTags(tags []string) string {
	s := ""
	for i, t := range tags {
		if i > 0 {
			s += ", "
		}
		s += t
	}
	return s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (f *editForm) form() fyne.CanvasObject {
	items := []fyne.CanvasObject{
		f.namespace, container.NewGridWithColumns(2, f.kind, f.imp),
		f.title, f.tags, f.ttl,
	}
	if f.clear != nil {
		items = append(items, f.clear)
	}
	items = append(items, f.content, container.NewHBox(f.cancel, f.save))
	return container.NewVBox(items...)
}

func (f *editForm) values() core.EditForm {
	imp, _ := strconv.Atoi(f.imp.Selected)
	return core.EditForm{
		Namespace:    f.namespace.Text,
		Kind:         f.kind.Selected,
		Importance:   imp,
		Title:        f.title.Text,
		Tags:         f.tags.Text,
		ExpiresIn:    f.ttl.Text,
		ClearExpires: f.clear != nil && f.clear.Checked,
		Content:      f.content.Text,
	}
}

// saveEdit saves the edit form, if the detail is in edit mode.
func (w *MainWindow) saveEdit() {
	f := w.edit
	if f == nil {
		return
	}
	req, id := f.values().Request(), f.id
	go func() { _ = w.ctrl.Update(w.ctx, id, req) }()
}

// cancelEdit leaves edit mode without saving.
func (w *MainWindow) cancelEdit() {
	if w.edit != nil {
		go w.ctrl.CancelEdit()
	}
}

// buildLinkAdd builds the row adding a link from memory id. A blank or
// invalid target does nothing, like in the web explorer.
func (w *MainWindow) buildLinkAdd(id int64) fyne.CanvasObject {
	r := &w.linkAdd
	r.kind = widget.NewSelect(linkKinds, nil)
	r.kind.SetSelected(linkKinds[0])
	r.target = w.newKeyEntry(false).asEntry()
	r.target.SetPlaceHolder("Target #id")
	r.add = widget.NewButton("+ Link", func() {
		target, ok := core.ParseTargetID(r.target.Text)
		if !ok {
			return
		}
		kind := store.LinkKind(r.kind.Selected)
		go func() { _ = w.ctrl.AddLink(w.ctx, id, target, kind) }()
	})
	return container.NewBorder(nil, nil, r.kind, r.add, r.target)
}

func (e *keyEntry) asEntry() *widget.Entry { return &e.Entry }

// setSimilar shows the similar-memories section of the displayed memory.
func (w *MainWindow) setSimilar(v core.SimilarView) {
	if w.simBox == nil || v.MemoryID != w.shownID {
		return
	}
	w.supersedeBtn = nil
	var objs []fyne.CanvasObject
	switch {
	case v.State == core.SimilarSearching:
		objs = append(objs, mutedLabel("Searching similar memories…"))
	case v.State == core.SimilarDone && len(v.Items) == 0:
		objs = append(objs, mutedLabel("No similar memories found in "+v.Namespace+"."))
	case v.State == core.SimilarDone:
		objs = append(objs, heading(fmt.Sprintf("Similar in %s (%d)", v.Namespace, len(v.Items))))
		for _, s := range v.Items {
			objs = append(objs, w.similarCard(v.MemoryID, s))
		}
	}
	w.simBox.Objects = objs
	w.simBox.Refresh()
}

func mutedLabel(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.Importance = widget.LowImportance
	l.Wrapping = fyne.TextWrapWord
	return l
}

func (w *MainWindow) similarCard(id int64, s store.Similar) fyne.CanvasObject {
	btn := widget.NewButton(fmt.Sprintf("Supersede #%d", s.ID), func() {
		w.confirm("Supersede", fmt.Sprintf("Archive #%d and mark it as superseded by #%d?", s.ID, id), func() {
			go func() { _ = w.ctrl.Supersede(w.ctx, id, s.ID) }()
		})
	})
	btn.Importance = widget.DangerImportance
	w.supersedeBtn = append(w.supersedeBtn, btn)
	meta := widget.NewLabel(fmt.Sprintf("#%d · %s · %.0f%% match", s.ID, s.Kind, s.Similarity*100))
	rows := []fyne.CanvasObject{container.NewBorder(nil, nil, meta, btn)}
	if s.Title != "" {
		rows = append(rows, heading(s.Title))
	}
	snippet := widget.NewLabel(s.Snippet)
	snippet.Wrapping = fyne.TextWrapWord
	rows = append(rows, snippet, widget.NewSeparator())
	return container.NewVBox(rows...)
}
