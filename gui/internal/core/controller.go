package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/Morditux/llm-memory/internal/store"
)

// PageSize is the number of memories per page of the memory list.
const PageSize = 50

// StatsLine formats stats the way the web explorer does.
func StatsLine(s store.Stats) string {
	return fmt.Sprintf("%d active · %d archived · schema v%d", s.Active, s.Archived, s.SchemaVersion)
}

// Filters is the user-selected restriction of the memory list.
type Filters struct {
	Namespace string // empty means all namespaces
	Query     string
	Tags      []string
	Kind      store.Kind // empty means all kinds
	Archived  store.ArchiveFilter
	Offset    int
}

// Observer receives controller events. Callbacks run on the goroutine that
// called the controller method and are never invoked concurrently, so a view
// must marshal them to its UI thread. Nil callbacks are ignored.
type Observer struct {
	Stats      func(store.Stats)
	Namespaces func([]store.Namespace)
	Error      func(error)
}

// Controller owns the UI state and loads data from a Backend.
type Controller struct {
	backend Backend
	obs     Observer

	notify sync.Mutex // serialises observer callbacks

	mu         sync.Mutex
	filters    Filters
	selectedID int64
	gens       map[string]uint64
}

// NewController returns a Controller reading from b and reporting to obs.
func NewController(b Backend, obs Observer) *Controller {
	return &Controller{backend: b, obs: obs}
}

// Filters returns the current filters.
func (c *Controller) Filters() Filters {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.filters
	f.Tags = append([]string(nil), f.Tags...)
	return f
}

// SelectNamespace selects a namespace ("" for all) and resets pagination.
func (c *Controller) SelectNamespace(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filters.Namespace = name
	c.filters.Offset = 0
}

// SetFilters replaces the filters other than the namespace and resets pagination.
func (c *Controller) SetFilters(query string, tags []string, kind store.Kind, archived store.ArchiveFilter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filters.Query = query
	c.filters.Tags = append([]string(nil), tags...)
	c.filters.Kind = kind
	c.filters.Archived = archived
	c.filters.Offset = 0
}

// SetOffset sets the pagination offset; negative values are clamped to zero.
func (c *Controller) SetOffset(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filters.Offset = max(n, 0)
}

// Select records the selected memory id (0 for none).
func (c *Controller) Select(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selectedID = id
}

// SelectedID returns the selected memory id, or 0.
func (c *Controller) SelectedID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedID
}

// LoadStats fetches the database stats and notifies the observer. Responses
// superseded by a more recent call are dropped.
func (c *Controller) LoadStats(ctx context.Context) error {
	gen := c.next("stats")
	s, err := c.backend.Stats(ctx)
	if err != nil {
		c.fail(err)
		return err
	}
	if c.stale("stats", gen) {
		return nil
	}
	if c.obs.Stats != nil {
		c.notify.Lock()
		defer c.notify.Unlock()
		c.obs.Stats(s)
	}
	return nil
}

// LoadNamespaces fetches the namespaces and notifies the observer.
func (c *Controller) LoadNamespaces(ctx context.Context) error {
	gen := c.next("namespaces")
	ns, err := c.backend.Namespaces(ctx)
	if err != nil {
		c.fail(err)
		return err
	}
	if c.stale("namespaces", gen) {
		return nil
	}
	if c.obs.Namespaces != nil {
		c.notify.Lock()
		defer c.notify.Unlock()
		c.obs.Namespaces(ns)
	}
	return nil
}

// Refresh reloads stats and namespaces concurrently and returns the first error.
func (c *Controller) Refresh(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = c.LoadStats(ctx) }()
	go func() { defer wg.Done(); errs[1] = c.LoadNamespaces(ctx) }()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) fail(err error) {
	if c.obs.Error != nil {
		c.notify.Lock()
		defer c.notify.Unlock()
		c.obs.Error(err)
	}
}

// next starts a request of the given kind and returns its generation.
func (c *Controller) next(kind string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gens == nil {
		c.gens = map[string]uint64{}
	}
	c.gens[kind]++
	return c.gens[kind]
}

// stale reports whether a newer request of the given kind has started.
func (c *Controller) stale(kind string, gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gens[kind] != gen
}
