package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// CountArchivedBefore counts memories archived strictly before t.
func (s *Store) CountArchivedBefore(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM memories WHERE archived_at IS NOT NULL AND archived_at < ?", FormatTime(t)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count archived: %w", err)
	}
	return n, nil
}

// Purge physically deletes memories archived strictly before t, with their
// history (ON DELETE CASCADE). It is the only hard delete and is not
// reachable from MCP tools.
func (s *Store) Purge(ctx context.Context, t time.Time) (int, error) {
	var n int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"DELETE FROM memories WHERE archived_at IS NOT NULL AND archived_at < ?", FormatTime(t))
		if err != nil {
			return fmt.Errorf("purge: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

// Count is one line of statistics.
type Count struct {
	Key      string `json:"key"`
	Active   int    `json:"active"`
	Archived int    `json:"archived"`
}

// Stats summarises the database.
type Stats struct {
	Path          string       `json:"path"`
	FileBytes     int64        `json:"file_bytes"` // database + WAL
	SchemaVersion int          `json:"schema_version"`
	Active        int          `json:"active"`
	Archived      int          `json:"archived"`
	ByNamespace   []Count      `json:"by_namespace"`
	ByKind        []Count      `json:"by_kind"`
	BySource      []Count      `json:"by_source"`
	Embeddings    []ModelCount `json:"embeddings,omitempty"` // semantic search only
}

// Stats counts memories by namespace, kind and source.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Path: s.path}
	var err error
	if st.SchemaVersion, err = s.SchemaVersion(ctx); err != nil {
		return Stats{}, err
	}
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			st.FileBytes += fi.Size()
		}
	}
	// Grouping columns come from this fixed list.
	for _, g := range []struct {
		col string
		dst *[]Count
	}{{"namespace", &st.ByNamespace}, {"kind", &st.ByKind}, {"source", &st.BySource}} {
		if *g.dst, err = s.countBy(ctx, g.col); err != nil {
			return Stats{}, err
		}
	}
	if st.Embeddings, err = s.embeddingCounts(ctx); err != nil {
		return Stats{}, err
	}
	for _, c := range st.ByNamespace {
		st.Active += c.Active
		st.Archived += c.Archived
	}
	return st, nil
}

func (s *Store) countBy(ctx context.Context, col string) ([]Count, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+col+`,
			count(*) FILTER (WHERE archived_at IS NULL),
			count(*) FILTER (WHERE archived_at IS NOT NULL)
		FROM memories GROUP BY `+col+` ORDER BY count(*) DESC, `+col)
	if err != nil {
		return nil, fmt.Errorf("stats by %s: %w", col, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Count{}
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Key, &c.Active, &c.Archived); err != nil {
			return nil, fmt.Errorf("stats by %s: %w", col, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats by %s: %w", col, err)
	}
	return out, nil
}

// Count returns the total number of memories, archived included.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM memories").Scan(&n); err != nil {
		return 0, fmt.Errorf("count memories: %w", err)
	}
	return n, nil
}
