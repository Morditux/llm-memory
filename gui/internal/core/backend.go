// Package core holds the GUI state and controller. It has no dependency on
// the widget toolkit so it can be tested anywhere.
package core

import (
	"context"

	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

// Backend is the data source of the GUI. *explorer.Explorer implements it;
// other implementations (an HTTP client, a test double) can be substituted.
type Backend interface {
	Stats(ctx context.Context) (store.Stats, error)
	Namespaces(ctx context.Context) ([]store.Namespace, error)
	List(ctx context.Context, f store.ListFilter) ([]store.Memory, error)
	Search(ctx context.Context, q store.SearchQuery) ([]store.SearchResult, error)
	Detail(ctx context.Context, id int64) (explorer.Detail, error)
	Create(ctx context.Context, r explorer.CreateRequest) (store.Memory, error)
	Update(ctx context.Context, id int64, r explorer.UpdateRequest) (store.Memory, error)
	Archive(ctx context.Context, id int64, reason string) (store.Memory, error)
	Restore(ctx context.Context, id int64) (store.Memory, error)
	Supersede(ctx context.Context, id, targetID int64) (store.Memory, error)
	AddLink(ctx context.Context, id, targetID int64, kind store.LinkKind) ([]store.LinkView, error)
	RemoveLink(ctx context.Context, id, targetID int64, kind store.LinkKind) ([]store.LinkView, error)
	SetNamespaceDescription(ctx context.Context, name, description string) (string, string, error)
	RenameNamespace(ctx context.Context, name, newName string) (string, error)
	MergeNamespace(ctx context.Context, name, target string) (int64, error)
	ArchiveNamespace(ctx context.Context, name, reason string) (int64, error)
	GC(ctx context.Context) (int64, error)
	Similar(ctx context.Context, id int64) ([]store.Similar, error)
}

var _ Backend = (*explorer.Explorer)(nil)
