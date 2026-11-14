package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Search limits.
const (
	DefaultSearchLimit = 10
	MaxSearchLimit     = 50
	maxQueryTerms      = 16
	maxSimilarTerms    = 32
	snippetWords       = 20
)

// Snippet markers around matched terms (Markdown bold, readable by LLMs and humans).
const (
	snippetOpen  = "**"
	snippetClose = "**"
	snippetEll   = "…"
)

// SearchQuery is the input of Search.
type SearchQuery struct {
	Query           string // free text; never interpreted as FTS5 syntax
	Namespace       string // exact namespace; empty = all. Must exist if set.
	Kinds           []Kind
	IncludeArchived bool
	Limit           int // default 10, capped at 50
}

// SearchResult is one ranked hit.
type SearchResult struct {
	Memory
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"` // bm25: lower is better; 0 when the query is empty
}

// queryTerms splits free text into words made of letters, digits and
// combining marks. Everything else (quotes, '*', ':', '-', parentheses…)
// is a separator, so no user input can reach FTS5 as syntax.
func queryTerms(s string, limit int) []string {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(words))
	for _, w := range words {
		k := strings.ToLower(w)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, w)
		if len(out) == limit {
			break
		}
	}
	return out
}

// quoteTerm turns a word into an FTS5 string literal. Terms only contain
// letters/digits/marks, but quotes are doubled anyway as a second barrier.
func quoteTerm(t string) string {
	return `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
}

// buildMatch joins terms into a MATCH expression. prefix adds '*' to each
// term; op is "AND" or "OR".
func buildMatch(terms []string, prefix bool, op string) string {
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = quoteTerm(t)
		if prefix {
			parts[i] += "*"
		}
	}
	return strings.Join(parts, " "+op+" ")
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultSearchLimit
	case n > MaxSearchLimit:
		return MaxSearchLimit
	default:
		return n
	}
}

// Search runs a full-text search. Each word is matched as a prefix; words are
// combined with AND, then with OR if AND finds nothing. An empty query (or one
// with no word characters) returns the most recently updated memories.
func (s *Store) Search(ctx context.Context, q SearchQuery) ([]SearchResult, error) {
	limit := clampLimit(q.Limit)
	ns := ""
	if q.Namespace != "" {
		var err error
		if ns, err = s.RequireNamespace(ctx, q.Namespace); err != nil {
			return nil, err
		}
	}
	for _, k := range q.Kinds {
		if err := validateKind(k); err != nil {
			return nil, err
		}
	}

	terms := queryTerms(q.Query, maxQueryTerms)
	if len(terms) == 0 {
		return s.recent(ctx, ns, q.Kinds, q.IncludeArchived, limit)
	}
	res, err := s.match(ctx, buildMatch(terms, true, "AND"), ns, q.Kinds, q.IncludeArchived, 0, limit)
	if err != nil || len(res) > 0 || len(terms) == 1 {
		return res, err
	}
	return s.match(ctx, buildMatch(terms, true, "OR"), ns, q.Kinds, q.IncludeArchived, 0, limit)
}

// filterSQL builds the WHERE fragments shared by search queries. Column
// names are fixed; values are bound parameters.
func filterSQL(ns string, kinds []Kind, includeArchived bool, excludeID int64) (string, []any) {
	var where []string
	var args []any
	if ns != "" {
		where, args = append(where, "m.namespace = ?"), append(args, ns)
	}
	if len(kinds) > 0 {
		ph := make([]string, len(kinds))
		for i, k := range kinds {
			ph[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "m.kind IN ("+strings.Join(ph, ", ")+")")
	}
	if !includeArchived {
		where = append(where, "m.archived_at IS NULL")
	}
	if excludeID > 0 {
		where, args = append(where, "m.id <> ?"), append(args, excludeID)
	}
	if len(where) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(where, " AND "), args
}

const prefixedColumns = `m.id, m.namespace, m.kind, m.title, m.content, m.tags, m.importance, m.source,
	m.created_at, m.updated_at, m.updated_by, m.archived_at`

func (s *Store) match(ctx context.Context, expr, ns string, kinds []Kind, includeArchived bool, excludeID int64, limit int) ([]SearchResult, error) {
	filter, fargs := filterSQL(ns, kinds, includeArchived, excludeID)
	query := `SELECT ` + prefixedColumns + `,
			snippet(memories_fts, -1, ?, ?, ?, ?),
			bm25(memories_fts, 2.0, 1.0, 1.0) AS score
		FROM memories_fts JOIN memories m ON m.id = memories_fts.rowid
		WHERE memories_fts MATCH ?` + filter + `
		ORDER BY score, m.id DESC
		LIMIT ?`
	args := []any{snippetOpen, snippetClose, snippetEll, snippetWords, expr}
	args = append(args, fargs...)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []SearchResult{}
	for rows.Next() {
		var r SearchResult
		var snip string
		m, err := scanMemory(rowScanner{rows, []any{&snip, &r.Score}})
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		r.Memory, r.Snippet = m, snip
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return out, nil
}

// rowScanner appends extra destinations after the memory columns.
type rowScanner struct {
	sc    scanner
	extra []any
}

func (r rowScanner) Scan(dest ...any) error { return r.sc.Scan(append(dest, r.extra...)...) }

func (s *Store) recent(ctx context.Context, ns string, kinds []Kind, includeArchived bool, limit int) ([]SearchResult, error) {
	filter, args := filterSQL(ns, kinds, includeArchived, 0)
	query := "SELECT " + prefixedColumns + " FROM memories m WHERE 1=1" + filter +
		" ORDER BY m.updated_at DESC, m.id DESC LIMIT ?"
	ms, err := queryMemories(ctx, s.db, query, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	out := make([]SearchResult, len(ms))
	for i, m := range ms {
		out[i] = SearchResult{Memory: m, Snippet: firstWords(m.Content, snippetWords)}
	}
	return out, nil
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) <= n {
		return strings.Join(f, " ")
	}
	return strings.Join(f[:n], " ") + snippetEll
}

// ---- Duplicate detection ----------------------------------------------------

// SimilarityThreshold is the minimum overlap (shared significant words over
// the size of the smaller word set) for a memory to count as a probable
// duplicate.
const SimilarityThreshold = 0.5

// Similar is a probable duplicate.
type Similar struct {
	ID         int64   `json:"id"`
	Kind       Kind    `json:"kind"`
	Title      string  `json:"title"`
	Snippet    string  `json:"snippet"`
	Similarity float64 `json:"similarity"` // 0..1
}

// FindSimilar returns up to n active memories of namespace ns whose wording
// overlaps strongly with text. excludeID (if > 0) is left out, so it can be
// called after insertion too. Candidates come from an FTS5 OR query on the
// significant words of text, then are filtered by word overlap.
func (s *Store) FindSimilar(ctx context.Context, text, ns string, n int, excludeID int64) ([]Similar, error) {
	if n <= 0 {
		return []Similar{}, nil
	}
	if ns == "" {
		ns = DefaultNamespace
	}
	ns, err := NormalizeNamespace(ns)
	if err != nil {
		return nil, err
	}
	words := significantWords(text)
	if len(words) == 0 {
		return []Similar{}, nil
	}
	terms := make([]string, 0, len(words))
	for w := range words {
		terms = append(terms, w)
	}
	sort.Strings(terms) // deterministic query
	if len(terms) > maxSimilarTerms {
		// Keep the longest words: they are the most discriminating.
		sort.SliceStable(terms, func(i, j int) bool { return len([]rune(terms[i])) > len([]rune(terms[j])) })
		terms = terms[:maxSimilarTerms]
	}

	cands, err := s.match(ctx, buildMatch(terms, false, "OR"), ns, nil, false, excludeID, 4*n+6)
	if err != nil {
		return nil, err
	}
	out := []Similar{}
	for _, c := range cands {
		sim := overlap(words, significantWords(c.Title+" "+c.Content))
		if sim < SimilarityThreshold {
			continue
		}
		out = append(out, Similar{ID: c.ID, Kind: c.Kind, Title: c.Title, Snippet: c.Snippet, Similarity: sim})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

func overlap(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	shared := 0
	for w := range small {
		if large[w] {
			shared++
		}
	}
	return float64(shared) / float64(len(small))
}

// significantWords returns the folded (lowercase, unaccented) words of at
// least 3 characters that are not common French/English stop words.
func significantWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range queryTerms(s, -1) {
		f := fold(w)
		if len([]rune(f)) < 3 || stopWords[f] {
			continue
		}
		out[f] = true
	}
	return out
}

// fold lowercases and strips common Latin diacritics, mirroring the
// unicode61 remove_diacritics tokenizer closely enough for overlap scoring.
func fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsMark(r) {
			continue
		}
		if rep, ok := foldMap[r]; ok {
			b.WriteString(rep)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

var foldMap = map[rune]string{
	'à': "a", 'â': "a", 'ä': "a", 'á': "a", 'ã': "a", 'å': "a",
	'ç': "c",
	'é': "e", 'è': "e", 'ê': "e", 'ë': "e",
	'î': "i", 'ï': "i", 'í': "i", 'ì': "i",
	'ô': "o", 'ö': "o", 'ó': "o", 'ò': "o", 'õ': "o", 'ø': "o",
	'ù': "u", 'û': "u", 'ü': "u", 'ú': "u",
	'ÿ': "y", 'ý': "y", 'ñ': "n",
	'œ': "oe", 'æ': "ae", 'ß': "ss",
}

var stopWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		les des une pour par sur dans avec sans sous entre vers chez est sont etait ete etre avoir
		qui que quoi dont ces cet cette ses son mes mon tes ton nos notre votre vos leur leurs
		plus moins tres aussi mais donc car pas comme tout tous toute toutes fait faire peut
		the and for with without from into onto that this these those are was were been being
		have has had not but all any can will would should could our your their its they them
		than then there here which what when where who why how also only just use used using`) {
		m[w] = true
	}
	return m
}()
