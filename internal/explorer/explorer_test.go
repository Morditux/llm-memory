package explorer

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Morditux/llm-memory/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestLineDiff(t *testing.T) {
	big := strings.TrimSuffix(strings.Repeat("x\n", 600), "\n") // 600 lines
	tests := []struct {
		name     string
		old, new string
		want     []DiffLine
	}{
		{"both empty", "", "", []DiffLine{{Context, ""}}},
		{"identical", "a\nb", "a\nb", []DiffLine{{Context, "a"}, {Context, "b"}}},
		{"add", "a", "a\nb", []DiffLine{{Context, "a"}, {Add, "b"}}},
		{"delete", "a\nb", "a", []DiffLine{{Context, "a"}, {Del, "b"}}},
		{"modify", "a\nb\nc", "a\nB\nc", []DiffLine{{Context, "a"}, {Del, "b"}, {Add, "B"}, {Context, "c"}}},
		{"from empty", "", "a", []DiffLine{{Del, ""}, {Add, "a"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LineDiff(tc.old, tc.new); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("guard above limit", func(t *testing.T) {
		// 600*600 = 360000 > 250000: everything removed then everything added.
		got := LineDiff(big, big)
		if len(got) != 1200 {
			t.Fatalf("len = %d, want 1200", len(got))
		}
		for i, l := range got {
			want := Del
			if i >= 600 {
				want = Add
			}
			if l.Op != want {
				t.Fatalf("line %d op = %v, want %v", i, l.Op, want)
			}
		}
	})
	t.Run("at limit uses LCS", func(t *testing.T) {
		// 500*500 = 250000 is not above the guard.
		s := strings.TrimSuffix(strings.Repeat("x\n", 500), "\n")
		got := LineDiff(s, s)
		if len(got) != 500 || got[0].Op != Context {
			t.Fatalf("expected 500 context lines, got %d (first %v)", len(got), got[0])
		}
	})
}

func TestParseExpiration(t *testing.T) {
	tests := []struct {
		name, in, at string
		wantNil      bool
		wantErr      string
		approx       time.Duration
	}{
		{name: "none", wantNil: true},
		{name: "blank", in: " ", at: " ", wantNil: true},
		{name: "both", in: "1h", at: "2030-01-01T00:00:00Z", wantErr: "not both"},
		{name: "days", in: "7d", approx: 7 * 24 * time.Hour},
		{name: "go duration", in: "90m", approx: 90 * time.Minute},
		{name: "bad days", in: "xd", wantErr: "invalid duration"},
		{name: "zero days", in: "0d", wantErr: "invalid duration"},
		{name: "bad duration", in: "soon", wantErr: "invalid duration"},
		{name: "negative duration", in: "-1h", wantErr: "invalid duration"},
		{name: "bad at", at: "tomorrow", wantErr: "invalid RFC 3339"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseExpiration(tc.in, tc.at)
			if tc.wantErr != "" {
				var ve *store.ValidationError
				if !errors.As(err, &ve) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want validation error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if d := time.Until(*got) - tc.approx; d > time.Minute || d < -time.Minute {
				t.Fatalf("got %v, want about now+%v", got, tc.approx)
			}
		})
	}
	t.Run("at is UTC", func(t *testing.T) {
		got, err := ParseExpiration("", "2030-01-01T02:00:00+02:00")
		if err != nil || got == nil || !got.Equal(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) || got.Location() != time.UTC {
			t.Fatalf("got %v, %v", got, err)
		}
	})
}

func TestMergeSimilar(t *testing.T) {
	fts := []store.Similar{{ID: 1, Similarity: 0.5}, {ID: 2, Similarity: 0.4}}
	tests := []struct {
		name     string
		fts, sem []store.Similar
		limit    int
		want     []int64
	}{
		{"no semantic keeps fts as is", fts, nil, 1, []int64{1, 2}},
		{"best score wins and sorts", fts, []store.Similar{{ID: 2, Similarity: 0.9}, {ID: 3, Similarity: 0.1}}, 5, []int64{2, 1, 3}},
		{"lower semantic score ignored", fts, []store.Similar{{ID: 1, Similarity: 0.1}}, 5, []int64{1, 2}},
		{"tie broken by higher id", []store.Similar{{ID: 1, Similarity: 0.5}}, []store.Similar{{ID: 2, Similarity: 0.5}}, 5, []int64{2, 1}},
		{"limit", fts, []store.Similar{{ID: 3, Similarity: 0.9}}, 2, []int64{3, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ids []int64
			for _, s := range MergeSimilar(tc.fts, tc.sem, tc.limit) {
				ids = append(ids, s.ID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestCreateWithLinksAndActor(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ex := New(st, nil, "")
	a, err := ex.Create(ctx, CreateRequest{Namespace: "demo", Kind: store.KindFact, Title: "A", Content: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ex.Create(ctx, CreateRequest{Namespace: "demo", Kind: store.KindFact, Title: "B", Content: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ex.Create(ctx, CreateRequest{
		Namespace: "demo", Kind: store.KindFact, Title: "C", Content: "gamma",
		RelatedTo: []int64{a.ID}, DependsOn: []int64{b.ID}, ExpiresIn: "1d",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != DefaultActor || c.ExpiresAt == nil {
		t.Fatalf("source = %q, expires = %v", c.Source, c.ExpiresAt)
	}
	d, err := ex.Detail(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[int64]store.LinkKind{}
	for _, l := range d.Links {
		kinds[l.ID] = l.Kind
	}
	if kinds[a.ID] != store.LinkRelatesTo || kinds[b.ID] != store.LinkDependsOn || len(d.History) == 0 {
		t.Fatalf("links = %+v, history = %d", d.Links, len(d.History))
	}

	if _, err := ex.Create(ctx, CreateRequest{Namespace: "demo", Kind: store.KindFact, Content: "x", ExpiresIn: "nope"}); err == nil {
		t.Fatal("expected expiration error")
	}
	var nf *store.NotFoundError
	if _, err := ex.Detail(ctx, 9999); !errors.As(err, &nf) {
		t.Fatalf("Detail(9999) err = %v, want NotFoundError", err)
	}

	gx := New(st, nil, "gui")
	m, err := gx.Create(ctx, CreateRequest{Namespace: "demo", Kind: store.KindFact, Content: "by gui"})
	if err != nil || m.Source != "gui" {
		t.Fatalf("source = %q, err = %v", m.Source, err)
	}
}

func TestSimilarWithoutSemanticEngine(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ex := New(st, nil, "")
	a, _ := ex.Create(ctx, CreateRequest{Namespace: "demo", Kind: store.KindFact, Title: "PostgreSQL", Content: "PostgreSQL database tuning"})
	b, _ := ex.Create(ctx, CreateRequest{Namespace: "demo", Kind: store.KindFact, Title: "PostgreSQL notes", Content: "PostgreSQL database tuning tips"})

	sims, err := ex.Similar(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sims {
		if s.ID == a.ID {
			t.Fatalf("current memory must be excluded: %+v", sims)
		}
	}
	if len(sims) == 0 || sims[0].ID != b.ID {
		t.Fatalf("expected #%d, got %+v", b.ID, sims)
	}
	var nf *store.NotFoundError
	if _, err := ex.Similar(ctx, 9999); !errors.As(err, &nf) {
		t.Fatalf("err = %v, want NotFoundError", err)
	}
}

type semStub struct {
	*store.Store
	sims []store.Similar
}

func (s semStub) FindSimilar(context.Context, string, string, float64, int) ([]store.Similar, error) {
	return s.sims, nil
}

func TestSimilarWithSemanticEngine(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a, _ := st.Create(ctx, store.NewMemory{Namespace: "demo", Kind: store.KindFact, Content: "alpha"})
	b, _ := st.Create(ctx, store.NewMemory{Namespace: "demo", Kind: store.KindFact, Content: "unrelated words"})
	ex := New(st, semStub{Store: st, sims: []store.Similar{{ID: a.ID, Similarity: 1}, {ID: b.ID, Similarity: 0.9}}}, "")
	sims, err := ex.Similar(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sims) != 1 || sims[0].ID != b.ID {
		t.Fatalf("got %+v", sims)
	}
}
