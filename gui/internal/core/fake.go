package core

import (
	"context"
	"errors"
	"sync"

	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

// ErrNotImplemented is returned by the FakeBackend methods that are not
// needed yet.
var ErrNotImplemented = errors.New("fake backend: not implemented")

// FakeBackend is an in-memory Backend for tests, here and in the view package.
// Set the exported fields before use; Hook, when set, runs at the start of
// every implemented method so tests can block or delay calls. ListFunc,
// SearchFunc and DetailFunc, when set, replace the static values.
type FakeBackend struct {
	StatsValue      store.Stats
	NamespacesValue []store.Namespace
	ListValue       []store.Memory
	SearchValue     []store.SearchResult
	// DetailValues maps a memory id to its detail; unknown ids yield a
	// *store.NotFoundError.
	DetailValues map[int64]explorer.Detail
	ListFunc     func(store.ListFilter) ([]store.Memory, error)
	SearchFunc   func(store.SearchQuery) ([]store.SearchResult, error)
	DetailFunc   func(id int64) (explorer.Detail, error)
	Err          error
	Hook         func(method string)

	mu       sync.Mutex
	calls    map[string]int
	lists    []store.ListFilter
	searches []store.SearchQuery
}

// ListCalls returns the filters of every List call, in order.
func (f *FakeBackend) ListCalls() []store.ListFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ListFilter(nil), f.lists...)
}

// SearchCalls returns the queries of every Search call, in order.
func (f *FakeBackend) SearchCalls() []store.SearchQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.SearchQuery(nil), f.searches...)
}

// Calls returns how many times method was invoked.
func (f *FakeBackend) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *FakeBackend) enter(method string) error {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[method]++
	f.mu.Unlock()
	if f.Hook != nil {
		f.Hook(method)
	}
	return f.Err
}

// Stats implements Backend.
func (f *FakeBackend) Stats(context.Context) (store.Stats, error) {
	if err := f.enter("Stats"); err != nil {
		return store.Stats{}, err
	}
	return f.StatsValue, nil
}

// Namespaces implements Backend.
func (f *FakeBackend) Namespaces(context.Context) ([]store.Namespace, error) {
	if err := f.enter("Namespaces"); err != nil {
		return nil, err
	}
	return f.NamespacesValue, nil
}

// List implements Backend.
func (f *FakeBackend) List(_ context.Context, lf store.ListFilter) ([]store.Memory, error) {
	f.mu.Lock()
	f.lists = append(f.lists, lf)
	f.mu.Unlock()
	if err := f.enter("List"); err != nil {
		return nil, err
	}
	if f.ListFunc != nil {
		return f.ListFunc(lf)
	}
	return f.ListValue, nil
}

// Search implements Backend.
func (f *FakeBackend) Search(_ context.Context, q store.SearchQuery) ([]store.SearchResult, error) {
	f.mu.Lock()
	f.searches = append(f.searches, q)
	f.mu.Unlock()
	if err := f.enter("Search"); err != nil {
		return nil, err
	}
	if f.SearchFunc != nil {
		return f.SearchFunc(q)
	}
	return f.SearchValue, nil
}

// Detail implements Backend.
func (f *FakeBackend) Detail(_ context.Context, id int64) (explorer.Detail, error) {
	if err := f.enter("Detail"); err != nil {
		return explorer.Detail{}, err
	}
	if f.DetailFunc != nil {
		return f.DetailFunc(id)
	}
	d, ok := f.DetailValues[id]
	if !ok {
		return explorer.Detail{}, &store.NotFoundError{ID: id}
	}
	return d, nil
}

// Create implements Backend.
func (f *FakeBackend) Create(context.Context, explorer.CreateRequest) (store.Memory, error) {
	return store.Memory{}, ErrNotImplemented
}

// Update implements Backend.
func (f *FakeBackend) Update(context.Context, int64, explorer.UpdateRequest) (store.Memory, error) {
	return store.Memory{}, ErrNotImplemented
}

// Archive implements Backend.
func (f *FakeBackend) Archive(context.Context, int64, string) (store.Memory, error) {
	return store.Memory{}, ErrNotImplemented
}

// Restore implements Backend.
func (f *FakeBackend) Restore(context.Context, int64) (store.Memory, error) {
	return store.Memory{}, ErrNotImplemented
}

// Supersede implements Backend.
func (f *FakeBackend) Supersede(context.Context, int64, int64) (store.Memory, error) {
	return store.Memory{}, ErrNotImplemented
}

// AddLink implements Backend.
func (f *FakeBackend) AddLink(context.Context, int64, int64, store.LinkKind) ([]store.LinkView, error) {
	return nil, ErrNotImplemented
}

// RemoveLink implements Backend.
func (f *FakeBackend) RemoveLink(context.Context, int64, int64, store.LinkKind) ([]store.LinkView, error) {
	return nil, ErrNotImplemented
}

// SetNamespaceDescription implements Backend.
func (f *FakeBackend) SetNamespaceDescription(context.Context, string, string) (string, string, error) {
	return "", "", ErrNotImplemented
}

// RenameNamespace implements Backend.
func (f *FakeBackend) RenameNamespace(context.Context, string, string) (string, error) {
	return "", ErrNotImplemented
}

// MergeNamespace implements Backend.
func (f *FakeBackend) MergeNamespace(context.Context, string, string) (int64, error) {
	return 0, ErrNotImplemented
}

// ArchiveNamespace implements Backend.
func (f *FakeBackend) ArchiveNamespace(context.Context, string, string) (int64, error) {
	return 0, ErrNotImplemented
}

// GC implements Backend.
func (f *FakeBackend) GC(context.Context) (int64, error) { return 0, ErrNotImplemented }

// Similar implements Backend.
func (f *FakeBackend) Similar(context.Context, int64) ([]store.Similar, error) {
	return nil, ErrNotImplemented
}
