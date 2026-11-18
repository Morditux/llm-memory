package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

// ArchiveReason is the reason recorded when a memory is archived from the GUI.
const ArchiveReason = "archived from desktop GUI"

// DefaultNamespace is the namespace used when the creation form leaves it blank.
const DefaultNamespace = "global"

// CreateForm holds the raw values of the creation form.
type CreateForm struct {
	Namespace  string
	Kind       string
	Importance int
	Title      string
	Tags       string // comma-separated
	ExpiresIn  string
	Content    string
}

// Request builds the creation request like the web explorer: the namespace
// and title are trimmed (blank namespace means "global"), the tags are split
// on commas, trimmed and stripped of empty items, and expires_in is trimmed.
func (f CreateForm) Request() explorer.CreateRequest {
	ns := strings.TrimSpace(f.Namespace)
	if ns == "" {
		ns = DefaultNamespace
	}
	return explorer.CreateRequest{
		Namespace:  ns,
		Kind:       store.Kind(f.Kind),
		Importance: f.Importance,
		Title:      strings.TrimSpace(f.Title),
		Tags:       splitTags(f.Tags),
		ExpiresIn:  strings.TrimSpace(f.ExpiresIn),
		Content:    f.Content,
	}
}

// EditForm holds the raw values of the edit form.
type EditForm struct {
	Namespace    string
	Kind         string
	Importance   int
	Title        string
	Tags         string // comma-separated
	ExpiresIn    string
	ClearExpires bool
	Content      string
}

// Request builds the update request like the web explorer: every field is
// sent (the namespace trimmed, the title as typed), expires_in only when it is
// not blank, and clear_expires when ticked.
func (f EditForm) Request() explorer.UpdateRequest {
	ns := strings.TrimSpace(f.Namespace)
	kind := store.Kind(f.Kind)
	tags := splitTags(f.Tags)
	r := explorer.UpdateRequest{
		Namespace:    &ns,
		Kind:         &kind,
		Importance:   &f.Importance,
		Title:        &f.Title,
		Tags:         &tags,
		Content:      &f.Content,
		ClearExpires: f.ClearExpires,
	}
	if ttl := strings.TrimSpace(f.ExpiresIn); ttl != "" {
		r.ExpiresIn = ttl
	}
	return r
}

// splitTags splits on commas, trims and drops empty items. It never returns
// nil so that an update with no tags clears them.
func splitTags(text string) []string {
	tags := []string{}
	for _, t := range strings.Split(text, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// ParseTargetID parses the link target field. ok is false when the field is
// blank or not a positive integer, in which case the link must not be added.
func ParseTargetID(text string) (id int64, ok bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	return id, err == nil && id > 0
}

// SimilarState is the progress of a FindSimilar request.
type SimilarState int

// SimilarState values.
const (
	SimilarSearching SimilarState = iota + 1
	SimilarDone
	// SimilarFailed means the search failed and the section must be cleared;
	// the error is reported through the banner.
	SimilarFailed
)

// SimilarView is the similar-memories section of a memory.
type SimilarView struct {
	MemoryID  int64
	Namespace string
	State     SimilarState
	Items     []store.Similar
}

// emit runs f serialised with the other observer callbacks.
func (c *Controller) emit(f func()) {
	c.notify.Lock()
	defer c.notify.Unlock()
	f()
}

func (c *Controller) banner(msg string, isErr bool) {
	if c.obs.Banner != nil {
		c.emit(func() { c.obs.Banner(msg, isErr) })
	}
}

// bannerErr reports a failed mutation and returns err.
func (c *Controller) bannerErr(err error) error {
	c.banner(err.Error(), true)
	return err
}

// refresh reloads stats, namespaces and the list concurrently, then the
// detail of id when it is not zero, like the web explorer's refresh(id).
func (c *Controller) refresh(ctx context.Context, id int64) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); _ = c.LoadStats(ctx) }()
	go func() { defer wg.Done(); _ = c.LoadNamespaces(ctx) }()
	go func() { defer wg.Done(); _ = c.LoadList(ctx) }()
	wg.Wait()
	if id != 0 {
		_ = c.LoadDetail(ctx, id)
	}
}

// Create creates a memory, announces it, then reloads everything and shows
// the new memory.
func (c *Controller) Create(ctx context.Context, r explorer.CreateRequest) (store.Memory, error) {
	m, err := c.backend.Create(ctx, r)
	if err != nil {
		return store.Memory{}, c.bannerErr(err)
	}
	c.banner(fmt.Sprintf("Created memory #%d", m.ID), false)
	c.refresh(ctx, m.ID)
	return m, nil
}

// Update modifies memory id and reloads everything; the detail returns to its
// read-only form.
func (c *Controller) Update(ctx context.Context, id int64, r explorer.UpdateRequest) error {
	if _, err := c.backend.Update(ctx, id, r); err != nil {
		return c.bannerErr(err)
	}
	c.banner(fmt.Sprintf("Updated memory #%d", id), false)
	c.refresh(ctx, id)
	return nil
}

// Archive archives memory id with ArchiveReason and reloads everything.
func (c *Controller) Archive(ctx context.Context, id int64) error {
	if _, err := c.backend.Archive(ctx, id, ArchiveReason); err != nil {
		return c.bannerErr(err)
	}
	c.banner(fmt.Sprintf("Archived memory #%d", id), false)
	c.refresh(ctx, id)
	return nil
}

// Restore un-archives memory id and reloads everything.
func (c *Controller) Restore(ctx context.Context, id int64) error {
	if _, err := c.backend.Restore(ctx, id); err != nil {
		return c.bannerErr(err)
	}
	c.banner(fmt.Sprintf("Restored memory #%d", id), false)
	c.refresh(ctx, id)
	return nil
}

// GC archives the expired memories and reloads everything, keeping the
// selected memory displayed.
func (c *Controller) GC(ctx context.Context) error {
	n, err := c.backend.GC(ctx)
	if err != nil {
		return c.bannerErr(err)
	}
	c.banner(fmt.Sprintf("GC archived %d expired memory(ies)", n), false)
	c.refresh(ctx, c.SelectedID())
	return nil
}

// Supersede makes memory id supersede targetID, which gets archived, then
// reloads everything.
func (c *Controller) Supersede(ctx context.Context, id, targetID int64) error {
	if _, err := c.backend.Supersede(ctx, id, targetID); err != nil {
		return c.bannerErr(err)
	}
	c.banner(fmt.Sprintf("Archived #%d (superseded by #%d)", targetID, id), false)
	c.refresh(ctx, id)
	return nil
}

// AddLink links id to targetID and reloads only the detail of id.
func (c *Controller) AddLink(ctx context.Context, id, targetID int64, kind store.LinkKind) error {
	if _, err := c.backend.AddLink(ctx, id, targetID, kind); err != nil {
		return c.bannerErr(err)
	}
	return c.LoadDetail(ctx, id)
}

// RemoveLink removes the link from id to targetID and reloads only the detail
// of id.
func (c *Controller) RemoveLink(ctx context.Context, id, targetID int64, kind store.LinkKind) error {
	if _, err := c.backend.RemoveLink(ctx, id, targetID, kind); err != nil {
		return c.bannerErr(err)
	}
	return c.LoadDetail(ctx, id)
}

// FindSimilar searches the memories similar to id and reports the progress
// through the Similar observer. The result is dropped when the selection
// changed meanwhile or a newer search started.
func (c *Controller) FindSimilar(ctx context.Context, id int64) error {
	gen := c.next("similar")
	c.mu.Lock()
	ns := ""
	if c.lastDetail != nil && c.lastDetail.Memory.ID == id {
		ns = c.lastDetail.Memory.Namespace
	}
	v := SimilarView{MemoryID: id, Namespace: ns, State: SimilarSearching}
	c.similar = &v
	c.mu.Unlock()
	c.notifySimilar(v)

	items, err := c.backend.Similar(ctx, id)
	c.mu.Lock()
	current := c.gens["similar"] == gen && c.selectedID == id
	if current {
		if err != nil {
			v.State = SimilarFailed
			c.similar = nil
		} else {
			v.State, v.Items = SimilarDone, items
			c.similar = &v
		}
	}
	c.mu.Unlock()
	if !current {
		return err
	}
	if err != nil {
		c.notifySimilar(v)
		return c.bannerErr(err)
	}
	c.notifySimilar(v)
	return nil
}

func (c *Controller) notifySimilar(v SimilarView) {
	if c.obs.Similar != nil {
		c.emit(func() { c.obs.Similar(v) })
	}
}

// Editing reports whether the displayed memory is in edit mode.
func (c *Controller) Editing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.editing
}

// Edit switches the displayed memory to its edit form. It does nothing when
// no memory is displayed or when it is archived.
func (c *Controller) Edit() {
	c.mu.Lock()
	ok := c.lastDetail != nil && c.lastDetail.Memory.ArchivedAt == nil
	if ok {
		c.editing = true
	}
	c.mu.Unlock()
	if ok {
		c.renotify()
	}
}

// CancelEdit leaves edit mode without saving.
func (c *Controller) CancelEdit() {
	c.mu.Lock()
	was := c.editing
	c.editing = false
	c.mu.Unlock()
	if was {
		c.renotify()
	}
}
