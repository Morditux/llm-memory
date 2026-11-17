package semantic

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Morditux/llm-memory/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

type fixture struct {
	st                         *store.Store
	pg, deploy, style, auth, x store.Memory
}

func seed(t *testing.T) fixture {
	t.Helper()
	st := newStore(t)
	mk := func(m store.NewMemory) store.Memory {
		got, err := st.Create(context.Background(), m)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	return fixture{
		st:     st,
		pg:     mk(store.NewMemory{Namespace: "demo", Kind: store.KindDecision, Title: "Choix technique", Content: "PostgreSQL plutôt que MySQL, pour JSONB."}),
		deploy: mk(store.NewMemory{Namespace: "demo", Kind: store.KindFact, Content: "Déploiement avec Docker Compose sur un VPS Hetzner."}),
		style:  mk(store.NewMemory{Kind: store.KindPreference, Content: "Réponses courtes et concises."}),
		auth:   mk(store.NewMemory{Namespace: "api", Kind: store.KindDecision, Content: "Jetons JWT signés en EdDSA."}),
		x:      mk(store.NewMemory{Namespace: "demo", Kind: store.KindNote, Content: "Penser à arroser les cactus du bureau."}),
	}
}

func ids(rs []store.SearchResult) []int64 {
	out := []int64{}
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func search(t *testing.T, e *Engine, q store.SearchQuery) []store.SearchResult {
	t.Helper()
	res, err := e.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	return res
}

func TestSemanticFindsByMeaning(t *testing.T) {
	f := seed(t)
	ctx := context.Background()

	// Full-text alone cannot find it: no memory contains "sgbd".
	if res, _ := f.st.Search(ctx, store.SearchQuery{Query: "SGBD"}); len(res) != 0 {
		t.Fatalf("precondition: FTS found %v", ids(res))
	}
	e := New(f.st, &fakeEmbedder{model: "fake"}, Options{})
	res := search(t, e, store.SearchQuery{Query: "SGBD"})
	if len(res) != 1 || res[0].ID != f.pg.ID {
		t.Fatalf("semantic search for SGBD = %v, want [%d]", ids(res), f.pg.ID)
	}
	if res[0].Snippet == "" || res[0].Score >= 0 {
		t.Errorf("snippet/score: %q / %v", res[0].Snippet, res[0].Score)
	}
	// Unrelated words: below the threshold, nothing returned.
	if res := search(t, e, store.SearchQuery{Query: "kubernetes helm"}); len(res) != 0 {
		t.Errorf("unrelated query returned %v", ids(res))
	}
}

func TestFusionRanksBothSignals(t *testing.T) {
	f := seed(t)
	e := New(f.st, &fakeEmbedder{model: "fake"}, Options{})
	// "docker" matches the deploy memory by words and by meaning: first.
	res := search(t, e, store.SearchQuery{Query: "docker"})
	if len(res) == 0 || res[0].ID != f.deploy.ID {
		t.Fatalf("docker: %v", ids(res))
	}
	if !strings.Contains(res[0].Snippet, "**Docker**") {
		t.Errorf("FTS snippet should be kept: %q", res[0].Snippet)
	}
	// A word-only match (no concept) still comes back through FTS.
	res = search(t, e, store.SearchQuery{Query: "cactus"})
	if len(res) != 1 || res[0].ID != f.x.ID {
		t.Errorf("cactus: %v", ids(res))
	}
}

func TestSemanticFilters(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	e := New(f.st, &fakeEmbedder{model: "fake"}, Options{})

	if res := search(t, e, store.SearchQuery{Query: "token login", Namespace: "demo"}); len(res) != 0 {
		t.Errorf("namespace filter leaked: %v", ids(res))
	}
	if res := search(t, e, store.SearchQuery{Query: "token login", Namespace: "api"}); len(res) != 1 || res[0].ID != f.auth.ID {
		t.Errorf("namespace api: %v", ids(res))
	}
	if res := search(t, e, store.SearchQuery{Query: "database", Tags: []string{"nonexistent"}}); len(res) != 0 {
		t.Errorf("tag filter leaked: %v", ids(res))
	}
	if res := search(t, e, store.SearchQuery{Query: "database", Kinds: []store.Kind{store.KindFact}}); len(res) != 0 {
		t.Errorf("kind filter leaked: %v", ids(res))
	}
	if _, err := f.st.Archive(ctx, f.pg.ID, "t", ""); err != nil {
		t.Fatal(err)
	}
	if res := search(t, e, store.SearchQuery{Query: "SGBD"}); len(res) != 0 {
		t.Errorf("archived memory returned: %v", ids(res))
	}
	if res := search(t, e, store.SearchQuery{Query: "SGBD", IncludeArchived: true}); len(res) != 1 || !res[0].Archived() {
		t.Errorf("include archived: %v", ids(res))
	}
	// Validation errors come from the full-text engine, unchanged.
	if _, err := e.Search(ctx, store.SearchQuery{Query: "x", Namespace: "nowhere"}); err == nil {
		t.Error("unknown namespace accepted")
	}
	// Limit.
	if res := search(t, e, store.SearchQuery{Query: "postgresql docker jwt réponses", Limit: 2}); len(res) != 2 {
		t.Errorf("limit: %v", ids(res))
	}
}

func TestCatchUpAndInvalidation(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	emb := &fakeEmbedder{model: "fake"}
	e := New(f.st, emb, Options{CatchUp: 2})

	// First search indexes 2 memories (catch-up limit), most recent first.
	search(t, e, store.SearchQuery{Query: "docker"})
	if n, _ := f.st.CountMissingEmbeddings(ctx, "fake"); n != 3 {
		t.Fatalf("missing after first search: %d, want 3", n)
	}
	search(t, e, store.SearchQuery{Query: "docker"})
	search(t, e, store.SearchQuery{Query: "docker"})
	if n, _ := f.st.CountMissingEmbeddings(ctx, "fake"); n != 0 {
		t.Fatalf("missing after catch-up: %d", n)
	}

	// Changing the text drops the embedding; the next search recomputes it.
	newContent := "Hébergement : serveur dédié."
	if _, err := f.st.Update(ctx, f.pg.ID, store.Patch{Content: &newContent}, "t"); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.st.CountMissingEmbeddings(ctx, "fake"); n != 1 {
		t.Fatalf("update should invalidate the embedding: missing=%d", n)
	}
	// Importance changes do not.
	imp := 3
	_, _ = f.st.Update(ctx, f.deploy.ID, store.Patch{Importance: &imp}, "t")
	if n, _ := f.st.CountMissingEmbeddings(ctx, "fake"); n != 1 {
		t.Errorf("importance change invalidated an embedding: missing=%d", n)
	}
	res := search(t, e, store.SearchQuery{Query: "hosting"})
	if len(res) < 2 {
		t.Errorf("re-embedded memory not found by meaning: %v", ids(res))
	}

	// Empty query: same as full-text (recent memories), no API call.
	calls := emb.callCount()
	got := search(t, e, store.SearchQuery{Query: "  "})
	want, _ := f.st.Search(ctx, store.SearchQuery{Query: "  "})
	if c := emb.callCount(); c != calls || len(got) != len(want) || got[0].ID != want[0].ID {
		t.Errorf("empty query: %v vs %v, calls %d→%d", ids(got), ids(want), calls, c)
	}
}

func TestAPIFailureFallsBackToFullText(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	emb := &fakeEmbedder{model: "fake", fail: errAPIDown}
	e := New(f.st, emb, Options{Logger: logger})

	got := search(t, e, store.SearchQuery{Query: "docker"})
	want, _ := f.st.Search(ctx, store.SearchQuery{Query: "docker"})
	if len(got) != len(want) || got[0].ID != want[0].ID {
		t.Errorf("fallback = %v, want FTS %v", ids(got), ids(want))
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("failure not logged: %s", logs.String())
	}

	// Indexing fails but the query embedding works: existing vectors still used.
	ok := &fakeEmbedder{model: "fake"}
	if _, err := New(f.st, ok, Options{}).Index(ctx, 0, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = f.st.Create(ctx, store.NewMemory{Kind: store.KindNote, Content: "nouvelle note"})
	partial := &fakeEmbedder{model: "fake", fail: errAPIDown, failOn: func(texts []string) bool { return texts[0] == "nouvelle note" }}
	res := search(t, New(f.st, partial, Options{}), store.SearchQuery{Query: "SGBD"})
	if len(res) != 1 || res[0].ID != f.pg.ID {
		t.Errorf("with failed catch-up: %v", ids(res))
	}
}

func TestReindex(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	emb := &fakeEmbedder{model: "fake"}
	e := New(f.st, emb, Options{CatchUp: -1})

	var progress []int
	n, err := e.Reindex(ctx, false, func(done int) { progress = append(progress, done) })
	if err != nil || n != 5 || len(progress) != 1 || progress[0] != 5 {
		t.Fatalf("Reindex = %d, %v, progress %v", n, err, progress)
	}
	if n, _ := e.Reindex(ctx, false, nil); n != 0 {
		t.Errorf("second reindex embedded %d", n)
	}
	if n, _ := e.Reindex(ctx, true, nil); n != 5 {
		t.Errorf("reindex --all embedded %d", n)
	}
	// Another model is independent.
	other := New(f.st, &fakeEmbedder{model: "other"}, Options{})
	if n, _ := other.Reindex(ctx, false, nil); n != 5 {
		t.Errorf("other model: %d", n)
	}
	st, _ := f.st.Stats(ctx)
	if len(st.Embeddings) != 2 || st.Embeddings[0].Count != 5 {
		t.Errorf("stats embeddings: %+v", st.Embeddings)
	}
	// With CatchUp disabled, search does not index anything new.
	_, _ = f.st.Create(ctx, store.NewMemory{Kind: store.KindNote, Content: "base de données"})
	search(t, e, store.SearchQuery{Query: "sql"})
	if n, _ := f.st.CountMissingEmbeddings(ctx, "fake"); n != 1 {
		t.Errorf("catch-up disabled but missing=%d", n)
	}
	// Errors stop the reindex and report progress.
	broken := New(f.st, &fakeEmbedder{model: "fake", fail: errAPIDown}, Options{})
	if _, err := broken.Reindex(ctx, false, nil); err == nil {
		t.Error("expected error")
	}
}

func TestDimensionMismatchIgnored(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	// Index with 32-dim vectors, then query with the normal size: no vector match, FTS only.
	if _, err := New(f.st, &fakeEmbedder{model: "fake", dims: 32}, Options{}).Index(ctx, 0, nil); err != nil {
		t.Fatal(err)
	}
	e := New(f.st, &fakeEmbedder{model: "fake"}, Options{CatchUp: -1})
	if res := search(t, e, store.SearchQuery{Query: "SGBD"}); len(res) != 0 {
		t.Errorf("mismatched dimensions used: %v", ids(res))
	}
}

func TestEngineOverHTTP(t *testing.T) {
	f := seed(t)
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(fakeAPI(&requests, &mu))
	defer srv.Close()
	c, err := NewClient(ClientConfig{URL: srv.URL + "/v1/embeddings", Model: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	e := New(f.st, c, Options{})
	res := search(t, e, store.SearchQuery{Query: "SGBD"})
	if len(res) != 1 || res[0].ID != f.pg.ID {
		t.Errorf("over HTTP: %v", ids(res))
	}
	if requests != 2 { // one indexing batch + one query
		t.Errorf("%d HTTP requests, want 2", requests)
	}
	if e.Model() != "fake" || !strings.Contains(e.Describe(), "0.30") {
		t.Errorf("Model/Describe: %s / %s", e.Model(), e.Describe())
	}
}

func TestCosineAndRanking(t *testing.T) {
	if c := Cosine([]float32{1, 0}, []float32{1, 0}); math.Abs(c-1) > 1e-9 {
		t.Errorf("identical: %v", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{0, 1}); c != 0 {
		t.Errorf("orthogonal: %v", c)
	}
	if c := Cosine([]float32{0, 0}, []float32{1, 1}); c != 0 {
		t.Errorf("null vector: %v", c)
	}
	if c := Cosine([]float32{1, 1}, []float32{-1, -1}); math.Abs(c+1) > 1e-9 {
		t.Errorf("opposite: %v", c)
	}
	vecs := []store.Vector{{ID: 1, Values: []float32{1, 0}}, {ID: 2, Values: []float32{1, 1}}, {ID: 3, Values: []float32{0, 1}}, {ID: 4, Values: []float32{1}}}
	got := rankByCosine([]float32{1, 0}, vecs, 0.5, 10)
	if len(got) != 2 || got[0].id != 1 || got[1].id != 2 {
		t.Errorf("rank: %+v", got)
	}
	if got := rankByCosine([]float32{1, 0}, vecs, -1, 1); len(got) != 1 {
		t.Errorf("limit: %+v", got)
	}
	if got := rankByCosine([]float32{1, 0}, vecs, -1, 10); len(got) != 3 {
		t.Errorf("no threshold should keep same-dimension vectors: %+v", got)
	}
}

func TestEmbedText(t *testing.T) {
	got := embedText(store.EmbeddingSource{Title: "T", Content: "C", Tags: []string{"a", "b"}})
	if got != "T\nC\na b" {
		t.Errorf("embedText = %q", got)
	}
	if got := embedText(store.EmbeddingSource{Content: "C"}); got != "C" {
		t.Errorf("embedText = %q", got)
	}
}

func TestSearchCatchUpScoped(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	demoActive, _ := st.Create(ctx, store.NewMemory{Namespace: "demo", Kind: store.KindFact, Content: "PostgreSQL pour JSONB"})
	demoArch, _ := st.Create(ctx, store.NewMemory{Namespace: "demo", Kind: store.KindFact, Content: "Ancien secret oublié"})
	if _, err := st.Archive(ctx, demoArch.ID, "t", ""); err != nil {
		t.Fatal(err)
	}
	apiActive, _ := st.Create(ctx, store.NewMemory{Namespace: "api", Kind: store.KindFact, Content: "Jetons JWT EdDSA"})

	emb := &fakeEmbedder{model: "fake"}
	e := New(st, emb, Options{})

	// Search scoped to "demo" (active only) must only catch up demoActive.
	search(t, e, store.SearchQuery{Query: "SGBD", Namespace: "demo"})
	if v, _ := st.Vectors(ctx, "fake", store.VectorFilter{Namespace: "demo"}); len(v) != 1 || v[0].ID != demoActive.ID {
		t.Fatalf("demo active vectors = %+v, want [%d]", v, demoActive.ID)
	}
	if v, _ := st.Vectors(ctx, "fake", store.VectorFilter{Namespace: "demo", IncludeArchived: true}); len(v) != 1 {
		t.Fatalf("archived memory was indexed without IncludeArchived: %+v", v)
	}
	if v, _ := st.Vectors(ctx, "fake", store.VectorFilter{Namespace: "api"}); len(v) != 0 {
		t.Fatalf("other namespace memory was indexed during scoped search: %+v", v)
	}

	// Search with IncludeArchived catches up demoArch too, still leaving api untouched.
	search(t, e, store.SearchQuery{Query: "SGBD", Namespace: "demo", IncludeArchived: true})
	if v, _ := st.Vectors(ctx, "fake", store.VectorFilter{Namespace: "demo", IncludeArchived: true}); len(v) != 2 {
		t.Fatalf("expected archived demo memory to be indexed with IncludeArchived: %+v", v)
	}
	if v, _ := st.Vectors(ctx, "fake", store.VectorFilter{Namespace: "api"}); len(v) != 0 {
		t.Fatalf("api namespace should still not be indexed: %+v", v)
	}
	_ = apiActive
}

func TestFindSimilar(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	emb := &fakeEmbedder{model: "fake"}
	e := New(f.st, emb, Options{})

	// When the target namespace has no indexed vectors yet, FindSimilar skips the API call.
	got, err := e.FindSimilar(ctx, "PostgreSQL pour JSONB", "demo", 0, 3)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty vectors FindSimilar = %+v, %v", got, err)
	}
	if emb.callCount() != 0 {
		t.Fatalf("expected 0 embed calls when no vectors exist, got %d", emb.callCount())
	}

	// Index only the "api" namespace: "demo" still has 0 vectors, so API is still skipped.
	if _, err := e.indexFiltered(ctx, store.MissingFilter{Namespace: "api"}, nil); err != nil {
		t.Fatal(err)
	}
	callsBefore := emb.callCount()
	got, err = e.FindSimilar(ctx, "PostgreSQL pour JSONB", "demo", 0, 3)
	if err != nil || len(got) != 0 || emb.callCount() != callsBefore {
		t.Fatalf("demo FindSimilar with only api indexed = %+v, %v (calls %d -> %d)", got, err, callsBefore, emb.callCount())
	}

	// Index everything: FindSimilar in "demo" now matches f.pg via the API.
	if _, err := e.Index(ctx, 0, nil); err != nil {
		t.Fatal(err)
	}
	got, err = e.FindSimilar(ctx, "SGBD pour JSONB", "demo", 0.5, 3)
	if err != nil || len(got) == 0 || got[0].ID != f.pg.ID {
		t.Fatalf("FindSimilar(demo) = %+v, %v; want #%d", got, err, f.pg.ID)
	}

	// API failure is logged and ignored (returns nil, nil).
	var logs bytes.Buffer
	down := New(f.st, &fakeEmbedder{model: "fake", fail: errAPIDown}, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	got, err = down.FindSimilar(ctx, "SGBD pour JSONB", "demo", 0.5, 3)
	if err != nil || len(got) != 0 {
		t.Fatalf("FindSimilar on API failure = %+v, %v", got, err)
	}
	if !strings.Contains(logs.String(), "duplicate check failed") {
		t.Errorf("expected warning log on API failure, got: %s", logs.String())
	}
}

type hookEmbedder struct {
	*fakeEmbedder
	onEmbed func()
}

func (h *hookEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if h.onEmbed != nil {
		h.onEmbed()
	}
	return h.fakeEmbedder.Embed(ctx, texts)
}

func TestIndexSkipsConcurrentlyEditedMemory(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	m, err := st.Create(ctx, store.NewMemory{Namespace: "demo", Kind: store.KindFact, Content: "MySQL initial"})
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	emb := &hookEmbedder{
		fakeEmbedder: &fakeEmbedder{model: "fake"},
		onEmbed: func() {
			if edited {
				return
			}
			edited = true
			updated := "PostgreSQL pour JSONB"
			if _, err := st.Update(ctx, m.ID, store.Patch{Content: &updated}, "other"); err != nil {
				t.Errorf("concurrent update: %v", err)
			}
		},
	}
	e := New(st, emb, Options{})

	// Index reads "MySQL initial", then onEmbed mutates m to "PostgreSQL pour JSONB":
	// the stale vector must not be stored.
	if _, err := e.Index(ctx, 0, nil); err != nil {
		t.Fatal(err)
	}
	if vecs, _ := st.Vectors(ctx, "fake", store.VectorFilter{}); len(vecs) != 0 {
		t.Fatalf("expected stale vector skipped during concurrent edit, got %+v", vecs)
	}

	// Next search catches up with the updated text and finds it by its new meaning.
	res := search(t, e, store.SearchQuery{Query: "SGBD", Namespace: "demo"})
	if len(res) != 1 || res[0].ID != m.ID {
		t.Fatalf("expected updated memory indexed and found by new meaning, got %v", ids(res))
	}
}
