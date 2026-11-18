package core

import (
	"context"
	"sync"

	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

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
	// Errs maps a method name ("Create", "GC", "Similar"…) to the error that
	// method returns, in addition to Err which fails every method.
	Errs map[string]error
	Hook func(method string)

	// CreateValue, GCValue, LinksValue and SimilarValue are the results of
	// Create, GC, AddLink/RemoveLink and Similar.
	CreateValue  store.Memory
	GCValue      int64
	LinksValue   []store.LinkView
	SimilarValue []store.Similar
	// RenameValue is the name RenameNamespace returns (default: the new
	// name), MergeValue and ArchiveNamespaceValue the counts returned by
	// MergeNamespace and ArchiveNamespace.
	RenameValue           string
	MergeValue            int64
	ArchiveNamespaceValue int64

	mu        sync.Mutex
	calls     map[string]int
	lists     []store.ListFilter
	searches  []store.SearchQuery
	mutations []MutationCall
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

// MutationCall records one call of a mutating FakeBackend method. Only the
// fields relevant to Method are set.
type MutationCall struct {
	Method   string
	ID       int64
	TargetID int64
	Kind     store.LinkKind
	Reason   string
	// Name is the namespace acted on; NewName the new name or merge target.
	Name, NewName, Description string
	Create                     explorer.CreateRequest
	Update                     explorer.UpdateRequest
}

// MutationCalls returns every mutating call, in order.
func (f *FakeBackend) MutationCalls() []MutationCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MutationCall(nil), f.mutations...)
}

// mutate records c and returns the error configured for its method.
func (f *FakeBackend) mutate(c MutationCall) error {
	f.mu.Lock()
	f.mutations = append(f.mutations, c)
	f.mu.Unlock()
	if err := f.enter(c.Method); err != nil {
		return err
	}
	return f.Errs[c.Method]
}

func (f *FakeBackend) memory(id int64) store.Memory {
	if m, ok := f.DetailValues[id]; ok {
		return m.Memory
	}
	return store.Memory{ID: id}
}

// Create implements Backend. It returns CreateValue.
func (f *FakeBackend) Create(_ context.Context, r explorer.CreateRequest) (store.Memory, error) {
	if err := f.mutate(MutationCall{Method: "Create", Create: r}); err != nil {
		return store.Memory{}, err
	}
	return f.CreateValue, nil
}

// Update implements Backend.
func (f *FakeBackend) Update(_ context.Context, id int64, r explorer.UpdateRequest) (store.Memory, error) {
	if err := f.mutate(MutationCall{Method: "Update", ID: id, Update: r}); err != nil {
		return store.Memory{}, err
	}
	return f.memory(id), nil
}

// Archive implements Backend.
func (f *FakeBackend) Archive(_ context.Context, id int64, reason string) (store.Memory, error) {
	if err := f.mutate(MutationCall{Method: "Archive", ID: id, Reason: reason}); err != nil {
		return store.Memory{}, err
	}
	return f.memory(id), nil
}

// Restore implements Backend.
func (f *FakeBackend) Restore(_ context.Context, id int64) (store.Memory, error) {
	if err := f.mutate(MutationCall{Method: "Restore", ID: id}); err != nil {
		return store.Memory{}, err
	}
	return f.memory(id), nil
}

// Supersede implements Backend.
func (f *FakeBackend) Supersede(_ context.Context, id, targetID int64) (store.Memory, error) {
	if err := f.mutate(MutationCall{Method: "Supersede", ID: id, TargetID: targetID}); err != nil {
		return store.Memory{}, err
	}
	return f.memory(id), nil
}

// AddLink implements Backend. It returns LinksValue.
func (f *FakeBackend) AddLink(_ context.Context, id, targetID int64, kind store.LinkKind) ([]store.LinkView, error) {
	if err := f.mutate(MutationCall{Method: "AddLink", ID: id, TargetID: targetID, Kind: kind}); err != nil {
		return nil, err
	}
	return f.LinksValue, nil
}

// RemoveLink implements Backend. It returns LinksValue.
func (f *FakeBackend) RemoveLink(_ context.Context, id, targetID int64, kind store.LinkKind) ([]store.LinkView, error) {
	if err := f.mutate(MutationCall{Method: "RemoveLink", ID: id, TargetID: targetID, Kind: kind}); err != nil {
		return nil, err
	}
	return f.LinksValue, nil
}

// SetNamespaceDescription implements Backend. It returns the name and
// description it was given.
func (f *FakeBackend) SetNamespaceDescription(_ context.Context, name, description string) (string, string, error) {
	if err := f.mutate(MutationCall{Method: "SetNamespaceDescription", Name: name, Description: description}); err != nil {
		return "", "", err
	}
	return name, description, nil
}

// RenameNamespace implements Backend. It returns RenameValue, or newName when
// RenameValue is empty.
func (f *FakeBackend) RenameNamespace(_ context.Context, name, newName string) (string, error) {
	if err := f.mutate(MutationCall{Method: "RenameNamespace", Name: name, NewName: newName}); err != nil {
		return "", err
	}
	if f.RenameValue != "" {
		return f.RenameValue, nil
	}
	return newName, nil
}

// MergeNamespace implements Backend. It returns MergeValue.
func (f *FakeBackend) MergeNamespace(_ context.Context, name, target string) (int64, error) {
	if err := f.mutate(MutationCall{Method: "MergeNamespace", Name: name, NewName: target}); err != nil {
		return 0, err
	}
	return f.MergeValue, nil
}

// ArchiveNamespace implements Backend. It returns ArchiveNamespaceValue.
func (f *FakeBackend) ArchiveNamespace(_ context.Context, name, reason string) (int64, error) {
	if err := f.mutate(MutationCall{Method: "ArchiveNamespace", Name: name, Reason: reason}); err != nil {
		return 0, err
	}
	return f.ArchiveNamespaceValue, nil
}

// GC implements Backend. It returns GCValue.
func (f *FakeBackend) GC(context.Context) (int64, error) {
	if err := f.mutate(MutationCall{Method: "GC"}); err != nil {
		return 0, err
	}
	return f.GCValue, nil
}

// Similar implements Backend. It returns SimilarValue.
func (f *FakeBackend) Similar(_ context.Context, id int64) ([]store.Similar, error) {
	if err := f.enter("Similar"); err != nil {
		return nil, err
	}
	if err := f.Errs["Similar"]; err != nil {
		return nil, err
	}
	return f.SimilarValue, nil
}
