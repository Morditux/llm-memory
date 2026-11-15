package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// EmbeddingSource is the text of a memory to embed.
type EmbeddingSource struct {
	ID      int64
	Title   string
	Content string
	Tags    []string
}

// Vector is a stored embedding.
type Vector struct {
	ID     int64
	Values []float32
}

// EncodeVector packs v as float32 little-endian.
func EncodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// DecodeVector unpacks a float32 little-endian blob.
func DecodeVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("vector blob of %d bytes is not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, nil
}

const missingEmbeddingsWhere = ` FROM memories m
	WHERE NOT EXISTS (SELECT 1 FROM memory_embeddings e WHERE e.memory_id = m.id AND e.model = ?)`

// MissingEmbeddings returns up to limit memories (active first, then archived,
// most recent first) that have no embedding for model. limit <= 0 means all.
func (s *Store) MissingEmbeddings(ctx context.Context, model string, limit int) ([]EmbeddingSource, error) {
	q := "SELECT m.id, m.title, m.content, m.tags" + missingEmbeddingsWhere + " ORDER BY (m.archived_at IS NULL) DESC, m.id DESC"
	args := []any{model}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("missing embeddings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []EmbeddingSource{}
	for rows.Next() {
		var e EmbeddingSource
		var tags string
		if err := rows.Scan(&e.ID, &e.Title, &e.Content, &tags); err != nil {
			return nil, fmt.Errorf("missing embeddings: %w", err)
		}
		e.Tags = splitTags(tags)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("missing embeddings: %w", err)
	}
	return out, nil
}

// CountMissingEmbeddings counts memories without an embedding for model.
func (s *Store) CountMissingEmbeddings(ctx context.Context, model string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*)"+missingEmbeddingsWhere, model).Scan(&n); err != nil {
		return 0, fmt.Errorf("count missing embeddings: %w", err)
	}
	return n, nil
}

// SetEmbeddings stores vectors for model, replacing existing ones. Ids of
// memories deleted in the meantime are ignored. Embeddings are derived data:
// they are not recorded in memory_history.
func (s *Store) SetEmbeddings(ctx context.Context, model string, vecs []Vector) error {
	if strings.TrimSpace(model) == "" {
		return &ValidationError{Field: "model", Msg: "must not be empty"}
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, v := range vecs {
			if len(v.Values) == 0 {
				return &ValidationError{Field: "vector", Msg: fmt.Sprintf("empty embedding for memory #%d", v.ID)}
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO memory_embeddings(memory_id, model, dims, vector)
				 SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM memories WHERE id = ?)`,
				v.ID, model, len(v.Values), EncodeVector(v.Values), v.ID); err != nil {
				return fmt.Errorf("store embedding of #%d: %w", v.ID, err)
			}
		}
		return nil
	})
}

// DeleteEmbeddings removes the embeddings of model (all models if empty).
func (s *Store) DeleteEmbeddings(ctx context.Context, model string) (int, error) {
	q, args := "DELETE FROM memory_embeddings", []any{}
	if model != "" {
		q, args = q+" WHERE model = ?", []any{model}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("delete embeddings: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// VectorFilter restricts Vectors like SearchQuery restricts Search.
type VectorFilter struct {
	Namespace       string
	Kinds           []Kind
	Tags            []string
	IncludeArchived bool
}

// Vectors returns the embeddings of model for the memories matching f.
func (s *Store) Vectors(ctx context.Context, model string, f VectorFilter) ([]Vector, error) {
	ns := ""
	if f.Namespace != "" {
		var err error
		if ns, err = NormalizeNamespace(f.Namespace); err != nil {
			return nil, err
		}
	}
	filter, fargs := filterSQL(ns, f.Kinds, f.Tags, f.IncludeArchived, 0)
	q := `SELECT e.memory_id, e.vector FROM memory_embeddings e JOIN memories m ON m.id = e.memory_id
		WHERE e.model = ?` + filter + ` ORDER BY e.memory_id`
	rows, err := s.db.QueryContext(ctx, q, append([]any{model}, fargs...)...)
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Vector{}
	for rows.Next() {
		var v Vector
		var blob []byte
		if err := rows.Scan(&v.ID, &blob); err != nil {
			return nil, fmt.Errorf("load embeddings: %w", err)
		}
		if v.Values, err = DecodeVector(blob); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}
	return out, nil
}

// GetMany returns the memories with the given ids (missing ids are absent).
func (s *Store) GetMany(ctx context.Context, ids []int64) (map[int64]Memory, error) {
	out := map[int64]Memory{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i], args[i] = "?", id
	}
	ms, err := queryMemories(ctx, s.db, "SELECT "+memoryColumns+" FROM memories WHERE id IN ("+strings.Join(ph, ", ")+")", args...)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		out[m.ID] = m
	}
	return out, nil
}

// HasSearchTerms reports whether a free-text query contains at least one
// searchable word (Search returns recent memories otherwise).
func HasSearchTerms(q string) bool { return len(queryTerms(q, 1)) > 0 }

// FirstWords is the snippet used when no FTS excerpt is available.
func FirstWords(s string) string { return firstWords(s, snippetWords) }

// ModelCount is the number of embeddings stored for a model.
type ModelCount struct {
	Model string `json:"model"`
	Count int    `json:"count"`
}

func (s *Store) embeddingCounts(ctx context.Context) ([]ModelCount, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT model, count(*) FROM memory_embeddings GROUP BY model ORDER BY model")
	if err != nil {
		return nil, fmt.Errorf("count embeddings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ModelCount
	for rows.Next() {
		var c ModelCount
		if err := rows.Scan(&c.Model, &c.Count); err != nil {
			return nil, fmt.Errorf("count embeddings: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
