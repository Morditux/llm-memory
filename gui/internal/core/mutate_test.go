package core

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

type banner struct {
	Msg   string
	IsErr bool
}

// recorder collects the observer events of a controller.
type recorder struct {
	mu       sync.Mutex
	banners  []banner
	details  []DetailView
	similars []SimilarView
	errs     []error
	counts   map[string]int
}

func (r *recorder) count(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[k]++
}

func (r *recorder) observer() Observer {
	return Observer{
		Stats:      func(store.Stats) { r.count("stats") },
		Namespaces: func([]store.Namespace) { r.count("namespaces") },
		List:       func(ListResult) { r.count("list") },
		Detail: func(v DetailView) {
			r.count("detail")
			r.mu.Lock()
			r.details = append(r.details, v)
			r.mu.Unlock()
		},
		Similar: func(v SimilarView) {
			r.mu.Lock()
			r.similars = append(r.similars, v)
			r.mu.Unlock()
		},
		Banner: func(msg string, isErr bool) {
			r.mu.Lock()
			r.banners = append(r.banners, banner{msg, isErr})
			r.mu.Unlock()
		},
		Error: func(err error) {
			r.mu.Lock()
			r.errs = append(r.errs, err)
			r.mu.Unlock()
		},
	}
}

func mutFake() *FakeBackend {
	m1 := store.Memory{ID: 1, Namespace: "demo", Kind: store.KindFact, Title: "One", Content: "body"}
	m2 := store.Memory{ID: 2, Namespace: "demo", Kind: store.KindNote}
	return &FakeBackend{
		DetailValues: map[int64]explorer.Detail{1: {Memory: m1}, 2: {Memory: m2}, 7: {Memory: store.Memory{ID: 7}}},
		CreateValue:  store.Memory{ID: 7},
		GCValue:      3,
	}
}

func setup(fb *FakeBackend) (*Controller, *recorder) {
	r := &recorder{}
	return NewController(fb, r.observer()), r
}

// assertRefreshed checks that a refresh(id) ran want times.
func assertRefreshed(t *testing.T, fb *FakeBackend, r *recorder, want int) {
	t.Helper()
	for _, m := range []string{"Stats", "Namespaces", "List", "Detail"} {
		if got := fb.Calls(m); got != want {
			t.Errorf("%s calls = %d, want %d", m, got, want)
		}
	}
	for _, k := range []string{"stats", "namespaces", "list", "detail"} {
		if r.counts[k] != want {
			t.Errorf("%s notifications = %d, want %d", k, r.counts[k], want)
		}
	}
}

func TestCreateFormRequest(t *testing.T) {
	got := CreateForm{
		Namespace: "  demo ", Kind: "decision", Importance: 3, Title: "  T  ",
		Tags: " a, ,b ,, c", ExpiresIn: " 7d ", Content: "  keep  ",
	}.Request()
	want := explorer.CreateRequest{
		Namespace: "demo", Kind: store.KindDecision, Importance: 3, Title: "T",
		Tags: []string{"a", "b", "c"}, ExpiresIn: "7d", Content: "  keep  ",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if ns := (CreateForm{Namespace: "   "}).Request().Namespace; ns != "global" {
		t.Fatalf("blank namespace = %q", ns)
	}
	if tags := (CreateForm{}).Request().Tags; len(tags) != 0 {
		t.Fatalf("tags = %v", tags)
	}
}

func TestEditFormRequest(t *testing.T) {
	r := EditForm{
		Namespace: " ns ", Kind: "note", Importance: 1, Title: " Title ",
		Tags: "x, y", ExpiresIn: "  ", Content: "c",
	}.Request()
	if *r.Namespace != "ns" || *r.Kind != store.KindNote || *r.Importance != 1 ||
		*r.Title != " Title " || !slices.Equal(*r.Tags, []string{"x", "y"}) || *r.Content != "c" {
		t.Fatalf("request = %+v", r)
	}
	if r.ExpiresIn != "" || r.ClearExpires {
		t.Fatalf("expiry = %q, clear = %v", r.ExpiresIn, r.ClearExpires)
	}
	r = EditForm{ExpiresIn: " 24h ", ClearExpires: true}.Request()
	if r.ExpiresIn != "24h" || !r.ClearExpires {
		t.Fatalf("expiry = %q, clear = %v", r.ExpiresIn, r.ClearExpires)
	}
	if r.Tags == nil || len(*r.Tags) != 0 {
		t.Fatal("empty tags must be sent as an empty list")
	}
}

func TestParseTargetID(t *testing.T) {
	for in, ok := range map[string]bool{"12": true, " 3 ": true, "": false, "abc": false, "0": false, "-4": false, "1.5": false} {
		if _, got := ParseTargetID(in); got != ok {
			t.Errorf("ParseTargetID(%q) ok = %v, want %v", in, got, ok)
		}
	}
}

func TestInvalidExpirationRejected(t *testing.T) {
	for _, in := range []string{"abc", "0d", "-1h", "1.5d"} {
		if _, err := explorer.ParseExpiration(in, ""); err == nil {
			t.Errorf("ParseExpiration(%q) accepted", in)
		}
	}
	var verr *store.ValidationError
	_, err := explorer.ParseExpiration("soon", "")
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), "soon") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateSelectsNewMemory(t *testing.T) {
	fb := mutFake()
	c, r := setup(fb)
	req := CreateForm{Namespace: "demo", Kind: "fact", Importance: 2, Content: "x"}.Request()
	m, err := c.Create(context.Background(), req)
	if err != nil || m.ID != 7 {
		t.Fatalf("m = %+v, err = %v", m, err)
	}
	calls := fb.MutationCalls()
	if len(calls) != 1 || calls[0].Method != "Create" || !reflect.DeepEqual(calls[0].Create, req) {
		t.Fatalf("calls = %+v", calls)
	}
	if want := []banner{{"Created memory #7", false}}; !slices.Equal(r.banners, want) {
		t.Fatalf("banners = %+v", r.banners)
	}
	assertRefreshed(t, fb, r, 1)
	if c.SelectedID() != 7 || r.details[0].Memory.ID != 7 {
		t.Fatalf("selected = %d, detail = %+v", c.SelectedID(), r.details)
	}
}

func TestMutationsRefreshAndAnnounce(t *testing.T) {
	ctx := context.Background()
	str := "x"
	tests := []struct {
		name   string
		run    func(c *Controller) error
		call   MutationCall
		banner string
		detail int64
	}{
		{"update", func(c *Controller) error { return c.Update(ctx, 1, explorer.UpdateRequest{Title: &str}) },
			MutationCall{Method: "Update", ID: 1, Update: explorer.UpdateRequest{Title: &str}}, "Updated memory #1", 1},
		{"archive", func(c *Controller) error { return c.Archive(ctx, 1) },
			MutationCall{Method: "Archive", ID: 1, Reason: "archived from desktop GUI"}, "Archived memory #1", 1},
		{"restore", func(c *Controller) error { return c.Restore(ctx, 1) },
			MutationCall{Method: "Restore", ID: 1}, "Restored memory #1", 1},
		{"supersede", func(c *Controller) error { return c.Supersede(ctx, 1, 2) },
			MutationCall{Method: "Supersede", ID: 1, TargetID: 2}, "Archived #2 (superseded by #1)", 1},
		{"gc", func(c *Controller) error { c.Select(2); return c.GC(ctx) },
			MutationCall{Method: "GC"}, "GC archived 3 expired memory(ies)", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fb := mutFake()
			c, r := setup(fb)
			if err := tt.run(c); err != nil {
				t.Fatal(err)
			}
			if got := fb.MutationCalls(); len(got) != 1 || !reflect.DeepEqual(got[0], tt.call) {
				t.Fatalf("calls = %+v, want %+v", got, tt.call)
			}
			if want := []banner{{tt.banner, false}}; !slices.Equal(r.banners, want) {
				t.Fatalf("banners = %+v", r.banners)
			}
			assertRefreshed(t, fb, r, 1)
			if r.details[0].Memory.ID != tt.detail {
				t.Fatalf("detail = %d, want %d", r.details[0].Memory.ID, tt.detail)
			}
		})
	}
}

func TestGCWithoutSelectionSkipsDetail(t *testing.T) {
	fb := mutFake()
	c, r := setup(fb)
	if err := c.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fb.Calls("Detail") != 0 || fb.Calls("List") != 1 || len(r.banners) != 1 {
		t.Fatalf("detail = %d, list = %d, banners = %v", fb.Calls("Detail"), fb.Calls("List"), r.banners)
	}
}

func TestLinksReloadOnlyDetail(t *testing.T) {
	ctx := context.Background()
	fb := mutFake()
	c, r := setup(fb)
	if err := c.AddLink(ctx, 1, 2, store.LinkDependsOn); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveLink(ctx, 1, 2, store.LinkDependsOn); err != nil {
		t.Fatal(err)
	}
	want := []MutationCall{
		{Method: "AddLink", ID: 1, TargetID: 2, Kind: store.LinkDependsOn},
		{Method: "RemoveLink", ID: 1, TargetID: 2, Kind: store.LinkDependsOn},
	}
	if got := fb.MutationCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %+v", got)
	}
	if fb.Calls("Detail") != 2 || fb.Calls("Stats")+fb.Calls("Namespaces")+fb.Calls("List") != 0 {
		t.Fatalf("detail = %d, others = %d", fb.Calls("Detail"), fb.Calls("Stats")+fb.Calls("Namespaces")+fb.Calls("List"))
	}
	if len(r.banners) != 0 {
		t.Fatalf("banners = %v", r.banners)
	}
}

func TestMutationErrorsReachBanner(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	runs := map[string]func(c *Controller) error{
		"Create":     func(c *Controller) error { _, err := c.Create(ctx, explorer.CreateRequest{}); return err },
		"Update":     func(c *Controller) error { return c.Update(ctx, 1, explorer.UpdateRequest{}) },
		"Archive":    func(c *Controller) error { return c.Archive(ctx, 1) },
		"Restore":    func(c *Controller) error { return c.Restore(ctx, 1) },
		"GC":         func(c *Controller) error { return c.GC(ctx) },
		"Supersede":  func(c *Controller) error { return c.Supersede(ctx, 1, 2) },
		"AddLink":    func(c *Controller) error { return c.AddLink(ctx, 1, 2, store.LinkRelatesTo) },
		"RemoveLink": func(c *Controller) error { return c.RemoveLink(ctx, 1, 2, store.LinkRelatesTo) },
	}
	for method, run := range runs {
		t.Run(method, func(t *testing.T) {
			fb := mutFake()
			fb.Errs = map[string]error{method: boom}
			c, r := setup(fb)
			if err := run(c); !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			if want := []banner{{"boom", true}}; !slices.Equal(r.banners, want) {
				t.Fatalf("banners = %+v", r.banners)
			}
			if n := fb.Calls("Stats") + fb.Calls("List") + fb.Calls("Detail"); n != 0 {
				t.Fatalf("%d reloads after a failed mutation", n)
			}
		})
	}
}

func TestEditMode(t *testing.T) {
	ctx := context.Background()
	fb := mutFake()
	fb.DetailValues[3] = explorer.Detail{Memory: store.Memory{ID: 3, ArchivedAt: new(time.Time)}}
	c, r := setup(fb)

	c.Edit() // nothing displayed
	if c.Editing() || len(r.details) != 0 {
		t.Fatal("edit mode without detail")
	}
	_ = c.LoadDetail(ctx, 3)
	c.Edit()
	if c.Editing() {
		t.Fatal("archived memories cannot be edited")
	}
	_ = c.LoadDetail(ctx, 1)
	c.Edit()
	if !c.Editing() || !r.details[len(r.details)-1].Editing {
		t.Fatal("not editing after Edit")
	}
	if fb.Calls("Detail") != 2 {
		t.Fatal("Edit must not refetch")
	}
	c.SetDiffMode(false)
	if !r.details[len(r.details)-1].Editing {
		t.Fatal("diff toggle must keep edit mode")
	}
	c.CancelEdit()
	if c.Editing() || r.details[len(r.details)-1].Editing {
		t.Fatal("still editing after Cancel")
	}

	c.Edit()
	if err := c.Update(ctx, 1, explorer.UpdateRequest{}); err != nil {
		t.Fatal(err)
	}
	if c.Editing() || r.details[len(r.details)-1].Editing {
		t.Fatal("not back to read mode after Save")
	}

	c.Edit()
	fb.Errs = map[string]error{"Update": errors.New("bad")}
	_ = c.Update(ctx, 1, explorer.UpdateRequest{})
	if !c.Editing() {
		t.Fatal("a failed Save must stay in edit mode")
	}
	_ = c.LoadDetail(ctx, 2)
	if c.Editing() {
		t.Fatal("jumping to another memory must leave edit mode")
	}
}

func TestFindSimilar(t *testing.T) {
	ctx := context.Background()
	fb := mutFake()
	fb.SimilarValue = []store.Similar{{ID: 2, Kind: store.KindNote, Similarity: 0.8}}
	c, r := setup(fb)
	_ = c.LoadDetail(ctx, 1)
	if err := c.FindSimilar(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if len(r.similars) != 2 {
		t.Fatalf("similar events = %+v", r.similars)
	}
	if s := r.similars[0]; s.State != SimilarSearching || s.Namespace != "demo" || s.MemoryID != 1 {
		t.Fatalf("first = %+v", s)
	}
	if s := r.similars[1]; s.State != SimilarDone || len(s.Items) != 1 || s.Items[0].ID != 2 {
		t.Fatalf("second = %+v", s)
	}
	if fb.Calls("Similar") != 1 || fb.Calls("Stats") != 0 {
		t.Fatal("unexpected calls")
	}

	// A re-notification keeps the results, a reload clears them.
	c.SetDiffMode(false)
	if last := r.similars[len(r.similars)-1]; len(r.similars) != 3 || last.State != SimilarDone {
		t.Fatalf("similar not re-sent: %+v", r.similars)
	}
	_ = c.LoadDetail(ctx, 1)
	c.SetDiffMode(true)
	if len(r.similars) != 3 {
		t.Fatalf("similar kept after reload: %+v", r.similars)
	}
}

func TestFindSimilarEmptyAndError(t *testing.T) {
	ctx := context.Background()
	fb := mutFake()
	c, r := setup(fb)
	_ = c.LoadDetail(ctx, 1)
	_ = c.FindSimilar(ctx, 1)
	if last := r.similars[len(r.similars)-1]; last.State != SimilarDone || len(last.Items) != 0 {
		t.Fatalf("last = %+v", last)
	}

	boom := errors.New("boom")
	fb.Errs = map[string]error{"Similar": boom}
	if err := c.FindSimilar(ctx, 1); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if last := r.similars[len(r.similars)-1]; last.State != SimilarFailed {
		t.Fatalf("last = %+v", last)
	}
	if want := []banner{{"boom", true}}; !slices.Equal(r.banners, want) {
		t.Fatalf("banners = %+v", r.banners)
	}
}

func TestFindSimilarDroppedWhenSelectionChanges(t *testing.T) {
	ctx := context.Background()
	fb := mutFake()
	c, r := setup(fb)
	_ = c.LoadDetail(ctx, 1)
	fb.Hook = func(m string) {
		if m == "Similar" {
			c.Select(2)
		}
	}
	_ = c.FindSimilar(ctx, 1)
	if len(r.similars) != 1 || r.similars[0].State != SimilarSearching {
		t.Fatalf("similar events = %+v", r.similars)
	}
}
