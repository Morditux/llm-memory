package view

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Morditux/llm-memory/gui/internal/core"
	"github.com/Morditux/llm-memory/internal/store"
)

const (
	allKinds      = "All kinds"
	previewLimit  = 240
	tagsFieldWide = 150
)

var (
	kindOptions     = []string{allKinds, "handoff", "decision", "preference", "fact", "task_state", "note"}
	archivedOptions = []string{"Active", "Archived", "All"}
	archivedFilters = []store.ArchiveFilter{store.ActiveOnly, store.ArchivedOnly, store.IncludeArchived}
)

// buildCenter builds the middle column: status banner, filters, memory cards
// and pager.
func (w *MainWindow) buildCenter() fyne.CanvasObject {
	w.banner = widget.NewLabel("")
	w.banner.Importance = widget.DangerImportance
	w.banner.Wrapping = fyne.TextWrapWord
	w.banner.Hide()

	w.search = widget.NewEntry()
	w.search.SetPlaceHolder("Search memories (FTS / semantic)…")
	w.search.OnChanged = func(s string) { w.ctrl.SetQuery(w.ctx, s) }
	w.tags = widget.NewEntry()
	w.tags.SetPlaceHolder("Tags (comma-sep)")
	w.tags.OnChanged = func(s string) {
		if !w.syncing {
			w.ctrl.SetTags(w.ctx, s)
		}
	}
	w.kind = widget.NewSelect(kindOptions, nil)
	w.kind.SetSelected(allKinds)
	w.kind.OnChanged = func(s string) {
		kind := store.Kind(s)
		if s == allKinds {
			kind = ""
		}
		w.ctrl.SetKind(kind)
		w.loadList()
	}
	w.arch = widget.NewSelect(archivedOptions, nil)
	w.arch.SetSelected(archivedOptions[0])
	w.arch.OnChanged = func(string) {
		w.ctrl.SetArchived(archivedFilters[max(w.arch.SelectedIndex(), 0)])
		w.loadList()
	}
	tagsBox := container.NewGridWrap(fyne.NewSize(tagsFieldWide, w.tags.MinSize().Height), w.tags)
	filters := container.NewBorder(nil, nil, nil, container.NewHBox(tagsBox, w.kind, w.arch), w.search)

	w.list = widget.NewList(
		func() int { return len(w.items) },
		newCard,
		func(i widget.ListItemID, o fyne.CanvasObject) { bindCard(o, w.items[i]) },
	)
	w.list.OnSelected = func(i widget.ListItemID) {
		if !w.syncing {
			w.loadDetail(w.items[i].ID)
		}
	}
	w.empty = widget.NewLabel("No memories match the current filter.")
	w.empty.Importance = widget.LowImportance
	w.empty.Hide()

	w.prevBtn = widget.NewButton("← Prev", func() { w.ctrl.PrevPage(); w.loadList() })
	w.nextBtn = widget.NewButton("Next →", func() { w.ctrl.NextPage(); w.loadList() })
	w.pageLbl = widget.NewLabel("")
	w.pager = container.NewCenter(container.NewHBox(w.prevBtn, w.pageLbl, w.nextBtn))
	w.pager.Hide()

	top := container.NewVBox(w.banner, filters)
	return container.NewBorder(top, w.pager, nil, nil, container.NewStack(w.list, w.empty))
}

// setList shows a loaded page of the list.
func (w *MainWindow) setList(r core.ListResult) {
	w.clearError()
	w.items = r.Items
	for i, m := range r.Items {
		w.list.SetItemHeight(i, cardHeight(m))
	}
	w.empty.Hidden = len(r.Items) > 0
	w.empty.Refresh()
	w.list.Hidden = len(r.Items) == 0
	w.pager.Hidden = !r.PagerVisible
	w.pager.Refresh()
	w.pageLbl.SetText(r.PageLabel)
	setDisabled(w.prevBtn, !r.PrevEnabled)
	setDisabled(w.nextBtn, !r.NextEnabled)
	w.list.Refresh()
	w.list.ScrollToTop()
	w.syncSelection()
}

// syncSelection highlights the card of the selected memory, if listed.
func (w *MainWindow) syncSelection() {
	id := w.ctrl.SelectedID()
	w.syncing = true
	defer func() { w.syncing = false }()
	w.list.UnselectAll()
	for i, m := range w.items {
		if m.ID == id {
			w.list.Select(i)
			return
		}
	}
}

func setDisabled(b *widget.Button, off bool) {
	if off {
		b.Disable()
	} else {
		b.Enable()
	}
}

// newCard builds the template of a memory card: meta line, title, preview.
func newCard() fyne.CanvasObject {
	title := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	preview := widget.NewLabel("")
	preview.Truncation = fyne.TextTruncateEllipsis
	return container.NewVBox(widget.NewRichText(), title, preview)
}

func bindCard(o fyne.CanvasObject, m store.Memory) {
	box := o.(*fyne.Container)
	meta := box.Objects[0].(*widget.RichText)
	meta.Segments = memoryMeta(m, true)
	meta.Refresh()
	title := box.Objects[1].(*widget.Label)
	title.SetText(m.Title)
	title.Hidden = m.Title == ""
	title.Refresh()
	box.Objects[2].(*widget.Label).SetText(preview(m.Content))
	box.Refresh()
}

// cardHeight is the height of the card of m: its rows plus inter-row padding.
func cardHeight(m store.Memory) float32 {
	rows := float32(2)
	if m.Title != "" {
		rows++
	}
	row := theme.TextSize() + theme.InnerPadding()*2
	return rows*row + (rows+1)*theme.Padding()
}

// preview collapses whitespace and truncates the content for a card.
func preview(content string) string {
	s := strings.Join(strings.Fields(content), " ")
	if r := []rune(s); len(r) > previewLimit {
		return string(r[:previewLimit]) + "…"
	}
	return s
}

// memoryMeta is the meta line of a card (list) or of the detail panel.
func memoryMeta(m store.Memory, card bool) []widget.RichTextSegment {
	var segs []widget.RichTextSegment
	add := func(text string, color fyne.ThemeColorName, bold bool) {
		segs = append(segs, &widget.TextSegment{
			Text: text + "  ",
			Style: widget.RichTextStyle{
				Inline:    true,
				ColorName: color,
				TextStyle: fyne.TextStyle{Bold: bold},
			},
		})
	}
	muted := theme.ColorNamePlaceHolder
	if card {
		add(fmt.Sprintf("#%d", m.ID), muted, false)
	}
	add(string(m.Kind), theme.ColorNamePrimary, true)
	add(m.Namespace, muted, false)
	if !card {
		add("by "+m.UpdatedBy, muted, false)
	}
	if m.Importance == 3 {
		add(fmt.Sprintf("imp:%d", m.Importance), theme.ColorNameWarning, true)
	} else {
		add(fmt.Sprintf("imp:%d", m.Importance), muted, false)
	}
	if card && m.ArchivedAt != nil {
		add("archived", theme.ColorNameError, false)
	}
	if m.ExpiresAt != nil {
		label := "expires:"
		if card {
			label = "ttl:"
		}
		add(label+m.ExpiresAt.Format("2006-01-02"), muted, false)
	}
	return segs
}
