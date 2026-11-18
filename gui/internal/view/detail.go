package view

import (
	"fmt"
	"image/color"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Morditux/llm-memory/gui/internal/core"
	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

const detailHelp = "Select a memory to inspect its content, links, diff history, and similar memories."

var (
	diffAddBG = color.NRGBA{R: 0x34, G: 0xd3, B: 0x99, A: 0x22}
	diffDelBG = color.NRGBA{R: 0xf8, G: 0x71, B: 0x71, A: 0x22}
)

// buildDetail builds the right column and shows the empty state.
func (w *MainWindow) buildDetail() fyne.CanvasObject {
	w.detail = container.NewVBox()
	w.detailScroll = container.NewVScroll(container.NewPadded(w.detail))
	w.setEmptyDetail()
	return w.detailScroll
}

func (w *MainWindow) setEmptyDetail() {
	w.resetDetailRefs()
	help := widget.NewLabel(detailHelp)
	help.Wrapping = fyne.TextWrapWord
	help.Importance = widget.LowImportance
	w.title = heading("Details")
	w.detail.Objects = []fyne.CanvasObject{w.title, help}
	w.detail.Refresh()
}

func (w *MainWindow) resetDetailRefs() {
	w.tagButtons, w.linkButtons, w.diffGrids, w.rawLabels = nil, nil, nil, nil
	w.supersedeBtn, w.unlinkBtns = nil, nil
	w.diffToggle = nil
	w.actions, w.edit, w.simBox, w.linkAdd = detailActions{}, nil, nil, linkAddRow{}
}

func heading(text string) *widget.Label {
	return widget.NewLabelWithStyle(text, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
}

// setDetail shows the selected memory with its links and history.
func (w *MainWindow) setDetail(v core.DetailView) {
	w.clearError()
	m := v.Memory
	w.resetDetailRefs()
	w.title = heading(fmt.Sprintf("Memory #%d", m.ID))
	objs := []fyne.CanvasObject{
		container.NewBorder(nil, nil, w.title, w.actionButtons(m)),
		widget.NewRichText(memoryMeta(m, false)...),
	}
	if v.Editing {
		w.edit = w.newEditForm(m)
		objs = append(objs, w.edit.form())
	} else {
		objs = append(objs, w.readBody(m)...)
	}
	w.simBox = container.NewVBox()
	objs = append(objs, w.simBox)

	objs = append(objs, heading(fmt.Sprintf("Links (%d)", len(v.Links))))
	if len(v.Links) == 0 {
		none := widget.NewLabel("No links.")
		none.Importance = widget.LowImportance
		objs = append(objs, none)
	}
	for _, l := range v.Links {
		objs = append(objs, w.linkRow(m.ID, l))
	}
	objs = append(objs, w.buildLinkAdd(m.ID))

	w.diffToggle = widget.NewButton(toggleLabel(v.DiffMode), func() {
		go w.ctrl.SetDiffMode(!v.DiffMode)
	})
	objs = append(objs, container.NewBorder(nil, nil,
		heading(fmt.Sprintf("History (%d)", len(v.History))), w.diffToggle))
	for _, h := range v.History {
		objs = append(objs, w.historyRow(h, v.DiffMode))
	}

	w.detail.Objects = objs
	w.detail.Refresh()
	if w.shownID != m.ID {
		w.detailScroll.ScrollToTop()
	}
	w.shownID = m.ID
	w.syncSelection()
}

func toggleLabel(diff bool) string {
	if diff {
		return "Show Raw"
	}
	return "Show Diff"
}

// onTag applies a tag as the only tags filter.
func (w *MainWindow) onTag(tag string) {
	w.ctrl.SelectTag(tag)
	w.syncing = true
	w.tags.SetText(tag)
	w.syncing = false
	w.loadList()
}

func (w *MainWindow) linkRow(id int64, l store.LinkView) fyne.CanvasObject {
	kind := string(l.Kind)
	if l.Direction != "outgoing" {
		kind = "← " + kind
	}
	jump := widget.NewButton(fmt.Sprintf("#%d", l.ID), func() { w.loadDetail(l.ID) })
	jump.Importance = widget.LowImportance
	w.linkButtons = append(w.linkButtons, jump)

	row := container.NewHBox(widget.NewLabelWithStyle(kind, fyne.TextAlignLeading, fyne.TextStyle{Italic: true}), jump)
	mk := widget.NewLabel("[" + string(l.MemoryKind) + "]")
	mk.Importance = widget.LowImportance
	row.Add(mk)
	if l.Title != "" {
		row.Add(widget.NewLabel(l.Title))
	}
	if l.Archived {
		a := widget.NewLabel("(archived)")
		a.Importance = widget.DangerImportance
		row.Add(a)
	}
	if l.Direction != "outgoing" || l.Kind == store.LinkSupersedes {
		return row
	}
	rm := widget.NewButton("×", func() {
		go func() { _ = w.ctrl.RemoveLink(w.ctx, id, l.ID, l.Kind) }()
	})
	rm.Importance = widget.DangerImportance
	w.unlinkBtns = append(w.unlinkBtns, rm)
	return container.NewBorder(nil, nil, nil, rm, row)
}

func (w *MainWindow) historyRow(h core.HistoryItem, diff bool) fyne.CanvasObject {
	meta := widget.NewRichText(
		&widget.TextSegment{Text: string(h.Action), Style: widget.RichTextStyle{Inline: true, TextStyle: fyne.TextStyle{Bold: true}}},
		&widget.TextSegment{Text: fmt.Sprintf(" · %s · %s", h.ChangedAt.Format(time.RFC3339Nano), h.ChangedBy),
			Style: widget.RichTextStyle{Inline: true, ColorName: theme.ColorNamePlaceHolder}},
	)
	if !diff {
		raw := widget.NewLabel(h.Snapshot)
		raw.Wrapping = fyne.TextWrapBreak
		raw.Selectable = true
		raw.TextStyle = fyne.TextStyle{Monospace: true}
		w.rawLabels = append(w.rawLabels, raw)
		return container.NewVBox(meta, raw)
	}
	grid := diffGrid(h.Diff)
	w.diffGrids = append(w.diffGrids, grid)
	return container.NewVBox(meta, container.NewHScroll(grid))
}

// diffGrid renders diff lines with the web explorer's "  ", "- ", "+ "
// prefixes and green/red/muted colors.
func diffGrid(lines []explorer.DiffLine) *widget.TextGrid {
	g := widget.NewTextGrid()
	g.Rows = make([]widget.TextGridRow, len(lines))
	for i, l := range lines {
		prefix, fg := "  ", color.Color(colorMuted)
		var bg color.Color
		switch l.Op {
		case explorer.Add:
			prefix, fg, bg = "+ ", colorGreen, diffAddBG
		case explorer.Del:
			prefix, fg, bg = "- ", colorRed, diffDelBG
		}
		style := &widget.CustomTextGridStyle{FGColor: fg, BGColor: bg}
		cells := make([]widget.TextGridCell, 0, len(prefix)+len(l.Text))
		for _, r := range prefix + l.Text {
			cells = append(cells, widget.TextGridCell{Rune: r, Style: style})
		}
		g.Rows[i] = widget.TextGridRow{Cells: cells}
	}
	g.Refresh()
	return g
}
