package semantic

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Morditux/llm-memory/internal/store"
)

// Tuning.
const (
	// DefaultMinScore is the cosine similarity below which a vector match is
	// ignored. It depends on the model; 0.3 suits common sentence models.
	DefaultMinScore = 0.3
	// DefaultCatchUp is the number of unindexed memories embedded before each
	// search, so that memories written elsewhere become searchable.
	DefaultCatchUp = 64
	// rrfK is the Reciprocal Rank Fusion constant (Cormack et al., 2009).
	rrfK = 60
	// candidateFactor: each ranking contributes limit*candidateFactor items.
	candidateFactor = 3
)

// Options configures an Engine.
type Options struct {
	MinScore float64      // default DefaultMinScore; negative disables the threshold
	CatchUp  int          // default DefaultCatchUp; negative disables catch-up
	Logger   *slog.Logger // nil = discard
}

// Engine runs hybrid (full-text + vector) searches.
type Engine struct {
	store    *store.Store
	emb      Embedder
	minScore float64
	catchUp  int
	log      *slog.Logger
}

// New builds an Engine.
func New(st *store.Store, emb Embedder, opts Options) *Engine {
	e := &Engine{store: st, emb: emb, minScore: opts.MinScore, catchUp: opts.CatchUp, log: opts.Logger}
	if e.minScore == 0 {
		e.minScore = DefaultMinScore
	}
	if e.catchUp == 0 {
		e.catchUp = DefaultCatchUp
	}
	if e.log == nil {
		e.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return e
}

// Model returns the embeddings model name.
func (e *Engine) Model() string { return e.emb.Model() }

// embedText is what gets embedded for a memory.
func embedText(m store.EmbeddingSource) string {
	parts := []string{}
	if m.Title != "" {
		parts = append(parts, m.Title)
	}
	parts = append(parts, m.Content)
	if len(m.Tags) > 0 {
		parts = append(parts, strings.Join(m.Tags, " "))
	}
	return strings.Join(parts, "\n")
}

// Index embeds up to limit memories that have no embedding for the model
// (limit <= 0: all of them), most recent first. progress, if not nil, is
// called after each stored batch.
func (e *Engine) Index(ctx context.Context, limit int, progress func(done int)) (int, error) {
	todo, err := e.store.MissingEmbeddings(ctx, e.emb.Model(), limit)
	if err != nil {
		return 0, err
	}
	done := 0
	const chunk = 64 // store and report progress regularly
	for start := 0; start < len(todo); start += chunk {
		batch := todo[start:min(start+chunk, len(todo))]
		texts := make([]string, len(batch))
		for i, m := range batch {
			texts[i] = embedText(m)
		}
		vecs, err := e.emb.Embed(ctx, texts)
		if err != nil {
			return done, err
		}
		rows := make([]store.Vector, len(batch))
		for i, m := range batch {
			rows[i] = store.Vector{ID: m.ID, Values: vecs[i]}
		}
		if err := e.store.SetEmbeddings(ctx, e.emb.Model(), rows); err != nil {
			return done, err
		}
		done += len(batch)
		if progress != nil {
			progress(done)
		}
	}
	return done, nil
}

// Reindex recomputes embeddings: missing ones only, or all of them for the
// current model when all is true.
func (e *Engine) Reindex(ctx context.Context, all bool, progress func(done int)) (int, error) {
	if all {
		if _, err := e.store.DeleteEmbeddings(ctx, e.emb.Model()); err != nil {
			return 0, err
		}
	}
	return e.Index(ctx, 0, progress)
}

// Search runs store.Search and a vector search with the same filters, and
// fuses both rankings with Reciprocal Rank Fusion. If the embeddings API
// fails, it logs a warning and returns the full-text results: recall keeps
// working without the API.
func (e *Engine) Search(ctx context.Context, q store.SearchQuery) ([]store.SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = store.DefaultSearchLimit
	}
	limit = min(limit, store.MaxSearchLimit)

	wide := q
	wide.Limit = min(limit*candidateFactor, store.MaxSearchLimit)
	fts, err := e.store.Search(ctx, wide) // also validates namespace and kinds
	if err != nil {
		return nil, err
	}
	if !store.HasSearchTerms(q.Query) {
		return truncate(fts, limit), nil
	}

	if e.catchUp > 0 {
		if n, err := e.Index(ctx, e.catchUp, nil); err != nil {
			e.log.WarnContext(ctx, "semantic: indexing failed, continuing", "err", err)
		} else if n > 0 {
			e.log.DebugContext(ctx, "semantic: indexed memories", "count", n)
		}
	}
	qv, err := e.emb.Embed(ctx, []string{q.Query})
	if err != nil {
		e.log.WarnContext(ctx, "semantic: query embedding failed, using full-text results only", "err", err)
		return truncate(fts, limit), nil
	}
	vecs, err := e.store.Vectors(ctx, e.emb.Model(), store.VectorFilter{
		Namespace: q.Namespace, Kinds: q.Kinds, Tags: q.Tags, IncludeArchived: q.IncludeArchived,
	})
	if err != nil {
		return nil, err
	}
	nearest := rankByCosine(qv[0], vecs, e.minScore, wide.Limit)
	return e.fuse(ctx, fts, nearest, limit)
}

type scored struct {
	id    int64
	score float64
}

// rankByCosine returns up to n ids by decreasing cosine similarity, ignoring
// vectors of a different dimension and scores below minScore.
func rankByCosine(q []float32, vecs []store.Vector, minScore float64, n int) []scored {
	out := []scored{}
	for _, v := range vecs {
		if len(v.Values) != len(q) {
			continue
		}
		c := Cosine(q, v.Values)
		if minScore >= 0 && c < minScore {
			continue
		}
		out = append(out, scored{v.ID, c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].id > out[j].id
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Cosine returns the cosine similarity of a and b (0 if either is null).
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// fuse merges the full-text and vector rankings: score(d) = Σ 1/(k + rank).
// Results keep their FTS excerpt when they have one.
func (e *Engine) fuse(ctx context.Context, fts []store.SearchResult, nearest []scored, limit int) ([]store.SearchResult, error) {
	rrf := map[int64]float64{}
	byID := map[int64]store.SearchResult{}
	for i, r := range fts {
		rrf[r.ID] += 1 / float64(rrfK+i+1)
		byID[r.ID] = r
	}
	var missing []int64
	for i, n := range nearest {
		rrf[n.id] += 1 / float64(rrfK+i+1)
		if _, ok := byID[n.id]; !ok {
			missing = append(missing, n.id)
		}
	}
	ms, err := e.store.GetMany(ctx, missing)
	if err != nil {
		return nil, err
	}
	for _, id := range missing {
		if m, ok := ms[id]; ok {
			byID[id] = store.SearchResult{Memory: m, Snippet: store.FirstWords(m.Content)}
		}
	}
	out := make([]store.SearchResult, 0, len(byID))
	for id, r := range byID {
		weight := 1.0
		switch r.Importance {
		case 3:
			weight = 1.15
		case 1:
			weight = 0.90
		}
		r.Score = -(rrf[id] * weight) // keep "lower is better", like bm25
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		return out[i].ID > out[j].ID
	})
	return truncate(out, limit), nil
}

func truncate(r []store.SearchResult, n int) []store.SearchResult {
	if len(r) > n {
		return r[:n]
	}
	return r
}

// Describe is a one-line summary for logs and the CLI.
func (e *Engine) Describe() string {
	return fmt.Sprintf("model %s, min score %.2f", e.emb.Model(), e.minScore)
}

// FindSimilar returns active memories in namespace whose cosine similarity to
// text is at least minScore (default 0.75 if <= 0). API failures are logged and
// ignored so writes never depend on the embeddings endpoint.
func (e *Engine) FindSimilar(ctx context.Context, text, namespace string, minScore float64, limit int) ([]store.Similar, error) {
	if strings.TrimSpace(text) == "" || limit <= 0 {
		return nil, nil
	}
	if minScore <= 0 {
		minScore = max(e.minScore, 0.75)
	}
	qv, err := e.emb.Embed(ctx, []string{text})
	if err != nil {
		e.log.WarnContext(ctx, "semantic: duplicate check failed, skipping", "err", err)
		return nil, nil
	}
	vecs, err := e.store.Vectors(ctx, e.emb.Model(), store.VectorFilter{Namespace: namespace})
	if err != nil {
		return nil, err
	}
	nearest := rankByCosine(qv[0], vecs, minScore, limit)
	now := time.Now().UTC()
	out := make([]store.Similar, 0, len(nearest))
	for _, s := range nearest {
		m, err := e.store.Get(ctx, s.id)
		if err != nil || m.Archived() || m.Expired(now) {
			continue
		}
		snip := strings.TrimSpace(m.Content)
		if r := []rune(snip); len(r) > 160 {
			snip = string(r[:160]) + "…"
		}
		out = append(out, store.Similar{
			ID:         m.ID,
			Kind:       m.Kind,
			Title:      m.Title,
			Snippet:    snip,
			Similarity: s.score,
		})
	}
	return out, nil
}
