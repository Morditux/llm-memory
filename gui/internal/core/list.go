package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Morditux/llm-memory/internal/store"
)

// DebounceDelay is how long the search query and the tags filter must stay
// unchanged before the list is reloaded.
const DebounceDelay = 200 * time.Millisecond

// AfterFunc schedules f to run once after d and returns a function that
// cancels it. It matches time.AfterFunc and is injectable for tests.
type AfterFunc func(d time.Duration, f func()) (stop func() bool)

func timeAfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// ListResult is one loaded page of the memory list.
type ListResult struct {
	Items []store.Memory
	// Search is true when the items are search hits; there is no paging then.
	Search       bool
	Offset       int
	PagerVisible bool
	PrevEnabled  bool
	NextEnabled  bool
	// PageLabel reads "Page n (k shown)" when the pager is relevant.
	PageLabel string
}

// ParseTags turns the comma-separated tags field into normalized tags.
// A blank field yields nil.
func ParseTags(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return store.NormalizeTags(strings.Split(text, ","))
}

// listFilter maps the filters to a store.ListFilter for the current page.
func (f Filters) listFilter() store.ListFilter {
	lf := store.ListFilter{
		Namespace: f.Namespace,
		Tags:      f.Tags,
		Archived:  f.Archived,
		Limit:     PageSize,
		Offset:    f.Offset,
	}
	if f.Kind != "" {
		lf.Kinds = []store.Kind{f.Kind}
	}
	return lf
}

// searchQuery maps the filters to a store.SearchQuery. Like the web explorer,
// any archive filter other than "active" includes archived memories.
func (f Filters) searchQuery() store.SearchQuery {
	q := store.SearchQuery{
		Query:           f.Query,
		Namespace:       f.Namespace,
		Tags:            f.Tags,
		IncludeArchived: f.Archived != store.ActiveOnly,
		Limit:           PageSize,
	}
	if f.Kind != "" {
		q.Kinds = []store.Kind{f.Kind}
	}
	return q
}

// SetQuery records the search text and schedules a list reload after
// DebounceDelay, restarting the delay on every call. The text is trimmed.
func (c *Controller) SetQuery(ctx context.Context, text string) {
	c.mu.Lock()
	c.filters.Query = strings.TrimSpace(text)
	c.filters.Offset = 0
	c.mu.Unlock()
	c.debounce(ctx)
}

// SetTags records the comma-separated tags filter and schedules a list reload
// after DebounceDelay, restarting the delay on every call.
func (c *Controller) SetTags(ctx context.Context, text string) {
	c.mu.Lock()
	c.filters.Tags = ParseTags(text)
	c.filters.Offset = 0
	c.mu.Unlock()
	c.debounce(ctx)
}

// SetKind records the kind filter ("" for all) and resets pagination. The
// caller reloads the list with LoadList.
func (c *Controller) SetKind(kind store.Kind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filters.Kind = kind
	c.filters.Offset = 0
}

// SetArchived records the archive filter and resets pagination. The caller
// reloads the list with LoadList.
func (c *Controller) SetArchived(a store.ArchiveFilter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filters.Archived = a
	c.filters.Offset = 0
}

// SelectTag makes tag the only tags filter and resets pagination. The caller
// reloads the list with LoadList.
func (c *Controller) SelectTag(tag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filters.Tags = ParseTags(tag)
	c.filters.Offset = 0
}

// NextPage advances to the next page. The caller reloads with LoadList.
func (c *Controller) NextPage() { c.shiftPage(PageSize) }

// PrevPage goes back one page, not below the first. The caller reloads with
// LoadList.
func (c *Controller) PrevPage() { c.shiftPage(-PageSize) }

func (c *Controller) shiftPage(delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filters.Offset = max(c.filters.Offset+delta, 0)
}

func (c *Controller) debounce(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil {
		c.pending()
	}
	c.pending = c.after(DebounceDelay, func() { _ = c.LoadList(ctx) })
}

// LoadList loads the memory list for the current filters and notifies the
// observer: a search when the query is not empty, a paged listing otherwise.
// Responses superseded by a more recent call are dropped. A pending debounce
// is cancelled since this call already reflects the latest filters.
func (c *Controller) LoadList(ctx context.Context) error {
	c.mu.Lock()
	if c.pending != nil {
		c.pending()
		c.pending = nil
	}
	f := c.filters
	f.Tags = append([]string(nil), f.Tags...)
	c.mu.Unlock()

	gen := c.next("list")
	var res ListResult
	if f.Query != "" {
		hits, err := c.backend.Search(ctx, f.searchQuery())
		if err != nil {
			return c.failIfCurrent("list", gen, err)
		}
		res.Search = true
		res.Items = make([]store.Memory, len(hits))
		for i, h := range hits {
			res.Items[i] = h.Memory
		}
	} else {
		items, err := c.backend.List(ctx, f.listFilter())
		if err != nil {
			return c.failIfCurrent("list", gen, err)
		}
		res.Items = items
		res.Offset = f.Offset
		res.PagerVisible = f.Offset > 0 || len(items) == PageSize
		res.PrevEnabled = f.Offset > 0
		res.NextEnabled = len(items) >= PageSize
		res.PageLabel = fmt.Sprintf("Page %d (%d shown)", f.Offset/PageSize+1, len(items))
	}
	if c.stale("list", gen) {
		return nil
	}
	if c.obs.List != nil {
		c.notify.Lock()
		defer c.notify.Unlock()
		c.obs.List(res)
	}
	return nil
}

// failIfCurrent reports err unless a newer request of the kind superseded it.
func (c *Controller) failIfCurrent(kind string, gen uint64, err error) error {
	if !c.stale(kind, gen) {
		c.fail(err)
	}
	return err
}
