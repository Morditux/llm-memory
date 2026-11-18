package core

import (
	"context"

	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

// HistoryItem is a history entry with its diff against the previous one.
type HistoryItem struct {
	store.HistoryEntry
	// Diff is nil unless the diff mode is on.
	Diff []explorer.DiffLine
}

// DetailView is the selected memory with its links and history.
type DetailView struct {
	Memory   store.Memory
	Links    []store.LinkView
	History  []HistoryItem
	DiffMode bool
	// Editing is true while the memory is shown in its edit form.
	Editing bool
}

func newDetailView(d explorer.Detail, diff, editing bool) DetailView {
	v := DetailView{Memory: d.Memory, Links: d.Links, DiffMode: diff, Editing: editing}
	v.History = make([]HistoryItem, len(d.History))
	for i, h := range d.History {
		v.History[i].HistoryEntry = h
		if diff {
			prev := ""
			if i > 0 {
				prev = d.History[i-1].Snapshot
			}
			v.History[i].Diff = explorer.LineDiff(prev, h.Snapshot)
		}
	}
	return v
}

// DiffMode reports whether history entries are shown as diffs (the default).
func (c *Controller) DiffMode() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.diffMode
}

// SetDiffMode switches between diff and raw history and re-notifies the
// observer with the detail already loaded, if any.
func (c *Controller) SetDiffMode(on bool) {
	c.mu.Lock()
	c.diffMode = on
	c.mu.Unlock()
	c.renotify()
}

// renotify sends the detail already loaded, and the similar results of that
// memory if any, to the observer again.
func (c *Controller) renotify() {
	c.mu.Lock()
	d := c.lastDetail
	diff, editing := c.diffMode, c.editing
	sim := c.similar
	c.mu.Unlock()
	if d == nil {
		return
	}
	c.notify.Lock()
	defer c.notify.Unlock()
	if c.obs.Detail != nil {
		c.obs.Detail(newDetailView(*d, diff, editing))
	}
	if sim != nil && c.obs.Similar != nil {
		c.obs.Similar(*sim)
	}
}

// LoadDetail selects memory id and loads its detail. It is also the link jump.
// Responses superseded by a more recent call are dropped.
func (c *Controller) LoadDetail(ctx context.Context, id int64) error {
	c.mu.Lock()
	c.selectedID = id
	c.mu.Unlock()
	gen := c.next("detail")
	d, err := c.backend.Detail(ctx, id)
	if err != nil {
		return c.failIfCurrent("detail", gen, err)
	}
	c.mu.Lock()
	if c.gens["detail"] != gen {
		c.mu.Unlock()
		return nil
	}
	c.lastDetail = &d
	c.editing = false
	c.similar = nil
	diff := c.diffMode
	c.mu.Unlock()
	if c.obs.Detail != nil {
		c.notify.Lock()
		defer c.notify.Unlock()
		c.obs.Detail(newDetailView(d, diff, false))
	}
	return nil
}
