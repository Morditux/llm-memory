package core

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

// fakeClock is a manual AfterFunc.
type fakeClock struct {
	mu      sync.Mutex
	timers  []*fakeTimer
	started int
}

type fakeTimer struct {
	f         func()
	cancelled bool
}

func (c *fakeClock) after(_ time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{f: f}
	c.timers = append(c.timers, t)
	c.started++
	return func() bool { c.mu.Lock(); defer c.mu.Unlock(); t.cancelled = true; return true }
}

// fire runs every non-cancelled timer once.
func (c *fakeClock) fire() {
	c.mu.Lock()
	ts := c.timers
	c.timers = nil
	c.mu.Unlock()
	for _, t := range ts {
		c.mu.Lock()
		skip := t.cancelled
		c.mu.Unlock()
		if !skip {
			t.f()
		}
	}
}

func newListController(fb *FakeBackend, obs Observer) (*Controller, *fakeClock) {
	c := NewController(fb, obs)
	clk := &fakeClock{}
	c.after = clk.after
	return c, clk
}

func mems(ids ...int64) []store.Memory {
	out := make([]store.Memory, len(ids))
	for i, id := range ids {
		out[i] = store.Memory{ID: id}
	}
	return out
}

func TestListFilterMapping(t *testing.T) {
	f := Filters{Namespace: "demo", Tags: []string{"a", "b"}, Kind: store.KindFact, Archived: store.ArchivedOnly, Offset: 100}
	got := f.listFilter()
	want := store.ListFilter{Namespace: "demo", Tags: []string{"a", "b"}, Kinds: []store.Kind{store.KindFact}, Archived: store.ArchivedOnly, Limit: 50, Offset: 100}
	if !slices.Equal(got.Tags, want.Tags) || !slices.Equal(got.Kinds, want.Kinds) ||
		got.Namespace != want.Namespace || got.Archived != want.Archived || got.Limit != 50 || got.Offset != 100 {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if k := (Filters{}).listFilter().Kinds; k != nil {
		t.Fatalf("empty kind must give nil Kinds, got %v", k)
	}
	if a := (Filters{Archived: store.IncludeArchived}).listFilter().Archived; a != store.IncludeArchived {
		t.Fatalf("archived = %v", a)
	}
}

func TestSearchQueryMapping(t *testing.T) {
	cases := []struct {
		a    store.ArchiveFilter
		want bool
	}{{store.ActiveOnly, false}, {store.ArchivedOnly, true}, {store.IncludeArchived, true}}
	for _, tc := range cases {
		q := Filters{Query: "x", Namespace: "n", Tags: []string{"t"}, Kind: store.KindNote, Archived: tc.a, Offset: 50}.searchQuery()
		if q.IncludeArchived != tc.want || q.Query != "x" || q.Namespace != "n" || q.Limit != 50 ||
			!slices.Equal(q.Tags, []string{"t"}) || !slices.Equal(q.Kinds, []store.Kind{store.KindNote}) {
			t.Fatalf("archived %v: %+v", tc.a, q)
		}
	}
}

func TestParseTags(t *testing.T) {
	if ParseTags("  ") != nil {
		t.Fatal("blank must give nil")
	}
	if got := ParseTags(" Foo, #bar ,foo,, "); !slices.Equal(got, []string{"foo", "bar"}) {
		t.Fatalf("got %v", got)
	}
}

func TestLoadListUsesListOrSearch(t *testing.T) {
	fb := &FakeBackend{ListValue: mems(1, 2), SearchValue: []store.SearchResult{{Memory: store.Memory{ID: 9}}}}
	var res ListResult
	c, _ := newListController(fb, Observer{List: func(r ListResult) { res = r }})
	ctx := context.Background()

	if err := c.LoadList(ctx); err != nil {
		t.Fatal(err)
	}
	if res.Search || len(res.Items) != 2 || fb.Calls("Search") != 0 {
		t.Fatalf("res = %+v", res)
	}
	c.SetQuery(ctx, "  hello ")
	if err := c.LoadList(ctx); err != nil {
		t.Fatal(err)
	}
	if !res.Search || res.PagerVisible || len(res.Items) != 1 || res.Items[0].ID != 9 {
		t.Fatalf("res = %+v", res)
	}
	if q := fb.SearchCalls()[0]; q.Query != "hello" || q.Limit != PageSize {
		t.Fatalf("query = %+v", q)
	}
}

func TestPagination(t *testing.T) {
	full := make([]store.Memory, PageSize)
	fb := &FakeBackend{ListValue: full}
	var res ListResult
	c, _ := newListController(fb, Observer{List: func(r ListResult) { res = r }})
	ctx := context.Background()

	_ = c.LoadList(ctx)
	if !res.PagerVisible || res.PrevEnabled || !res.NextEnabled || res.PageLabel != "Page 1 (50 shown)" {
		t.Fatalf("page 1: %+v", res)
	}
	c.NextPage()
	fb.ListValue = full[:7]
	_ = c.LoadList(ctx)
	if got := fb.ListCalls()[1].Offset; got != 50 {
		t.Fatalf("offset = %d", got)
	}
	if !res.PagerVisible || !res.PrevEnabled || res.NextEnabled || res.PageLabel != "Page 2 (7 shown)" {
		t.Fatalf("page 2: %+v", res)
	}
	c.PrevPage()
	c.PrevPage()
	if c.Filters().Offset != 0 {
		t.Fatal("offset must not go below 0")
	}
	fb.ListValue = full[:3]
	_ = c.LoadList(ctx)
	if res.PagerVisible || res.PageLabel != "Page 1 (3 shown)" {
		t.Fatalf("short first page: %+v", res)
	}
}

func TestFilterChangesResetOffset(t *testing.T) {
	c, _ := newListController(&FakeBackend{}, Observer{})
	ctx := context.Background()
	steps := map[string]func(){
		"namespace": func() { c.SelectNamespace("x") },
		"query":     func() { c.SetQuery(ctx, "q") },
		"tags":      func() { c.SetTags(ctx, "a") },
		"kind":      func() { c.SetKind(store.KindNote) },
		"archived":  func() { c.SetArchived(store.IncludeArchived) },
		"tag click": func() { c.SelectTag("z") },
	}
	for name, step := range steps {
		c.SetOffset(150)
		step()
		if c.Filters().Offset != 0 {
			t.Errorf("%s did not reset the offset", name)
		}
	}
}

func TestDebounce(t *testing.T) {
	fb := &FakeBackend{}
	c, clk := newListController(fb, Observer{})
	ctx := context.Background()

	c.SetQuery(ctx, "h")
	c.SetQuery(ctx, "he")
	c.SetTags(ctx, "a")
	if n := fb.Calls("List") + fb.Calls("Search"); n != 0 {
		t.Fatalf("%d loads before the delay", n)
	}
	clk.fire()
	if n := fb.Calls("Search"); n != 1 || fb.Calls("List") != 0 {
		t.Fatalf("search = %d, list = %d", n, fb.Calls("List"))
	}
	q := fb.SearchCalls()[0]
	if q.Query != "he" || !slices.Equal(q.Tags, []string{"a"}) {
		t.Fatalf("query = %+v", q)
	}
}

func TestImmediateLoadCancelsDebounce(t *testing.T) {
	fb := &FakeBackend{}
	c, clk := newListController(fb, Observer{})
	ctx := context.Background()
	c.SetTags(ctx, "a")
	c.SetKind(store.KindFact)
	_ = c.LoadList(ctx)
	clk.fire()
	if n := fb.Calls("List"); n != 1 {
		t.Fatalf("List called %d times, want 1", n)
	}
}

func TestStaleListDropped(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	fb := &FakeBackend{}
	var first sync.Once
	fb.ListFunc = func(f store.ListFilter) ([]store.Memory, error) {
		blocked := false
		first.Do(func() { blocked = true })
		if blocked {
			close(entered)
			<-release
			return mems(1), nil
		}
		return mems(2), nil
	}
	var mu sync.Mutex
	var seen []int64
	c, _ := newListController(fb, Observer{List: func(r ListResult) {
		mu.Lock()
		seen = append(seen, r.Items[0].ID)
		mu.Unlock()
	}})
	done := make(chan struct{})
	go func() { _ = c.LoadList(context.Background()); close(done) }()
	<-entered
	_ = c.LoadList(context.Background())
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []int64{2}) {
		t.Fatalf("seen = %v, want [2]", seen)
	}
}

func TestStaleListErrorDropped(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	fb := &FakeBackend{}
	var first sync.Once
	fb.ListFunc = func(store.ListFilter) ([]store.Memory, error) {
		blocked := false
		first.Do(func() { blocked = true })
		if blocked {
			close(entered)
			<-release
			return nil, errors.New("old failure")
		}
		return mems(2), nil
	}
	var errs []error
	c, _ := newListController(fb, Observer{Error: func(err error) { errs = append(errs, err) }})
	done := make(chan struct{})
	go func() { _ = c.LoadList(context.Background()); close(done) }()
	<-entered
	_ = c.LoadList(context.Background())
	close(release)
	<-done
	if len(errs) != 0 {
		t.Fatalf("stale error reported: %v", errs)
	}
}

func TestListErrorReported(t *testing.T) {
	boom := errors.New("boom")
	var got error
	c, _ := newListController(&FakeBackend{Err: boom}, Observer{Error: func(err error) { got = err }})
	if err := c.LoadList(context.Background()); !errors.Is(err, boom) || !errors.Is(got, boom) {
		t.Fatalf("err = %v, observer = %v", err, got)
	}
}

func TestSelectTagFiltersAndReloads(t *testing.T) {
	fb := &FakeBackend{}
	c, _ := newListController(fb, Observer{})
	c.SetOffset(100)
	c.SelectTag("Go")
	_ = c.LoadList(context.Background())
	lf := fb.ListCalls()[0]
	if !slices.Equal(lf.Tags, []string{"go"}) || lf.Offset != 0 {
		t.Fatalf("filter = %+v", lf)
	}
}

func snap(s string) store.HistoryEntry { return store.HistoryEntry{Snapshot: s} }

func TestDetailHistoryDiff(t *testing.T) {
	fb := &FakeBackend{DetailValues: map[int64]explorer.Detail{
		7: {
			Memory:  store.Memory{ID: 7},
			History: []store.HistoryEntry{snap("a\nb"), snap("a\nc")},
			Links:   []store.LinkView{{ID: 3}},
		},
	}}
	var views []DetailView
	c, _ := newListController(fb, Observer{Detail: func(v DetailView) { views = append(views, v) }})
	if !c.DiffMode() {
		t.Fatal("diff mode must be on by default")
	}
	if err := c.LoadDetail(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if c.SelectedID() != 7 {
		t.Fatal("selection not recorded")
	}
	v := views[0]
	if !v.DiffMode || len(v.Links) != 1 {
		t.Fatalf("view = %+v", v)
	}
	first := []explorer.DiffLine{{Op: explorer.Del, Text: ""}, {Op: explorer.Add, Text: "a"}, {Op: explorer.Add, Text: "b"}}
	if !slices.Equal(v.History[0].Diff, first) {
		t.Fatalf("first diff = %v", v.History[0].Diff)
	}
	second := explorer.LineDiff("a\nb", "a\nc")
	if !slices.Equal(v.History[1].Diff, second) {
		t.Fatalf("second diff = %v, want %v", v.History[1].Diff, second)
	}

	c.SetDiffMode(false)
	raw := views[1]
	if raw.DiffMode || raw.History[0].Diff != nil || raw.History[1].Snapshot != "a\nc" {
		t.Fatalf("raw view = %+v", raw)
	}
	if fb.Calls("Detail") != 1 {
		t.Fatal("toggling the diff must not refetch")
	}
}

func TestSetDiffModeWithoutDetail(t *testing.T) {
	called := false
	c, _ := newListController(&FakeBackend{}, Observer{Detail: func(DetailView) { called = true }})
	c.SetDiffMode(false)
	if called || c.DiffMode() {
		t.Fatalf("called = %v, diff = %v", called, c.DiffMode())
	}
}

func TestStaleDetailDropped(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	fb := &FakeBackend{}
	fb.DetailFunc = func(id int64) (explorer.Detail, error) {
		blocked := false
		first.Do(func() { blocked = true })
		if blocked {
			close(entered)
			<-release
		}
		return explorer.Detail{Memory: store.Memory{ID: id}}, nil
	}
	var mu sync.Mutex
	var seen []int64
	c, _ := newListController(fb, Observer{Detail: func(v DetailView) { mu.Lock(); seen = append(seen, v.Memory.ID); mu.Unlock() }})
	done := make(chan struct{})
	go func() { _ = c.LoadDetail(context.Background(), 1); close(done) }()
	<-entered
	_ = c.LoadDetail(context.Background(), 2)
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []int64{2}) || c.SelectedID() != 2 {
		t.Fatalf("seen = %v, selected = %d", seen, c.SelectedID())
	}
}

func TestDetailNotFound(t *testing.T) {
	var got error
	c, _ := newListController(&FakeBackend{}, Observer{Error: func(err error) { got = err }})
	if err := c.LoadDetail(context.Background(), 5); err == nil || got == nil {
		t.Fatalf("err = %v, observer = %v", err, got)
	}
}
