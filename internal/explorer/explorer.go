// Package explorer holds the memory-browsing operations shared by the web
// explorer (internal/ui) and desktop front-ends. It contains no transport
// concern: callers map its results and errors to HTTP, widgets, etc.
package explorer

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/Morditux/llm-memory/internal/store"
)

// DefaultActor is the author recorded for writes made through the web explorer.
const DefaultActor = "ui"

// similarLimit is the maximum number of similar memories returned.
const similarLimit = 5

// Searcher abstracts store.Search and semantic.Engine.Search.
type Searcher interface {
	Search(ctx context.Context, q store.SearchQuery) ([]store.SearchResult, error)
}

type similarSearcher interface {
	FindSimilar(ctx context.Context, text, namespace string, minScore float64, limit int) ([]store.Similar, error)
}

// Explorer exposes the operations needed to browse and edit memories.
type Explorer struct {
	st     *store.Store
	search Searcher
	actor  string
}

// New returns an Explorer over st. search may be nil, in which case the store's
// full-text search is used. actor is recorded as the author of writes; an empty
// value means DefaultActor.
func New(st *store.Store, search Searcher, actor string) *Explorer {
	if search == nil {
		search = st
	}
	if actor == "" {
		actor = DefaultActor
	}
	return &Explorer{st: st, search: search, actor: actor}
}

// Detail is a memory together with its history and links.
type Detail struct {
	Memory  store.Memory         `json:"memory"`
	History []store.HistoryEntry `json:"history"`
	Links   []store.LinkView     `json:"links"`
}

// CreateRequest describes a memory to create.
type CreateRequest struct {
	Namespace  string     `json:"namespace"`
	Kind       store.Kind `json:"kind"`
	Title      string     `json:"title"`
	Content    string     `json:"content"`
	Tags       []string   `json:"tags"`
	Importance int        `json:"importance"`
	ExpiresIn  string     `json:"expires_in"`
	ExpiresAt  string     `json:"expires_at"`
	RelatedTo  []int64    `json:"related_to"`
	DependsOn  []int64    `json:"depends_on"`
}

// UpdateRequest describes a partial update; nil fields are left unchanged.
type UpdateRequest struct {
	Namespace    *string     `json:"namespace"`
	Kind         *store.Kind `json:"kind"`
	Title        *string     `json:"title"`
	Content      *string     `json:"content"`
	Tags         *[]string   `json:"tags"`
	Importance   *int        `json:"importance"`
	ExpiresIn    string      `json:"expires_in"`
	ExpiresAt    string      `json:"expires_at"`
	ClearExpires bool        `json:"clear_expires"`
}

// Stats returns database counters.
func (e *Explorer) Stats(ctx context.Context) (store.Stats, error) {
	return e.st.Stats(ctx)
}

// Namespaces lists the known namespaces.
func (e *Explorer) Namespaces(ctx context.Context) ([]store.Namespace, error) {
	return e.st.ListNamespaces(ctx)
}

// List returns memories matching f.
func (e *Explorer) List(ctx context.Context, f store.ListFilter) ([]store.Memory, error) {
	return e.st.List(ctx, f)
}

// Search runs a full-text (or semantic, when configured) search.
func (e *Explorer) Search(ctx context.Context, q store.SearchQuery) ([]store.SearchResult, error) {
	return e.search.Search(ctx, q)
}

// Detail returns a memory with its history and links.
func (e *Explorer) Detail(ctx context.Context, id int64) (Detail, error) {
	m, err := e.st.Get(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	hist, err := e.st.History(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	links, err := e.st.Links(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	return Detail{Memory: m, History: hist, Links: links}, nil
}

// Create stores a new memory.
func (e *Explorer) Create(ctx context.Context, r CreateRequest) (store.Memory, error) {
	exp, err := ParseExpiration(r.ExpiresIn, r.ExpiresAt)
	if err != nil {
		return store.Memory{}, err
	}
	var links []store.LinkRef
	for _, id := range r.RelatedTo {
		links = append(links, store.LinkRef{TargetID: id, Kind: store.LinkRelatesTo})
	}
	for _, id := range r.DependsOn {
		links = append(links, store.LinkRef{TargetID: id, Kind: store.LinkDependsOn})
	}
	return e.st.Create(ctx, store.NewMemory{
		Namespace:  r.Namespace,
		Kind:       r.Kind,
		Title:      r.Title,
		Content:    r.Content,
		Tags:       r.Tags,
		Importance: r.Importance,
		Source:     e.actor,
		Links:      links,
		ExpiresAt:  exp,
	})
}

// Update modifies a memory.
func (e *Explorer) Update(ctx context.Context, id int64, r UpdateRequest) (store.Memory, error) {
	exp, err := ParseExpiration(r.ExpiresIn, r.ExpiresAt)
	if err != nil {
		return store.Memory{}, err
	}
	return e.st.Update(ctx, id, store.Patch{
		Namespace:      r.Namespace,
		Kind:           r.Kind,
		Title:          r.Title,
		Content:        r.Content,
		Tags:           r.Tags,
		Importance:     r.Importance,
		ExpiresAt:      exp,
		ClearExpiresAt: r.ClearExpires,
	}, e.actor)
}

// Archive archives a memory, recording reason in its history.
func (e *Explorer) Archive(ctx context.Context, id int64, reason string) (store.Memory, error) {
	return e.st.Archive(ctx, id, e.actor, reason)
}

// Restore un-archives a memory.
func (e *Explorer) Restore(ctx context.Context, id int64) (store.Memory, error) {
	return e.st.Restore(ctx, id, e.actor)
}

// Supersede makes memory id supersede targetID.
func (e *Explorer) Supersede(ctx context.Context, id, targetID int64) (store.Memory, error) {
	return e.st.Supersede(ctx, id, targetID, e.actor)
}

// AddLink adds a link from id to targetID and returns the updated links of id.
func (e *Explorer) AddLink(ctx context.Context, id, targetID int64, kind store.LinkKind) ([]store.LinkView, error) {
	if err := e.st.AddLink(ctx, id, targetID, kind, e.actor); err != nil {
		return nil, err
	}
	return e.st.Links(ctx, id)
}

// RemoveLink removes a link from id to targetID and returns the updated links of id.
func (e *Explorer) RemoveLink(ctx context.Context, id, targetID int64, kind store.LinkKind) ([]store.LinkView, error) {
	if err := e.st.RemoveLink(ctx, id, targetID, kind, e.actor); err != nil {
		return nil, err
	}
	return e.st.Links(ctx, id)
}

// SetNamespaceDescription creates the namespace if needed and sets its
// description. It returns the normalized name and the trimmed description.
func (e *Explorer) SetNamespaceDescription(ctx context.Context, name, description string) (string, string, error) {
	norm, err := e.st.RequireNamespace(ctx, name)
	if err != nil {
		return "", "", err
	}
	description = strings.TrimSpace(description)
	if _, err := e.st.EnsureNamespace(ctx, norm, description); err != nil {
		return "", "", err
	}
	return norm, description, nil
}

// RenameNamespace renames a namespace and returns its normalized new name.
func (e *Explorer) RenameNamespace(ctx context.Context, name, newName string) (string, error) {
	return e.st.RenameNamespace(ctx, name, newName, e.actor)
}

// MergeNamespace moves every memory of name into target and returns the count.
func (e *Explorer) MergeNamespace(ctx context.Context, name, target string) (int64, error) {
	return e.st.MergeNamespace(ctx, name, target, e.actor)
}

// ArchiveNamespace archives every active memory of a namespace.
func (e *Explorer) ArchiveNamespace(ctx context.Context, name, reason string) (int64, error) {
	return e.st.ArchiveNamespace(ctx, name, e.actor, reason)
}

// GC archives expired memories and returns how many were archived.
func (e *Explorer) GC(ctx context.Context) (int64, error) {
	return e.st.ArchiveExpired(ctx, time.Now().UTC(), e.actor)
}

// Similar returns memories similar to id: full-text matches, merged with
// semantic matches when the Searcher supports them.
func (e *Explorer) Similar(ctx context.Context, id int64) ([]store.Similar, error) {
	m, err := e.st.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	q := strings.TrimSpace(m.Title + " " + m.Content)
	sims, err := e.st.FindSimilar(ctx, q, m.Namespace, similarLimit, id)
	if err != nil {
		return nil, err
	}
	if sf, ok := e.search.(similarSearcher); ok {
		if sems, serr := sf.FindSimilar(ctx, q, m.Namespace, 0, similarLimit); serr == nil {
			filtered := sems[:0]
			for _, s := range sems {
				if s.ID != id {
					filtered = append(filtered, s)
				}
			}
			sims = MergeSimilar(sims, filtered, similarLimit)
		}
	}
	return sims, nil
}

// MergeSimilar merges full-text and semantic matches, keeping the best score
// per id, sorted by decreasing similarity then id, truncated to limit.
func MergeSimilar(fts, sem []store.Similar, limit int) []store.Similar {
	if len(sem) == 0 {
		return fts
	}
	byID := make(map[int64]store.Similar, len(fts)+len(sem))
	for _, s := range fts {
		byID[s.ID] = s
	}
	for _, s := range sem {
		if cur, ok := byID[s.ID]; !ok || s.Similarity > cur.Similarity {
			byID[s.ID] = s
		}
	}
	out := make([]store.Similar, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b store.Similar) int {
		if a.Similarity > b.Similarity {
			return -1
		}
		if a.Similarity < b.Similarity {
			return 1
		}
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
